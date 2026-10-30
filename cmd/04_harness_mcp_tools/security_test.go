package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

func TestBadPolicyAndMissingSkillRuleFailBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "skills", "demo"), 0755)
	os.WriteFile(filepath.Join(root, "skills", "demo", "SKILL.md"), []byte("---\nname: demo\ndescription: demo\n---\npublic"), 0600)
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chdir(root); e != nil {
		t.Fatal(e)
	}
	defer os.Chdir(cwd)
	policy := filepath.Join(root, "policy.json")
	t.Setenv("AGENT_EGRESS_POLICY", policy)
	t.Setenv("MCP_SERVER_URL", s.URL)
	t.Setenv("LLM_BASE_URL", s.URL)
	t.Setenv("LLM_API_KEY", "")
	os.WriteFile(policy, []byte(`{"version":1,"unknown":true}`), 0600)
	if e = run(context.Background()); e == nil {
		t.Fatal("invalid policy accepted")
	}
	rules := []egress.Rule{}
	for _, method := range []string{"initialize", "notifications/initialized", "tools/list", "close"} {
		rules = append(rules, egress.Rule{Operation: egress.ControlOperation(method)})
	}
	cfg := egress.Config{Version: 1, Targets: []egress.Target{
		{ID: "model", Destination: egress.Destination{Kind: "llm", Address: s.URL + "/chat/completions"}, Rules: []egress.Rule{{Operation: egress.ModelOperation()}}},
		{ID: "mail", Destination: egress.Destination{Kind: "mcp", Address: s.URL}, Rules: rules},
	}}
	b, _ := json.Marshal(cfg)
	os.WriteFile(policy, b, 0600)
	if e = run(context.Background()); e == nil {
		t.Fatal("missing skill rule accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("startup contacted network before configuration validation")
	}
}
