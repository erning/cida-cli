package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erning/cida-cli/internal/config"
	"github.com/erning/cida-cli/internal/testutil"
)

func TestStreamMustFinish(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format config.Format
		body   string
	}{
		{"chat", config.ChatCompletions, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"},
		{"responses", config.Responses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"},
		{"anthropic", config.AnthropicMessages, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serve(t, 200, "text/event-stream", tc.body)
			text, err := collect(t, testConfig(srv.URL, tc.format))
			if text != "partial" || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("text %q, err %v", text, err)
			}
		})
	}
}

func TestProviderIncompleteOrRefused(t *testing.T) {
	for _, tc := range []struct {
		name              string
		format            config.Format
		contentType, body string
	}{
		{"chat length", config.ChatCompletions, "text/event-stream", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"},
		{"chat refusal", config.ChatCompletions, "application/json", `{"choices":[{"message":{"refusal":"No"}}]}`},
		{"responses incomplete", config.Responses, "text/event-stream", "data: {\"type\":\"response.incomplete\"}\n\n"},
		{"responses refusal", config.Responses, "text/event-stream", "data: {\"type\":\"response.refusal.delta\",\"delta\":\"No\"}\n\n"},
		{"anthropic length", config.AnthropicMessages, "text/event-stream", "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n"},
		{"responses partial JSON", config.Responses, "application/json", `{"status":"incomplete","output_text":"partial"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serve(t, 200, tc.contentType, tc.body)
			_, err := collect(t, testConfig(srv.URL, tc.format))
			var pe *ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err %v, want provider failure", err)
			}
		})
	}
}

func TestCompleteResponsesAndAnthropic(t *testing.T) {
	for _, tc := range []struct {
		format config.Format
		body   string
	}{
		{config.Responses, `{"status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"你好"}]}]}`},
		{config.AnthropicMessages, `{"content":[{"type":"thinking","thinking":"ignored"},{"type":"text","text":"你好"}],"stop_reason":"end_turn"}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			srv, _, _ := serve(t, 200, "application/json", tc.body)
			text, err := collect(t, testConfig(srv.URL, tc.format))
			if err != nil || text != "你好" {
				t.Fatalf("text %q, err %v", text, err)
			}
		})
	}
}

func TestSSEMultilineCRLFAndLargeChunk(t *testing.T) {
	chunk := strings.Repeat("界", 30_000)
	body := ": keepalive\r\ndata: {\"choices\":\r\ndata: [{\"delta\":{\"content\":\"" + chunk + "\"}}]}\r\n\r\ndata: [DONE]"
	srv, _, _ := serve(t, 200, "text/event-stream", body)
	text, err := collect(t, testConfig(srv.URL, config.ChatCompletions))
	if err != nil || text != chunk {
		t.Fatalf("received %d bytes, err %v", len(text), err)
	}
}

func TestTextArrivesBeforeStreamCompletes(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	client := &Client{HTTP: &http.Client{Transport: testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}, nil
	})}}
	firstText := make(chan string, 1)
	finished := make(chan error, 1)
	cfg := testConfig("http://127.0.0.1/v1", config.ChatCompletions)
	go func() {
		finished <- client.Send(context.Background(), Prepare(cfg, BuildPrompt(cfg, translate(cfg), "hello")), func(s string) { firstText <- s })
	}()
	go func() { io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n") }()
	select {
	case text := <-firstText:
		if text != "first" {
			t.Fatalf("text = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("text was buffered until completion")
	}
	select {
	case err := <-finished:
		t.Fatalf("stream ended early: %v", err)
	default:
	}
	go func() { io.WriteString(writer, "data: [DONE]\n\n"); writer.Close() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not complete")
	}
}

func TestRequestHeadersAndBodyMerge(t *testing.T) {
	cfg := testConfig("http://127.0.0.1/v1", config.ChatCompletions)
	cfg.Auth = config.AuthAPIKey
	cfg.Headers = map[string]string{"X-Project": "test"}
	cfg.Body = map[string]any{"stream": false, "options": map[string]any{"one": true}}
	p := Prepare(cfg, BuildPrompt(cfg, translate(cfg), "text"))
	req, err := p.httpRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("api-key") != cfg.APIKey || req.Header.Get("X-Project") != "test" || p.Body["stream"] != false {
		t.Fatal("headers or overrides missing")
	}
	merged := mergeJSON(map[string]any{"nested": map[string]any{"keep": 1, "remove": 2}, "array": []int{1}}, map[string]any{"nested": map[string]any{"remove": nil, "add": 3}, "array": []int{2}})
	nested := merged["nested"].(map[string]any)
	if _, ok := nested["remove"]; ok || nested["keep"] != 1 || nested["add"] != 3 || merged["array"].([]int)[0] != 2 {
		t.Fatalf("merge = %#v", merged)
	}
}

func TestMalformedEventFailsStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format config.Format
		body   string
	}{
		{"chat broken JSON", config.ChatCompletions, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {broken\n\ndata: [DONE]\n\n"},
		{"chat non-object", config.ChatCompletions, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: 42\n\ndata: [DONE]\n\n"},
		{"chat choices", config.ChatCompletions, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"choices\":\"x\"}\n\ndata: [DONE]\n\n"},
		{"chat content type", config.ChatCompletions, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":5}}]}\n\ndata: [DONE]\n\n"},
		{"responses delta", config.Responses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":[1]}\n\ndata: {\"type\":\"response.completed\"}\n\n"},
		{"anthropic delta", config.AnthropicMessages, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\ndata: {oops\n\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serve(t, 200, "text/event-stream", tc.body)
			text, err := collect(t, testConfig(srv.URL, tc.format))
			if text != "partial" || !errors.As(err, new(*MalformedEventError)) {
				t.Fatalf("text %q, err %v", text, err)
			}
			if Reason(err) != "the reply stream is corrupt" {
				t.Errorf("reason %q", Reason(err))
			}
		})
	}
}

func TestStreamToleratesEmptyAndDoneEvents(t *testing.T) {
	body := "data:\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\ndata: {\"type\":\"response.completed\"}\n\n"
	srv, _, _ := serve(t, 200, "text/event-stream", body)
	text, err := collect(t, testConfig(srv.URL, config.Responses))
	if err != nil || text != "ok" {
		t.Fatalf("text %q, err %v", text, err)
	}
}

func redirectClient(t *testing.T, from string, target string, seen *http.Header) *Client {
	return &Client{HTTP: &http.Client{
		CheckRedirect: SameOriginRedirects,
		Transport: testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == from {
				return &http.Response{StatusCode: 307, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			}
			*seen = r.Header.Clone()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)), Request: r}, nil
		}),
	}}
}

func TestRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, from, target string
		refused            bool
	}{
		{"other host", "https://provider.example/v1", "https://other.example/collect", true},
		{"other port", "https://provider.example/v1", "https://provider.example:8443/v1", true},
		{"downgrade", "https://provider.example/v1", "http://provider.example/v1", true},
		{"upgrade", "http://provider.example/v1", "https://provider.example/v1", true},
		{"same origin", "https://provider.example/v1", "https://provider.example:443/v2", false},
		{"same origin relative", "https://provider.example/v1", "/v2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen http.Header
			cfg := testConfig(tc.from, config.AnthropicMessages)
			err := redirectClient(t, tc.from, tc.target, &seen).Send(context.Background(), Prepare(cfg, BuildPrompt(cfg, translate(cfg), "text")), func(string) {})
			if tc.refused {
				var re *RedirectError
				if !errors.As(err, &re) || seen != nil {
					t.Fatalf("err %v, forwarded headers %v", err, seen)
				}
				if strings.Contains(err.Error(), "collect") || strings.Contains(err.Error(), cfg.APIKey) {
					t.Errorf("error leaks path or key: %v", err)
				}
				return
			}
			if err != nil || seen.Get("x-api-key") != cfg.APIKey {
				t.Fatalf("err %v, headers %v", err, seen)
			}
		})
	}
}

func TestDefaultClientRefusesCrossOriginRedirect(t *testing.T) {
	if DefaultClient.HTTP.CheckRedirect == nil {
		t.Fatal("DefaultClient follows any redirect")
	}
}

func TestHeaderCredentialsAreRedacted(t *testing.T) {
	srv, _, _ := serve(t, 401, "application/json", `{"error":{"message":"bad Bearer sk-header-secret / sk-header-secret"}}`)
	cfg := testConfig(srv.URL, config.ChatCompletions)
	cfg.APIKey = ""
	cfg.Auth = config.AuthNone
	cfg.Headers = map[string]string{"Authorization": "Bearer sk-header-secret", "X-Project": "visible-project"}
	_, err := collect(t, cfg)
	var he *HTTPError
	if !errors.As(err, &he) || strings.Contains(he.Body, "sk-header-secret") || strings.Contains(he.ProviderMessage, "sk-header-secret") {
		t.Fatalf("err %v", err)
	}
	if got := RedactorFor(cfg).Redact("X-Project visible-project Bearer"); got != "X-Project visible-project Bearer" {
		t.Errorf("over-redacted: %q", got)
	}
}
