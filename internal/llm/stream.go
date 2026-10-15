// Adapted from cida-go; modified for the standalone cida-cli.
// Original Cida policies and protocols: Xuanwo, Apache-2.0 (licenses/Cida.txt).
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/erning/cida-cli/internal/config"
)

// Client sends prepared requests.
type Client struct {
	HTTP *http.Client
}

// DefaultClient waits up to five minutes for a reply to start; a streamed
// reply then takes as long as it takes.
var DefaultClient = &Client{HTTP: &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Minute,
		TLSHandshakeTimeout:   30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}}

// Run builds the request for action and text and streams the reply.
func Run(ctx context.Context, cfg *config.Config, action config.Action, text string, onText func(string)) error {
	return DefaultClient.Send(ctx, Prepare(cfg, BuildPrompt(cfg, action, text)), onText)
}

// Send posts p and hands every piece of reply text to onText, whether the
// service streams server-sent events or answers with one JSON document.
func (c *Client) Send(ctx context.Context, p *Prepared, onText func(string)) error {
	req, err := p.httpRequest(ctx)
	if err != nil {
		return &TransportError{Err: err}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &TransportError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		text := p.redactor.Redact(string(data))
		return &HTTPError{Status: resp.StatusCode, ProviderMessage: providerMessage(parseJSON(text)), Body: text}
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		err := receiveEvents(ctx, resp.Body, p, onText)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	data, readErr := io.ReadAll(resp.Body)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return &TransportError{Err: readErr}
	}
	text := string(data)
	doc := parseJSON(text)
	if doc == nil {
		return &UnexpectedResponseError{Detail: "The reply is neither JSON nor an event stream."}
	}
	if obj, _ := doc.(map[string]any); obj != nil && obj["error"] != nil {
		if msg := providerMessage(doc); msg != "" {
			return &ProviderError{Message: p.redactor.Redact(msg)}
		}
	}
	if message := completionFailure(p.Format, doc); message != "" {
		return &ProviderError{Message: p.redactor.Redact(message)}
	}
	reply, ok := completeReply(p.Format, doc)
	if !ok {
		return &UnexpectedResponseError{Detail: fmt.Sprintf("The JSON reply holds no %s reply.", p.Format)}
	}
	if reply == "" {
		return ErrEmptyResult
	}
	onText(reply)
	return nil
}

func receiveEvents(ctx context.Context, body io.Reader, p *Prepared, onText func(string)) error {
	var parser sseParser
	recognized := false
	sawText := false

	// handle returns false once the reply is complete.
	handle := func(ev sseEvent) (bool, error) {
		kind, text := streamEvent(p.Format, ev)
		switch kind {
		case evText:
			recognized, sawText = true, true
			onText(text)
		case evRecognized:
			recognized = true
		case evDone:
			return false, nil
		case evFailure:
			return false, &ProviderError{Message: p.redactor.Redact(text)}
		}
		return true, nil
	}

	r := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := r.ReadString('\n')
		if line != "" || err == nil {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if ev, ok := parser.consume(line); ok {
				more, herr := handle(ev)
				if herr != nil {
					return herr
				}
				if !more {
					return finish(sawText)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &TransportError{Err: err}
		}
	}
	if ev, ok := parser.finish(); ok {
		more, err := handle(ev)
		if err != nil {
			return err
		}
		if !more {
			return finish(sawText)
		}
	}
	if !recognized {
		return &UnexpectedResponseError{Detail: fmt.Sprintf("The event stream holds no %s events.", p.Format)}
	}
	// An EOF without a protocol completion marker is an interrupted stream,
	// even when some text arrived. Never report a partial translation as success.
	return &TransportError{Err: io.ErrUnexpectedEOF}
}

func finish(sawText bool) error {
	if !sawText {
		return ErrEmptyResult
	}
	return nil
}

func parseJSON(text string) any {
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		return nil
	}
	return v
}

