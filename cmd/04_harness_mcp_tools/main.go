// Вариант 4. LLM + харнесс + инструменты по MCP (и локальные).
//
// Отличие от варианта 3 только в том, откуда берутся инструменты.
// Инструменты почты реализованы не здесь, а на отдельном MCP-сервере
// (cmd/mcp_mail_server). Харнесс через MCP-клиент:
//  1. подключается к серверу (initialize);
//  2. получает список инструментов (tools/list) и кладёт их описания в контекст
//     рядом с локальными — для модели они неотличимы;
//  3. когда модель предлагает вызвать MCP-инструмент, отправляет серверу
//     tools/call и возвращает результат в контекст.
//
// Код харнесса при этом не меняется: MCP-клиент — ещё одна реализация
// tools.Toolbox.
//
// Запуск:
//
//	go run ./cmd/mcp_mail_server        # в отдельном терминале
//	go run ./cmd/04_harness_mcp_tools
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wannacry39/agent_nirs/internal/cli"
	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/harness"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/localtools"
	"github.com/wannacry39/agent_nirs/internal/mcp"
	"github.com/wannacry39/agent_nirs/internal/prompt"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

func main() {
	if err := run(context.Background()); err != nil {
		cli.Fatal(err)
	}
}

func run(ctx context.Context) (err error) {
	guard, err := cli.LoadSecurity()
	if err != nil {
		return err
	}
	defer func() {
		if guard != nil {
			err = egress.SafeError(err)
		}
	}()
	model := llm.NewFromEnv()
	if err := cli.BindModel(model, guard); err != nil {
		return err
	}
	library, err := cli.LoadSkills()
	if err != nil {
		return err
	}
	if err := cli.ValidateSkills(guard, !library.Empty()); err != nil {
		return err
	}
	serverURL := os.Getenv("MCP_SERVER_URL")
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8000/mcp"
	}
	mailServer := mcp.NewClient(serverURL)
	if guard != nil {
		mailServer, err = mcp.NewProtectedClient(serverURL, guard)
		if err != nil {
			return err
		}
	}
	if err := mailServer.Connect(ctx); err != nil {
		return fmt.Errorf("не удалось подключиться к MCP-серверу %s: %w (запущен ли go run ./cmd/mcp_mail_server?)", serverURL, err)
	}
	// Закрываем сессию на сервере при любом выходе из run.
	defer mailServer.Close(ctx)
	if guard == nil {
		fmt.Fprintf(os.Stderr, "[MCP-клиент] подключён к %s, инструментов: %d\n", serverURL, len(mailServer.Tools()))
	} else {
		fmt.Fprintf(os.Stderr, "[MCP-клиент] защищённое подключение, инструментов: %d\n", len(mailServer.Tools()))
	}

	mem, err := cli.OpenMemory()
	if err != nil {
		return err
	}

	h, err := harness.New(harness.Config{
		LLM:          model,
		Egress:       guard,
		SystemPrompt: prompt.Build(true, !library.Empty()),
		Toolboxes: []tools.Toolbox{
			localtools.New(), // локальные инструменты
			mailServer,       // MCP-инструменты
		},
		Memory:   mem,
		Skills:   library,
		ReadOnly: cli.ReadOnly(),
		Confirm:  cli.Confirm,
	})
	if err != nil {
		return err
	}

	cli.Run(ctx, h, "Вариант 4: LLM + харнесс + локальные и MCP-инструменты", os.Args[1:])
	return nil
}
