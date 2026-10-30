package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

func TestSecurityStartupDoesNotSilentlyDisable(t *testing.T) {
	t.Setenv("AGENT_EGRESS_POLICY", "")
	g, e := LoadSecurity()
	if e != nil || g != nil {
		t.Fatal(e)
	}
	t.Setenv("AGENT_EGRESS_POLICY", filepath.Join(t.TempDir(), "absent.json"))
	if _, e = LoadSecurity(); !errors.Is(e, egress.ErrConfig) {
		t.Fatal("missing policy accepted")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte(`{"version":1,"typo":true}`), 0600)
	t.Setenv("AGENT_EGRESS_POLICY", path)
	if _, e = LoadSecurity(); !errors.Is(e, egress.ErrConfig) {
		t.Fatal("malformed policy accepted")
	}
}
func TestExamplePolicyAndAutomaticCredentialRegistration(t *testing.T) {
	b, e := os.ReadFile("../../config/egress.example.json")
	if e != nil {
		t.Fatal(e)
	}
	var cfg egress.Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		t.Fatal(e)
	}
	var audit bytes.Buffer
	g, e := egress.Load(strings.NewReader(string(b)), nil, &audit, "nonstandard-provider-key-001")
	if e != nil {
		t.Fatal(e)
	}
	if !g.HasCredential("nonstandard-provider-key-001") {
		t.Fatal("credential not registered")
	}
	if e = g.Validate(egress.SkillDestination(), egress.ToolOperation("load_skill")); e != nil {
		t.Fatal(e)
	}
	d := egress.Destination{Kind: "mcp", Address: "http://127.0.0.1:8000/mcp"}
	e = g.Check(context.Background(), egress.ToolAttempt(d, "search_messages", map[string]any{"query": "nonstandard-provider-key-001"}))
	if !errors.Is(e, egress.ErrDenied) {
		t.Fatal("provider credential passed MCP")
	}
	if strings.Contains(audit.String(), "nonstandard-provider-key-001") {
		t.Fatal("audit leaked credential")
	}
}