// get walks objects by key.
func get(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// providerMessage is the message a provider put in an error document:
// OpenAI and Anthropic nest it under error.message; others use error,
// message or detail.
func providerMessage(doc any) string {
	for _, keys := range [][]string{{"error", "message"}, {"error"}, {"message"}, {"detail"}} {
		if s := str(get(doc, keys...)); s != "" {
			return s
		}
	}
	return ""
}

// MARK: server-sent events

type sseEvent struct {
	name string
	data string
}

// sseParser assembles events from lines: an empty line ends an event, ":"
// starts a comment, and several data lines join with newlines.
type sseParser struct {
	name string
	data []string
}

func (p *sseParser) consume(line string) (sseEvent, bool) {
	if line == "" {
		return p.dispatch()
	}
	if strings.HasPrefix(line, ":") {
		return sseEvent{}, false
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "data":
		p.data = append(p.data, value)
	case "event":
		p.name = value
	}
	return sseEvent{}, false
}

func (p *sseParser) finish() (sseEvent, bool) { return p.dispatch() }

func (p *sseParser) dispatch() (sseEvent, bool) {
	defer func() { p.name, p.data = "", nil }()
	if len(p.data) == 0 {
		return sseEvent{}, false
	}
	return sseEvent{name: p.name, data: strings.Join(p.data, "\n")}, true
}

type eventKind int

const (
	evUnrecognized eventKind = iota // not of this format: a sign format is wrong
	evText
	evRecognized // of this format, carrying no text: a start, a stop reason, a ping
	evDone
	evFailure
)

func streamEvent(format config.Format, ev sseEvent) (eventKind, string) {
	if format == config.ChatCompletions && ev.data == "[DONE]" {
		return evDone, ""
	}
	payload := parseJSON(ev.data)
	if _, ok := payload.(map[string]any); !ok {
		return evUnrecognized, ""
	}
	typ := str(get(payload, "type"))
	if typ == "" {
		typ = ev.name
	}
	failure := func() (eventKind, string) {
		if msg := providerMessage(payload); msg != "" {
			return evFailure, msg
		}
		return evFailure, ev.data
	}
	switch format {
	case config.ChatCompletions:
		if get(payload, "error") != nil {
			return failure()
		}
		choices, ok := get(payload, "choices").([]any)
		if !ok {
			return evUnrecognized, ""
		}
		if len(choices) > 0 {
			if message := chatFailure(choices[0]); message != "" {
				return evFailure, message
			}
			if text := str(get(choices[0], "delta", "content")); text != "" {
				return evText, text
			}
		}
		return evRecognized, ""
	case config.Responses:
		switch typ {
		case "response.output_text.delta":
			if text := str(get(payload, "delta")); text != "" {
				return evText, text
			}
			return evRecognized, ""
		case "response.completed":
			if message := completionFailure(format, get(payload, "response")); message != "" {
				return evFailure, message
			}
			return evDone, ""
		case "response.incomplete":
			return evFailure, "The provider returned an incomplete response."
		case "response.refusal.delta":
			return evFailure, "The provider refused the request."
		case "response.failed":
			if msg := str(get(payload, "response", "error", "message")); msg != "" {
				return evFailure, msg
			}
			return evFailure, "The provider reported that the response failed."
		case "error":
			return failure()
		}
		if strings.HasPrefix(typ, "response.") {
			return evRecognized, ""
		}
		return evUnrecognized, ""
	case config.AnthropicMessages:
		if typ == "message_delta" {
			if message := stopFailure(str(get(payload, "delta", "stop_reason"))); message != "" {
				return evFailure, message
			}
		}
		switch typ {
		case "content_block_delta":
			if str(get(payload, "delta", "type")) == "text_delta" {
				if text := str(get(payload, "delta", "text")); text != "" {
					return evText, text
				}
			}
			return evRecognized, ""
		case "message_stop":
			return evDone, ""
		case "error":
			return failure()
		case "message_start", "content_block_start", "content_block_stop", "message_delta", "ping":
			return evRecognized, ""
		}
	}
	return evUnrecognized, ""
}

func stopFailure(reason string) string {
	switch reason {
	case "length", "max_tokens", "model_context_window_exceeded":
		return "The output limit was reached; increase the token limit in body."
	case "content_filter", "refusal":
		return "The provider refused or filtered the request."
	}
	return ""
}

func chatFailure(choice any) string {
	if reason := stopFailure(str(get(choice, "finish_reason"))); reason != "" {
		return reason
	}
	if str(get(choice, "delta", "refusal")) != "" || str(get(choice, "message", "refusal")) != "" {
		return "The provider refused the request."
	}
	return ""
}

func completionFailure(format config.Format, doc any) string {
	switch format {
	case config.ChatCompletions:
		choices, _ := get(doc, "choices").([]any)
		if len(choices) > 0 {
			return chatFailure(choices[0])
		}
	case config.AnthropicMessages:
		return stopFailure(str(get(doc, "stop_reason")))
	case config.Responses:
		switch str(get(doc, "status")) {
		case "incomplete", "failed", "cancelled":
			return "The provider did not complete the response."
		}
	}
	return ""
}

// completeReply is the reply in a response that was not streamed, or false
// when the document is not this format's.
func completeReply(format config.Format, doc any) (string, bool) {
	switch format {
	case config.ChatCompletions:
		choices, ok := get(doc, "choices").([]any)
		if !ok {
			return "", false
		}
		if len(choices) == 0 {
			return "", true
		}
		return str(get(choices[0], "message", "content")), true
	case config.Responses:
		if s, ok := get(doc, "output_text").(string); ok {
			return s, true
		}
		output, ok := get(doc, "output").([]any)
		if !ok {
			return "", false
		}
		var b strings.Builder
		for _, item := range output {
			if str(get(item, "type")) != "message" {
				continue
			}
			content, _ := get(item, "content").([]any)
			for _, part := range content {
				if str(get(part, "type")) == "output_text" {
					b.WriteString(str(get(part, "text")))
				}
			}
		}
		return b.String(), true
	case config.AnthropicMessages:
		content, ok := get(doc, "content").([]any)
		if !ok {
			return "", false
		}
		var b strings.Builder
		for _, part := range content {
			if str(get(part, "type")) == "text" {
				b.WriteString(str(get(part, "text")))
			}
		}
		return b.String(), true
	}
	return "", false
}

// MARK: errors

// HTTPError is a reply with a status outside 2xx.
type HTTPError struct {
	Status          int
	ProviderMessage string
	Body            string // redacted
}

func (e *HTTPError) Error() string {
	if e.ProviderMessage != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.ProviderMessage)
	}
	return fmt.Sprintf("the model service answered HTTP %d", e.Status)
}

