package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/erning/cida-cli/internal/config"
	"github.com/erning/cida-cli/internal/testutil"
)

func testConfig(url string, format config.Format) *config.Config {
	return &config.Config{Endpoint: url, Format: format, Model: "m", APIKey: "sk-secret-key"}
}

func translate(cfg *config.Config) config.Action {
	a, _ := cfg.FindAction(config.Translate)
	return a
}

// serve answers every request with contentType and body, and keeps the
// last request's headers and body.
func serve(t *testing.T, status int, contentType, body string) (*testutil.Server, *http.Request, *map[string]any) {
	t.Helper()
	var got http.Request
	sent := map[string]any{}
	srv := testutil.NewServer(t, DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &sent)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &sent
}

func collect(t *testing.T, cfg *config.Config) (string, error) {
	t.Helper()
	var b strings.Builder
	err := Run(context.Background(), cfg, translate(cfg), "hello", func(s string) { b.WriteString(s) })
	return b.String(), err
}

func TestChatCompletionsStream(t *testing.T) {
	srv, req, body := serve(t, 200, "text/event-stream", strings.Join([]string{
		`: keep-alive`,
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	text, err := collect(t, testConfig(srv.URL, config.ChatCompletions))
	if err != nil || text != "你好" {
		t.Fatalf("text %q, err %v", text, err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-secret-key" {
		t.Errorf("authorization %q", req.Header.Get("Authorization"))
	}
	msgs := (*body)["messages"].([]any)
	system := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, `"language_behavior":"translate_between"`) ||
		!strings.Contains(system, `"my_language":"简体中文"`) ||
		!strings.HasPrefix(system, config.DefaultTranslationPrompt) {
		t.Errorf("system message:\n%s", system)
	}
}

func TestResponsesStream(t *testing.T) {
	srv, _, body := serve(t, 200, "text/event-stream", strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n"))
	text, err := collect(t, testConfig(srv.URL, config.Responses))
	if err != nil || text != "Hi" {
		t.Fatalf("text %q, err %v", text, err)
	}
	if (*body)["store"] != false || (*body)["instructions"] == nil {
		t.Errorf("body %v", *body)
	}
}

func TestAnthropicStream(t *testing.T) {
	srv, req, body := serve(t, 200, "text/event-stream; charset=utf-8", strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hm"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hey"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	cfg := testConfig(srv.URL, config.AnthropicMessages)
	cfg.Body = map[string]any{"max_tokens": 100.0, "stream": nil, "thinking": map[string]any{"type": "disabled"}}
	text, err := collect(t, cfg)
	if err != nil || text != "Hey" {
		t.Fatalf("text %q, err %v", text, err)
	}
	if req.Header.Get("x-api-key") != "sk-secret-key" || req.Header.Get("anthropic-version") == "" {
		t.Errorf("headers %v", req.Header)
	}
	if _, ok := (*body)["stream"]; ok || (*body)["max_tokens"] != 100.0 || (*body)["thinking"] == nil {
		t.Errorf("body not merged: %v", *body)
	}
}

func TestCompleteJSONReply(t *testing.T) {
	srv, _, _ := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"whole"}}]}`)
	text, err := collect(t, testConfig(srv.URL, config.ChatCompletions))
	if err != nil || text != "whole" {
		t.Fatalf("text %q, err %v", text, err)
	}
}

func TestHTTPErrorIsRedacted(t *testing.T) {
	srv, _, _ := serve(t, 401, "application/json", `{"error":{"message":"bad key sk-secret-key"}}`)
	_, err := collect(t, testConfig(srv.URL, config.ChatCompletions))
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(he.Body, "sk-secret-key") || !strings.Contains(he.ProviderMessage, "bad key") {
		t.Errorf("body %q, message %q", he.Body, he.ProviderMessage)
	}
	if Reason(err) != "the provider rejected the API key" {
		t.Errorf("reason %q", Reason(err))
	}
}

func TestWrongFormat(t *testing.T) {
	srv, _, _ := serve(t, 200, "text/event-stream", "data: {\"type\":\"message_start\"}\n\n")
	_, err := collect(t, testConfig(srv.URL, config.ChatCompletions))
	if !errors.As(err, new(*UnexpectedResponseError)) {
		t.Fatalf("err %v", err)
	}
}

func TestStreamError(t *testing.T) {
	srv, _, _ := serve(t, 200, "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")
	_, err := collect(t, testConfig(srv.URL, config.AnthropicMessages))
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Message != "overloaded" {
		t.Fatalf("err %v", err)
	}
}

func TestCustomActionFollowsPolicy(t *testing.T) {
	cfg := testConfig("https://a.b/", config.ChatCompletions)
	p := BuildPrompt(cfg, config.Action{ID: "Concise", Name: "Concise", Prompt: "Shorten it."}, "x")
	if !strings.Contains(p.System, `"language_behavior":"follow_policy","operation":"Concise"`) ||
		strings.Contains(p.System, "my_language\":") {
		t.Errorf("system:\n%s", p.System)
	}
}

func TestRedactor(t *testing.T) {
	r := Redactor{Secret: "a/b+c"}
	if got := r.Redact(`k=a/b+c j="a\/b+c" q=a%2Fb%2Bc`); got != `k=•••• j="••••" q=••••` {
		t.Errorf("redacted %q", got)
	}
	if got := (Redactor{Secret: "ab"}).Redact("ab"); got != "ab" {
		t.Errorf("short secret redacted: %q", got)
	}
}
