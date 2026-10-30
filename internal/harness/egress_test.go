package harness

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/mcp"
	"github.com/wannacry39/agent_nirs/internal/memory"
	"github.com/wannacry39/agent_nirs/internal/skills"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

func protectedHarness(t *testing.T, f *fakeLLM, cfg Config, includeSkill bool) (*Harness, error) {
	t.Helper()
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	model := &llm.Client{BaseURL: s.URL, HTTP: s.Client()}
	targets := []egress.Target{{ID: "model", Destination: model.Destination(), Rules: []egress.Rule{{Operation: egress.ModelOperation()}}}}
	if includeSkill {
		targets = append(targets, egress.Target{ID: "skills", Destination: egress.SkillDestination(), Rules: []egress.Rule{{Operation: egress.ToolOperation("load_skill")}}})
	}
	byDest := map[egress.Destination][]egress.Rule{}
	for _, box := range cfg.Toolboxes {
		for _, tool := range box.Tools() {
			byDest[tool.Destination] = append(byDest[tool.Destination], egress.Rule{Operation: egress.ToolOperation(tool.Name)})
		}
	}
	for d, rules := range byDest {
		targets = append(targets, egress.Target{ID: "local", Destination: d, Rules: rules})
	}
	g, e := egress.New(egress.Config{Version: 1, Targets: targets}, egress.Options{Secrets: []string{"DEMO_ONLY_SECRET_001"}})
	if e != nil {
		t.Fatal(e)
	}
	model.Guard = g
	cfg.LLM = model
	cfg.Egress = g
	return New(cfg)
}
func TestEgressProtectedSkillLoadAndExternalRefusal(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "private")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: private\ndescription: обычные инструкции\n---\nDEMO_ONLY_SECRET_001\n"), 0600)
	library, e := skills.Load(dir)
	if e != nil {
		t.Fatal(e)
	}
	f := &fakeLLM{reply: func(request) llm.Message { return toolCall("s1", "load_skill", `{"name":"private"}`) }}
	var logs bytes.Buffer
	h, e := protectedHarness(t, f, Config{Skills: library, Log: &logs}, true)
	if e != nil {
		t.Fatal(e)
	}
	_, e = h.Run(context.Background(), "выбери инструкцию")
	if !errors.Is(e, egress.ErrDenied) {
		t.Fatal(e)
	}
	if len(f.requests) != 1 {
		t.Fatal("skill secret reached external model")
	}
	if len(h.context.Skills) != 1 || !strings.Contains(h.context.Skills[0].Text, "DEMO_ONLY_SECRET_001") {
		t.Fatal("skill was not loaded or was rewritten")
	}
	if !h.context.HasSkill("private") {
		t.Fatal("skill lost")
	}
	if strings.Contains(logs.String(), "DEMO_ONLY_SECRET_001") {
		t.Fatal("skill leaked to logs")
	}
	before := len(h.context.Skills)
	result := h.execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Name: "load_skill", Arguments: `{"name":"private"}`}}, &budget{limits: DefaultLimits, start: time.Now()})
	if result.IsError || len(h.context.Skills) != before {
		t.Fatal("repeat skill load changed context")
	}
	// Новый харнесс не получает skills другого агента даже при общей политике.
	cfg := h.cfg
	other, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if len(other.context.Skills) != 0 {
		t.Fatal("skill shared between agents")
	}
	if _, e = protectedHarness(t, f, Config{Skills: library}, false); e == nil {
		t.Fatal("missing skill policy silently accepted")
	}
}
func TestEgressMemoryAndCompressionAreProtected(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "compression"}[compression], func(t *testing.T) {
			f := &fakeLLM{reply: func(request) llm.Message { return llm.Message{Content: "unexpected"} }}
			cfg := Config{}
			var memoryFile string
			if !compression {
				memoryFile = filepath.Join(t.TempDir(), "memory.json")
				m, e := memory.Open(memoryFile)
				if e != nil {
					t.Fatal(e)
				}
				if e = m.Remember("контакт сотрудника user@example.invalid"); e != nil {
					t.Fatal(e)
				}
				cfg.Memory = m
			} else {
				cfg.Context = ContextPolicy{MaxHistoryMessages: 2}
			}
			h, e := protectedHarness(t, f, cfg, false)
			if e != nil {
				t.Fatal(e)
			}
			if compression {
				h.context.AddUser("DEMO_ONLY_SECRET_001")
				h.context.AddAssistant(llm.Message{Content: "старый ответ"})
			}
			var before []byte
			if memoryFile != "" {
				before, _ = os.ReadFile(memoryFile)
			}
			_, e = h.Run(context.Background(), "контакт сотрудника")
			if !errors.Is(e, egress.ErrDenied) || len(f.requests) != 0 {
				t.Fatalf("external request made: %v", e)
			}
			if compression && (len(h.context.History) != 3 || h.context.Summary != "") {
				t.Fatal("compression destroyed history")
			}
			if memoryFile != "" {
				after, _ := os.ReadFile(memoryFile)
				if !bytes.Equal(before, after) {
					t.Fatal("memory changed")
				}
			}
		})
	}
}
func TestEgressBlocksBeforeConfirmationAndPreservesResults(t *testing.T) {
	box := tools.NewLocalToolbox()
	executed, confirmed := 0, 0
	for _, name := range []string{"read", "write"} {
		box.Register(tools.Tool{Name: name, Destination: egress.Local("demo"), Dangerous: name == "write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, map[string]any) (string, error) { executed++; return "public", nil })
	}
	f := &fakeLLM{reply: func(request) llm.Message {
		return llm.Message{ToolCalls: []llm.ToolCall{
			{ID: "one", Function: llm.FunctionCall{Name: "read", Arguments: `{}`}},
			{ID: "two", Function: llm.FunctionCall{Name: "write", Arguments: `{"password":"DEMO_ONLY_SECRET_001"}`}},
		}}
	}}
	var log bytes.Buffer
	h, e := protectedHarness(t, f, Config{Toolboxes: []tools.Toolbox{box}, Log: &log, Confirm: func(string) bool { confirmed++; return true }}, false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = h.Run(context.Background(), "выполни действия")
	if !errors.Is(e, egress.ErrDenied) {
		t.Fatal(e)
	}
	if executed != 1 || confirmed != 0 {
		t.Fatalf("executed=%d confirmed=%d", executed, confirmed)
	}
	results := map[string]int{}
	for _, m := range h.context.History {
		if m.Role == "tool" {
			results[m.ToolCallID]++
		}
	}
	if results["one"] != 1 || results["two"] != 1 {
		t.Fatal("tool result pairing broken", results)
	}
	if strings.Contains(log.String(), "DEMO_ONLY_SECRET_001") {
		t.Fatal("arguments leaked to logs")
	}
	if len(f.requests) != 1 {
		t.Fatal("forbidden tool call history was sent to model")
	}
}

func TestEgressRejectsPartiallyProtectedAssembly(t *testing.T) {
	f := &fakeLLM{reply: func(request) llm.Message { return llm.Message{Content: "not expected"} }}
	h, e := protectedHarness(t, f, Config{}, false)
	if e != nil {
		t.Fatal(e)
	}
	cfg := h.cfg
	cfg.Toolboxes = []tools.Toolbox{mcp.NewClient("https://example.invalid/mcp")}
	if _, e = New(cfg); !errors.Is(e, egress.ErrConfig) {
		t.Fatal("unprotected MCP client accepted", e)
	}
	cfg = h.cfg
	cfg.LLM = &llm.Client{BaseURL: cfg.LLM.BaseURL, HTTP: cfg.LLM.HTTP}
	if _, e = New(cfg); !errors.Is(e, egress.ErrConfig) {
		t.Fatal("unprotected model client accepted", e)
	}
	cfg = h.cfg
	model := *cfg.LLM
	model.APIKey = "atypical-credential-without-binding"
	cfg.LLM = &model
	if _, e = New(cfg); !errors.Is(e, egress.ErrConfig) {
		t.Fatal("unregistered model credential accepted", e)
	}
	if len(f.requests) != 0 {
		t.Fatal("partial assembly made network request")
	}
}
