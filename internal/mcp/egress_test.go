package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

func rpcRules(allow []egress.Allowance) []egress.Rule {
	r := []egress.Rule{}
	for _, m := range []string{"initialize", "notifications/initialized", "tools/list", "close"} {
		r = append(r, egress.Rule{Operation: egress.ControlOperation(m)})
	}
	return append(r, egress.Rule{Operation: egress.ToolOperation("search_messages"), Allow: allow}, egress.Rule{Operation: egress.ToolOperation("other")})
}
func secureRPC(t *testing.T) (*Client, *Server, *atomic.Int32, *atomic.Int32, *egress.Guard) {
	t.Helper()
	server := NewServer("fixture", "1")
	var requests, handlers atomic.Int32
	ro := true
	for _, name := range []string{"search_messages", "other"} {
		server.AddTool(ServerTool{Info: ToolInfo{Name: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}, Annotations: &ToolAnnotations{ReadOnlyHint: &ro}}, Handler: func(context.Context, map[string]any) (string, error) { handlers.Add(1); return "ok", nil }})
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); server.ServeHTTP(w, r) }))
	t.Cleanup(s.Close)
	g, e := egress.New(egress.Config{Version: 1, Targets: []egress.Target{{ID: "mail", Destination: egress.Destination{Kind: "mcp", Address: s.URL}, Rules: rpcRules([]egress.Allowance{{Category: "email", Path: []any{"query"}}})}}}, egress.Options{Secrets: []string{"DEMO_ONLY_SECRET_001"}, Credentials: []string{"unusual-provider-credential-0001"}})
	if e != nil {
		t.Fatal(e)
	}
	c, e := NewProtectedClient(s.URL, g)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Connect(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c, server, &requests, &handlers, g
}
func TestEgressMCPDirectCanonicalCallsAndRetries(t *testing.T) {
	c, s, requests, handlers, g := secureRPC(t)
	for _, tt := range []struct {
		name  string
		args  map[string]any
		allow bool
	}{
		{"search_messages", map[string]any{"query": "public"}, true},
		{"search_messages", map[string]any{"query": "user@example.invalid"}, true},
		{"search_messages", map[string]any{"query": "public", "text": "user@example.invalid"}, false},
		{"search_messages", map[string]any{"query": "public", "nested.query": "user@example.invalid"}, false},
		{"other", map[string]any{"query": "user@example.invalid"}, false},
		{"search_messages", map[string]any{"query": "unusual-provider-credential-0001"}, false},
	} {
		before := requests.Load()
		hBefore := handlers.Load()
		expected := g.Check(context.Background(), egress.ToolAttempt(c.destination(), tt.name, tt.args)) == nil
		if expected != tt.allow {
			t.Fatal("harness policy mismatch")
		}
		result := c.Call(context.Background(), tt.name, tt.args)
		if result.IsError == tt.allow {
			t.Fatalf("unexpected call result %+v", result)
		}
		if !tt.allow && (requests.Load() != before || handlers.Load() != hBefore) {
			t.Fatal("blocked request reached server")
		}
		if tt.allow && handlers.Load() != hBefore+1 {
			t.Fatal("allowed handler not executed")
		}
	}
	args := map[string]any{"query": "user@example.invalid"}
	before := requests.Load()
	if _, e := c.send(context.Background(), "tools/call", map[string]any{"name": "search_messages", "arguments": args, "query": "user@example.invalid"}); e == nil {
		t.Fatal("envelope inherited argument allowance")
	}
	if requests.Load() != before {
		t.Fatal("extra envelope data sent")
	}
	if !reflect.DeepEqual(args, map[string]any{"query": "user@example.invalid"}) {
		t.Fatal("arguments changed")
	}
	s.mu.Lock()
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
	if result := c.Call(context.Background(), "search_messages", args); result.IsError {
		t.Fatal(result)
	}
	before = requests.Load()
	if _, e := c.send(context.Background(), "tools/list", map[string]any{"cursor": "DEMO_ONLY_SECRET_001"}); e == nil {
		t.Fatal("cursor bypassed guard")
	}
	if requests.Load() != before {
		t.Fatal("secret cursor sent")
	}
	specs := []any{}
	for _, tool := range c.Tools() {
		specs = append(specs, tool.Spec())
	}
	encoded, _ := json.Marshal(specs)
	var values any
	json.Unmarshal(encoded, &values)
	if string(encoded) == "" {
		t.Fatal("empty specs")
	}
	for _, v := range values.([]any) {
		if _, ok := v.(map[string]any)["Destination"]; ok {
			t.Fatal("metadata in model spec")
		}
	}
}
func TestEgressMCPRedirectDoesNotForward(t *testing.T) {
	var stolen atomic.Int32
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1) }))
	defer thief.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, thief.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	g, e := egress.New(egress.Config{Version: 1, Targets: []egress.Target{{ID: "redirect", Destination: egress.Destination{Kind: "mcp", Address: s.URL}, Rules: rpcRules(nil)}}}, egress.Options{})
	if e != nil {
		t.Fatal(e)
	}
	c, e := NewProtectedClient(s.URL, g)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Connect(context.Background()); e == nil {
		t.Fatal("redirect accepted")
	}
	if stolen.Load() != 0 {
		t.Fatal("redirect forwarded request")
	}
}
