// Adapted from cida-go; modified for the standalone cida-cli.
// Original Cida policies and protocols: Xuanwo, Apache-2.0 (licenses/Cida.txt).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/erning/cida-cli/internal/config"
)

const (
	anthropicVersion = "2023-06-01"
	// Anthropic Messages requires max_tokens; body can change it.
	anthropicMaxTokens = 8192
)

// Prepared is a request ready to send.
type Prepared struct {
	URL      string
	Format   config.Format
	Body     map[string]any
	header   http.Header
	redactor Redactor
}

// Prepare builds the request cfg sends for prompt.
func Prepare(cfg *config.Config, prompt Prompt) *Prepared {
	body := mergeJSON(baseBody(cfg, prompt), cfg.Body)
	p := &Prepared{
		URL:      strings.TrimSpace(cfg.Endpoint),
		Format:   cfg.Format,
		Body:     body,
		header:   http.Header{},
		redactor: Redactor{Secret: cfg.APIKey},
	}
	p.header.Set("Content-Type", "application/json")
	p.header.Set("Accept", "text/event-stream")
	if cfg.APIKey != "" {
		switch cfg.ResolvedAuth() {
		case config.AuthBearer:
			p.set("Authorization", "Bearer "+cfg.APIKey)
		case config.AuthXAPIKey:
			p.set("x-api-key", cfg.APIKey)
		case config.AuthAPIKey:
			p.set("api-key", cfg.APIKey)
		}
	}
	if cfg.Format == config.AnthropicMessages {
		p.set("anthropic-version", anthropicVersion)
	}
	for name, value := range cfg.Headers {
		p.set(name, value)
	}
	return p
}

func (p *Prepared) set(name, value string) {
	p.header.Set(name, value)
}

// BodyJSON is the request body, indented when indent is set.
func (p *Prepared) BodyJSON(indent bool) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(p.Body)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (p *Prepared) httpRequest(ctx context.Context) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(p.BodyJSON(false)))
	if err != nil {
		return nil, err
	}
	req.Header = p.header.Clone()
	return req, nil
}

func baseBody(cfg *config.Config, prompt Prompt) map[string]any {
	user := map[string]any{"role": "user", "content": prompt.User}
	switch cfg.Format {
	case config.Responses:
		// Cida keeps no record of requests, so the service is asked not to either.
		return map[string]any{
			"model":        cfg.Model,
			"stream":       true,
			"store":        false,
			"instructions": prompt.System,
			"input":        []any{user},
		}
	case config.AnthropicMessages:
		return map[string]any{
			"model":      cfg.Model,
			"max_tokens": anthropicMaxTokens,
			"stream":     true,
			"system":     prompt.System,
			"messages":   []any{user},
		}
	default:
		return map[string]any{
			"model":  cfg.Model,
			"stream": true,
			"messages": []any{
				map[string]any{"role": "system", "content": prompt.System},
				user,
			},
		}
	}
}

// mergeJSON lays override over base: objects merge member by member and
// recursively, a null member removes the key, anything else replaces.
func mergeJSON(base, override map[string]any) map[string]any {
	for k, v := range override {
		if v == nil {
			delete(base, k)
			continue
		}
		if vo, ok := v.(map[string]any); ok {
			if bo, ok := base[k].(map[string]any); ok {
				base[k] = mergeJSON(bo, vo)
				continue
			}
			base[k] = mergeJSON(map[string]any{}, vo)
			continue
		}
		base[k] = v
	}
	return base
}
