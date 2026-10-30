// Демонстрация контроля реальных исходящих запросов без API-ключа.
// Получатели работают локально, но объявлены отдельными назначениями политики.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "демонстрация не прошла:", err)
		os.Exit(1)
	}
}
func run() error {
	ctx := context.Background()
	var toolRequests, modelRequests atomic.Int32
	server := mcp.NewServer("egress-demo", "1")
	readOnly := true
	for _, name := range []string{"get_weather", "search_messages"} {
		server.AddTool(mcp.ServerTool{Info: mcp.ToolInfo{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &readOnly}}, Handler: func(context.Context, map[string]any) (string, error) {
			toolRequests.Add(1)
			return "учебные данные", nil
		}})
	}
	rpc := httptest.NewServer(server)
	defer rpc.Close()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelRequests.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"готово"}}],"usage":{"total_tokens":1}}`)
	}))
	defer modelServer.Close()
	model := &llm.Client{BaseURL: modelServer.URL, HTTP: modelServer.Client()}
	rules := []egress.Rule{{Operation: egress.ToolOperation("get_weather")}, {Operation: egress.ToolOperation("search_messages"), Allow: []egress.Allowance{{Category: "email", Path: []any{"query"}}}}}
	for _, method := range []string{"initialize", "notifications/initialized", "tools/list", "close"} {
		rules = append(rules, egress.Rule{Operation: egress.ControlOperation(method)})
	}
	guard, err := egress.New(egress.Config{Version: 1, Targets: []egress.Target{
		{ID: "demo-mail", Destination: egress.Destination{Kind: "mcp", Address: rpc.URL}, Rules: rules},
		{ID: "demo-model", Destination: model.Destination(), Rules: []egress.Rule{{Operation: egress.ModelOperation()}}},
	}}, egress.Options{Secrets: []string{"DEMO_ONLY_SECRET_001"}})
	if err != nil {
		return err
	}
	model.Guard = guard
	client, err := mcp.NewProtectedClient(rpc.URL, guard)
	if err != nil {
		return err
	}
	if err = client.Connect(ctx); err != nil {
		return err
	}
	defer client.Close(ctx)
	for i, c := range []struct {
		name  string
		args  map[string]any
		allow bool
	}{
		{"get_weather", map[string]any{"city": "Казань"}, true},
		{"get_weather", map[string]any{"city": "Казань DEMO_ONLY_SECRET_001"}, false},
		{"search_messages", map[string]any{"query": "user@example.invalid"}, true},
		{"get_weather", map[string]any{"city": "user@example.invalid"}, false},
	} {
		before := toolRequests.Load()
		result := client.Call(ctx, c.name, c.args)
		if result.IsError == c.allow {
			return fmt.Errorf("сценарий %d: неверное решение", i+1)
		}
		expected := int32(0)
		if c.allow {
			expected = 1
		}
		if toolRequests.Load()-before != expected {
			return fmt.Errorf("сценарий %d: неверное число выполнений", i+1)
		}
		fmt.Printf("Сценарий %d: разрешено=%v, выполнений инструмента=%d — PASS\n", i+1, c.allow, expected)
	}
	_, err = model.Complete(ctx, []llm.Message{{Role: "user", Content: "DEMO_ONLY_SECRET_001"}}, nil)
	if !errors.Is(err, egress.ErrDenied) || modelRequests.Load() != 0 {
		return fmt.Errorf("секрет передан модели")
	}
	fmt.Println("Сценарий 5: секрет в контексте, запросов к модели=0 — PASS")
	schema := map[string]any{"type": "object", "properties": map[string]any{"password": map[string]any{"type": "string"}}}
	_, err = model.Complete(ctx, []llm.Message{{Role: "user", Content: "public"}}, []llm.ToolSpec{{Type: "function", Function: llm.FunctionSpec{Name: "demo", Parameters: schema}}})
	if err != nil || modelRequests.Load() != 1 {
		return fmt.Errorf("декларация схемы заблокирована")
	}
	fmt.Println("Сценарий 6: декларация password, запросов к модели=1 — PASS")
	return nil
}
