package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `{"endpoint":"https://api.example.com/v1/chat/completions","format":"chat-completions","model":"m","api_key":"sk-test"}`

func TestConfigDefaultsAndActions(t *testing.T) {
	c, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + `,"actions":[{"name":" Concise ","prompt":" Shorten it. "}]}`))
	if err != nil {
		t.Fatal(err)
	}
	my, foreign := c.Languages()
	if my != "简体中文" || foreign != "English" || c.ResolvedAuth() != AuthBearer {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, name := range []string{"TRANSLATE", "Improve", "concise"} {
		if _, ok := c.FindAction(name); !ok {
			t.Errorf("action %q not found", name)
		}
	}
	if a, _ := c.FindAction("concise"); a.ID != "Concise" || a.Prompt != "Shorten it." {
		t.Fatalf("custom action: %+v", a)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"comment", `{// comment\n}`, "invalid JSON"},
		{"trailing comma", `{"model":"m",}`, "invalid JSON"},
		{"null", `null`, "JSON object"},
		{"array", `[]`, "invalid JSON"},
		{"two documents", minimal + `{}`, "exactly one"},
		{"trailing garbage", minimal + `garbage`, "exactly one"},
		{"old translation override", strings.TrimSuffix(minimal, "}") + `,"translation_prompt":"x"}`, "unknown field"},
		{"old improvement override", strings.TrimSuffix(minimal, "}") + `,"improvement_prompt":"x"}`, "unknown field"},
		{"GUI setting", strings.TrimSuffix(minimal, "}") + `,"font":"x"}`, "unknown field"},
		{"bad endpoint", strings.Replace(minimal, "https://api.example.com/v1/chat/completions", "example.com", 1), "full http(s) URL"},
		{"bad format", strings.Replace(minimal, "chat-completions", "chat", 1), "format must be"},
		{"missing model", strings.Replace(minimal, `"model":"m"`, `"model":" "`, 1), "model is missing"},
		{"missing key", strings.Replace(minimal, `"api_key":"sk-test"`, `"api_key":""`, 1), "api_key is missing"},
		{"built-in shadow", strings.TrimSuffix(minimal, "}") + `,"actions":[{"name":"Translate","prompt":"p"}]}`, "shadows a built-in"},
		{"duplicate action", strings.TrimSuffix(minimal, "}") + `,"actions":[{"name":"Short","prompt":"p"},{"name":"short","prompt":"q"}]}`, "defined twice"},
		{"empty prompt", strings.TrimSuffix(minimal, "}") + `,"actions":[{"name":"short","prompt":" "}]}`, "has no prompt"},
		{"custom typo", strings.TrimSuffix(minimal, "}") + `,"actions":[{"name":"short","promt":"p"}]}`, "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAuthAndLocalServices(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:8080/v1", "http://127.0.0.1:8080/v1", "http://[::1]:8080/v1"} {
		data := `{"endpoint":"` + endpoint + `","format":"anthropic-messages","model":"local"}`
		c, err := Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if c.RequiresAPIKey() || c.ResolvedAuth() != AuthXAPIKey {
			t.Fatalf("local config: %+v", c)
		}
	}
	_, err := Parse([]byte(`{"endpoint":"https://api.example.com/v1","format":"responses","model":"m","auth":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".config", "cida", "config.json")
	if got, err := Path(); got != want || err != nil {
		t.Fatalf("Path() = %q, %v; want %q", got, err, want)
	}
}

func TestBodyPreservesLargeNumbers(t *testing.T) {
	c, err := Parse([]byte(strings.TrimSuffix(minimal, "}") + `,"body":{"seed":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c.Body)
	if err != nil || string(data) != `{"seed":9007199254740993}` {
		t.Fatalf("body %s, err %v", data, err)
	}
}
