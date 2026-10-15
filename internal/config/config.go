// Package config reads the standalone CLI's JSON configuration.
// Adapted from cida-go; GUI settings and prompt overrides have been removed.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Format string

const (
	ChatCompletions   Format = "chat-completions"
	Responses         Format = "responses"
	AnthropicMessages Format = "anthropic-messages"
)

type Auth string

const (
	AuthBearer  Auth = "bearer"
	AuthXAPIKey Auth = "x-api-key"
	AuthAPIKey  Auth = "api-key"
	AuthNone    Auth = "none"
)

// Built-in policies are fixed; custom policies belong in actions.
// Original Cida policies: Xuanwo, Apache-2.0 (licenses/Cida.txt).
const (
	DefaultTranslationPrompt = "Translate the user-provided text into the target language specified by the application. Preserve meaning, tone, terminology, and formatting. Return only the translated text."
	DefaultImprovementPrompt = "You are a writing assistant. Improve the user-provided text for clarity, grammar, and natural tone. Keep the original language, meaning, and formatting. Prefer precise technical wording. Return only the improved text."
	Translate                = "translate"
	Improve                  = "improve"
)

type Config struct {
	Endpoint        string            `json:"endpoint"`
	Format          Format            `json:"format"`
	Model           string            `json:"model"`
	APIKey          string            `json:"api_key,omitempty"`
	Auth            Auth              `json:"auth,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            map[string]any    `json:"body,omitempty"`
	MyLanguage      string            `json:"my_language,omitempty"`
	ForeignLanguage string            `json:"foreign_language,omitempty"`
	Actions         []CustomAction    `json:"actions,omitempty"`
}

type CustomAction struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

type Action struct {
	ID     string
	Name   string
	Prompt string
}

// Path deliberately uses ~/.config on every platform, including macOS.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cida", "config.json"), nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// LoadForPrompt reads the prompt's languages and actions without requiring
// service settings or credentials. The schema and custom actions remain strict.
func LoadForPrompt(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := decode(data)
	if err != nil {
		return nil, err
	}
	if err := c.validateActions(); err != nil {
		return nil, err
	}
	return c, nil
}

// Parse accepts exactly one JSON object. Unknown fields, including removed
// GUI settings and built-in prompt overrides, are errors.
func Parse(data []byte) (*Config, error) {
	c, err := decode(data)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func decode(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var c *Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if c == nil {
		return nil, errors.New("configuration must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid JSON: expected exactly one configuration object")
	}
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Model = strings.TrimSpace(c.Model)
	return c, nil
}

func (c *Config) Validate() error {
	u, err := url.Parse(c.Endpoint)
	switch {
	case c.Endpoint == "":
		return errors.New("endpoint is missing")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return errors.New("endpoint must be a full http(s) URL")
	case u.User != nil:
		return errors.New("endpoint must not contain URL credentials; use api_key or headers")
	}
	switch c.Format {
	case ChatCompletions, Responses, AnthropicMessages:
	default:
		return errors.New("format must be chat-completions, responses or anthropic-messages")
	}
	if c.Model == "" {
		return errors.New("model is missing")
	}
	switch c.Auth {
	case "", AuthBearer, AuthXAPIKey, AuthAPIKey, AuthNone:
	default:
		return errors.New("auth must be bearer, x-api-key, api-key or none")
	}
	if c.RequiresAPIKey() && strings.TrimSpace(c.APIKey) == "" {
		return errors.New("api_key is missing")
	}
	return c.validateActions()
}

func (c *Config) validateActions() error {
	seen := []string{Translate, Improve}
	for i, a := range c.Actions {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return fmt.Errorf("actions[%d] has no name", i)
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return fmt.Errorf("action %q has no prompt", name)
		}
		for _, previous := range seen {
			if strings.EqualFold(name, previous) {
				return fmt.Errorf("action %q is defined twice or shadows a built-in action", name)
			}
		}
		seen = append(seen, name)
	}
	return nil
}

func (c *Config) ResolvedAuth() Auth {
	if c.Auth != "" {
		return c.Auth
	}
	if c.Format == AnthropicMessages {
		return AuthXAPIKey
	}
	return AuthBearer
}

func (c *Config) RequiresAPIKey() bool {
	if c.ResolvedAuth() == AuthNone {
		return false
	}
	u, err := url.Parse(c.Endpoint)
	if err == nil {
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return false
		}
	}
	return true
}

func (c *Config) Languages() (my, foreign string) {
	my, foreign = strings.TrimSpace(c.MyLanguage), strings.TrimSpace(c.ForeignLanguage)
	if my == "" {
		my = "简体中文"
	}
	if foreign == "" {
		foreign = "English"
	}
	return my, foreign
}

func (c *Config) ActionList() []Action {
	list := []Action{
		{ID: Translate, Name: Translate, Prompt: DefaultTranslationPrompt},
		{ID: Improve, Name: Improve, Prompt: DefaultImprovementPrompt},
	}
	for _, a := range c.Actions {
		name := strings.TrimSpace(a.Name)
		list = append(list, Action{ID: name, Name: name, Prompt: strings.TrimSpace(a.Prompt)})
	}
	return list
}

func (c *Config) FindAction(name string) (Action, bool) {
	for _, a := range c.ActionList() {
		if strings.EqualFold(a.ID, name) {
			return a, true
		}
	}
	return Action{}, false
}
