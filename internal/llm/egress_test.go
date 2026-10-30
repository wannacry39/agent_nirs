package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

func protectedModel(t *testing.T, s *httptest.Server, secrets []string) *Client {
	t.Helper()
	g, e := egress.New(egress.Config{Version: 1, Targets: []egress.Target{{ID: "model", Destination: egress.Destination{Kind: "llm", Address: s.URL + "/chat/completions"}, Rules: []egress.Rule{{Operation: egress.ModelOperation()}}}}}, egress.Options{Secrets: secrets})
	if e != nil {
		t.Fatal(e)
	}
	return &Client{BaseURL: s.URL, HTTP: s.Client(), Guard: g}
}
func TestEgressProtectsActualModelBody(t *testing.T) {
	var count atomic.Int32
	var received []byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		received, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"готово"}}],"usage":{"total_tokens":1}}`)
	}))
	defer s.Close()
	secret := "DEMO_ONLY_SECRET_001"
	c := protectedModel(t, s, []string{secret})
	schema := map[string]any{"type": "object", "properties": map[string]any{"password": map[string]any{"type": "string", "description": "Пароль пользователя"}}}
	spec := []ToolSpec{{Type: "function", Function: FunctionSpec{Name: "demo", Parameters: schema}}}
	msgs := []Message{{Role: "system", Content: "постоянный промпт"}, {Role: "user", Content: "обычный запрос"}}
	if _, e := c.Complete(context.Background(), msgs, spec); e != nil {
		t.Fatal(e)
	}
	if count.Load() != 1 || !bytes.Contains(received, []byte(`"password"`)) {
		t.Fatal("schema was blocked or rewritten")
	}
	tests := []struct {
		name     string
		messages []Message
		tools    []ToolSpec
	}{
		{"context", []Message{{Role: "user", Content: secret}}, nil},
		{"description", msgs, []ToolSpec{{Type: "function", Function: FunctionSpec{Name: "demo", Description: secret, Parameters: schema}}}},
		{"arguments", []Message{{Role: "assistant", ToolCalls: []ToolCall{{Function: FunctionCall{Name: "demo", Arguments: `{"password":"no-known-prefix"}`}}}}}, nil},
		{"email", []Message{{Role: "user", Content: "user@example.invalid"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := count.Load()
			_, e := c.CompleteAnswer(context.Background(), tt.messages, tt.tools)
			if !errors.Is(e, egress.ErrDenied) {
				t.Fatal(e)
			}
			if count.Load() != before {
				t.Fatal("forbidden body reached provider")
			}
			if strings.Contains(e.Error(), secret) {
				t.Fatal("secret in error")
			}
		})
	}
	if msgs[0].Content != "постоянный промпт" || schema["properties"].(map[string]any)["password"].(map[string]any)["type"] != "string" {
		t.Fatal("input mutated")
	}
}
func TestEgressModelCredentialsRedirectAndRemoteErrors(t *testing.T) {
	var stolen atomic.Int32
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1) }))
	defer thief.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, thief.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	c := protectedModel(t, s, nil)
	c.APIKey = "SYNTHETIC_AUTH_KEY_001"
	_, e := c.Complete(context.Background(), []Message{{Role: "user", Content: "public"}}, nil)
	if e == nil || stolen.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	_, e = c.Complete(context.Background(), []Message{{Role: "user", Content: c.APIKey}}, nil)
	if !errors.Is(e, egress.ErrDenied) || strings.Contains(e.Error(), c.APIKey) {
		t.Fatal("credential leaked")
	}
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+c.APIKey {
			t.Error("authorization missing")
		}
		http.Error(w, c.APIKey, 400)
	})
	_, e = c.Complete(context.Background(), []Message{{Role: "user", Content: "public"}}, nil)
	if e == nil || strings.Contains(e.Error(), c.APIKey) {
		t.Fatal("remote error leaked credential")
	}
}
func TestEgressModelRetryRechecksAndSuppressesBodies(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.Header().Set("Retry-After", "1")
		if n == 1 {
			http.Error(w, "DEMO_ONLY_SECRET_001", 429)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer s.Close()
	c := protectedModel(t, s, nil)
	c.Retries = 1
	var log bytes.Buffer
	c.Log = &log
	if _, e := c.Complete(context.Background(), []Message{{Content: "public"}}, nil); e != nil {
		t.Fatal(e)
	}
	if count.Load() != 2 || strings.Contains(log.String(), "DEMO_ONLY_SECRET_001") {
		t.Fatal("retry/log failure")
	}
}