// ProviderError is an error event in a successful stream, or an error
// document.
type ProviderError struct{ Message string }

func (e *ProviderError) Error() string { return "the provider reported: " + e.Message }

// UnexpectedResponseError is a reply that is not in the configured format.
type UnexpectedResponseError struct{ Detail string }

func (e *UnexpectedResponseError) Error() string {
	return "unrecognized reply: " + e.Detail + " Check format."
}

// TransportError is a failure to reach the service or read its reply.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "cannot reach the model service: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ErrEmptyResult means the service returned no text.
var ErrEmptyResult = errors.New("the model service returned no text")

// Reason is a short cause of err, for people.
func Reason(err error) string {
	var he *HTTPError
	var te *TransportError
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "stopped"
	case errors.As(err, &he):
		switch {
		case he.Status == 401 || he.Status == 403:
			return "the provider rejected the API key"
		case he.Status == 404:
			return "endpoint or model not found"
		case he.Status == 429:
			return "too many requests or quota exhausted"
		case he.Status >= 400 && he.Status < 500:
			return "the provider rejected the request"
		default:
			return "the provider failed"
		}
	case errors.As(err, new(*ProviderError)):
		return "the provider returned an error"
	case errors.As(err, new(*UnexpectedResponseError)):
		return "the reply does not match format"
	case errors.Is(err, ErrEmptyResult):
		return "no text returned"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "the response ended before completion"
	case errors.As(err, &te):
		var ue *url.Error
		if errors.As(te.Err, &ne) && ne.Timeout() {
			return "request timed out"
		}
		if errors.As(te.Err, &ue) && strings.Contains(strings.ToLower(ue.Err.Error()), "certificate") {
			return "secure connection failed"
		}
		return "cannot reach the service"
	}
	return "request failed"
}

// MARK: redaction

// Redactor replaces the API key with •••• in anything Cida prints or
// shows, including a provider's echo of it inside JSON or a URL.
type Redactor struct{ Secret string }

const mask = "••••"

func (r Redactor) Redact(text string) string {
	// Anything shorter cannot be a key, and replacing it would garble text.
	if len([]rune(r.Secret)) < 4 {
		return text
	}
	quoted, _ := json.Marshal(r.Secret)
	jsonEscaped := string(quoted[1 : len(quoted)-1])
	for _, form := range []string{
		r.Secret,
		jsonEscaped,
		strings.ReplaceAll(jsonEscaped, "/", `\/`),
		url.QueryEscape(r.Secret),
	} {
		text = strings.ReplaceAll(text, form, mask)
	}
	return text
}
