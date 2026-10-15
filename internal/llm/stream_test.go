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
