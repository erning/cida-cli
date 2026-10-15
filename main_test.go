package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erning/cida-cli/internal/llm"
	"github.com/erning/cida-cli/internal/testutil"
)

func cliConfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	doc := map[string]any{
		"endpoint": endpoint, "format": "chat-completions", "model": "test-model", "api_key": "sk-cli-secret",
		"my_language": "粤语", "foreign_language": "日本語",
		"actions": []map[string]string{{"name": "concise", "prompt": "Shorten it."}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIInputAndAction(t *testing.T) {
	for _, tc := range []struct {
		name, action, input, wantInput, behavior string
		text                                     []string
	}{
		{"default translate", "", "ignored pipe", "你好 world", "translate_between", []string{"你好", "world"}},
		{"stdin improve", "IMPROVE", "First line.\n第二行。\n", "First line.\n第二行。\n", "preserve_source", nil},
		{"custom", "ConCiSe", "", "Long text", "follow_policy", []string{"Long text"}},
		{"dash text", "translate", "", "--help", "translate_between", []string{"--", "--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent map[string]any
			srv := testutil.NewServer(t, llm.DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer sk-cli-secret" {
					t.Error("wrong request method or authentication")
				}
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"result\"}}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			args := []string{"--config", cliConfig(t, srv.URL)}
			if tc.action != "" {
				args = append(args, "--action", tc.action)
			}
			args = append(args, tc.text...)
			var out, diagnostics bytes.Buffer
			code := run(context.Background(), args, strings.NewReader(tc.input), false, &out, &diagnostics)
			if code != 0 || out.String() != "result\n" || diagnostics.Len() != 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), diagnostics.String())
			}
			messages := sent["messages"].([]any)
			if got := messages[1].(map[string]any)["content"]; got != tc.wantInput {
				t.Fatalf("source = %q, want %q", got, tc.wantInput)
			}
			system := messages[0].(map[string]any)["content"].(string)
			if !strings.Contains(system, `"language_behavior":"`+tc.behavior+`"`) {
				t.Fatalf("wrong action: %s", system)
			}
			if tc.behavior == "translate_between" && (!strings.Contains(system, `"my_language":"粤语"`) || !strings.Contains(system, `"foreign_language":"日本語"`)) {
				t.Fatal("configured languages not passed to the model")
			}
		})
	}
}

func TestCLILocalErrorsNeverCallProvider(t *testing.T) {
	srv := testutil.NewServer(t, llm.DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected provider request") }))
	defer srv.Close()
	path := cliConfig(t, srv.URL)
	for _, tc := range []struct {
		name        string
		args        []string
		input       string
		interactive bool
		want        string
	}{
		{"empty", []string{"--config", path}, " \n", false, "empty"},
		{"no input", nil, "", true, "provide text"},
		{"unknown action", []string{"--config", path, "-a", "missing", "text"}, "", false, "available: translate, improve, concise"},
		{"missing flag value", []string{"--action"}, "", false, "flag needs an argument"},
		{"unknown flag", []string{"--missing"}, "", false, "flag provided but not defined"},
		{"missing configuration", []string{"--config", filepath.Join(t.TempDir(), "absent.json"), "text"}, "", false, "config.example.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := run(context.Background(), tc.args, strings.NewReader(tc.input), tc.interactive, &out, &diagnostics)
			if code != 2 || out.Len() != 0 || !strings.Contains(diagnostics.String(), tc.want) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), diagnostics.String())
			}
		})
	}
}

func TestCLIHelpAndVersionWithoutConfiguration(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		var out, diagnostics bytes.Buffer
		code := run(context.Background(), args, nil, true, &out, &diagnostics)
		if code != 0 || out.Len() == 0 || diagnostics.Len() != 0 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), diagnostics.String())
		}
	}
}

func TestCLIFailedRequest(t *testing.T) {
	srv := testutil.NewServer(t, llm.DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad sk-cli-secret"}}`)
	}))
	defer srv.Close()
	var out, diagnostics bytes.Buffer
	code := run(context.Background(), []string{"--config", cliConfig(t, srv.URL), "text"}, nil, false, &out, &diagnostics)
	if code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "HTTP 401") || strings.Contains(diagnostics.String(), "sk-cli-secret") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), diagnostics.String())
	}
}

func TestCLICancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := testutil.NewServer(t, llm.DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	defer cancel()
	var out, diagnostics bytes.Buffer
	code := run(ctx, []string{"--config", cliConfig(t, srv.URL), "text"}, nil, false, &out, &diagnostics)
	if code != 130 || diagnostics.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, diagnostics.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestCLIOutputFailure(t *testing.T) {
	srv := testutil.NewServer(t, llm.DefaultClient.HTTP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"result"}}]}`)
	}))
	defer srv.Close()
	var diagnostics bytes.Buffer
	code := run(context.Background(), []string{"--config", cliConfig(t, srv.URL), "text"}, nil, false, brokenWriter{}, &diagnostics)
	if code != 1 || !strings.Contains(diagnostics.String(), "output closed") {
		t.Fatalf("exit %d, stderr %q", code, diagnostics.String())
	}
}
