// Вариант 5. LLM + харнесс + субагенты.
//
// Отличие от вариантов 3 и 4 в том, что часть работы главный агент может
// передать субагенту. Субагент — это такой же агент (LLM + харнесс), но
// со своим циклом и своим контекстом:
//  1. модель главного агента выбирает субагента по описанию и пишет задачу
//     (вызов инструмента run_subagent);
//  2. харнесс главного агента собирает для субагента новый харнесс с пустым
//     контекстом, своим системным промптом, своими инструментами и лимитами;
//  3. субагент решает задачу в своём цикле и возвращает итог, который
//     попадает в контекст главного агента как результат инструмента.
//
// Код харнесса не меняется: набор субагентов — ещё одна реализация
// tools.Toolbox (internal/subagent).
//
// Субагенты в примере:
//   - weather: сводка погоды, локальный инструмент get_weather и skill
//     weather_report;
//   - mail: разбор почты, MCP-инструменты почтового сервера и skill
//     inbox_digest. Подключается, только если запущен MCP-сервер.
//
// Главный агент и субагент mail ходят к одному и тому же инструменту
// search_messages одного MCP-сервера через один MCP-клиент: главный —
// для быстрой проверки, субагент — для объёмного разбора, чьи шаги
// в контекст главного не попадают.
//
// Запуск:
//
//	go run ./cmd/mcp_mail_server        # в отдельном терминале (для субагента mail)
//	go run ./cmd/05_harness_subagents
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wannacry39/agent_nirs/internal/cli"
	"github.com/wannacry39/agent_nirs/internal/harness"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/localtools"
	"github.com/wannacry39/agent_nirs/internal/mcp"
	"github.com/wannacry39/agent_nirs/internal/prompt"
	"github.com/wannacry39/agent_nirs/internal/subagent"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

func main() {
	if err := run(context.Background()); err != nil {
		cli.Fatal(err)
	}
}

func run(ctx context.Context) error {
	model := llm.NewFromEnv()
	mem, err := cli.OpenMemory()
	if err != nil {
		return err
	}
	library, err := cli.LoadSkills()
	if err != nil {
		return err
	}
	local := localtools.New()

	// Лимиты субагента меньше, чем у главного агента: он решает часть задачи.
	subLimits := harness.Limits{MaxSteps: 6, MaxTokens: 20_000}

	defs := []subagent.Definition{{
		Name:         "weather",
		Description:  "Сводка погоды по одному или нескольким городам. Передай названия городов.",
		SystemPrompt: prompt.Subagent("готовишь сводку погоды по городам.", !library.Empty()),
		Toolboxes:    []tools.Toolbox{tools.Subset(local, "get_weather")},
		Skills:       library,
		Limits:       subLimits,
	}}

	// Субагент почты работает с MCP-сервером. Если сервер не запущен,
	// главный агент работает только с субагентом погоды.
	//
	// Один и тот же MCP-клиент (mailServer) попадает и к главному агенту
	// (только search_messages: быстрая проверка), и к субагенту mail (все
	// инструменты почты: разбор, сводка, удаление). Оба харнесса ходят
	// к одному инструменту одного MCP-сервера; для сервера это один клиент
	// и одна сессия. Субагенты выполняются по очереди, поэтому вызовы не
	// пересекаются.
	serverURL := os.Getenv("MCP_SERVER_URL")
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8000/mcp"
	}
	mailServer := mcp.NewClient(serverURL)
	mainTools := []tools.Toolbox{tools.Subset(local, "get_current_time", "save_note")}
	if err := mailServer.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[MCP-клиент] сервер %s недоступен (%v): субагент mail отключён\n", serverURL, err)
	} else {
		defer mailServer.Close(ctx)
		fmt.Fprintf(os.Stderr, "[MCP-клиент] подключён к %s, инструментов: %d\n", serverURL, len(mailServer.Tools()))
		mainTools = append(mainTools, tools.Subset(mailServer, "search_messages"))
		defs = append(defs, subagent.Definition{
			Name:         "mail",
			Description:  "Разбор входящей почты: сводка, поиск писем от отправителя, удаление письма. Передай адреса и что нужно сделать.",
			SystemPrompt: prompt.Subagent("разбираешь почту пользователя.", !library.Empty()),
			Toolboxes:    []tools.Toolbox{mailServer},
			Skills:       library,
			Limits:       subLimits,
		})
	}

	subagents, err := subagent.New(subagent.Config{
		LLM:      model,
		ReadOnly: cli.ReadOnly(),
		Confirm:  cli.Confirm,
	}, defs...)
	if err != nil {
		return err
	}

	// У главного агента: время, заметки, быстрый поиск по почте (тот же
	// MCP-клиент, что у субагента mail) и субагенты. Погоду он сам не
	// получает, только через субагента weather.
	h, err := harness.New(harness.Config{
		LLM:          model,
		SystemPrompt: prompt.Build(true, false) + prompt.Delegation(),
		Toolboxes:    append(mainTools, subagents),
		Memory:       mem,
		ReadOnly:     cli.ReadOnly(),
		Confirm:      cli.Confirm,
	})
	if err != nil {
		return err
	}

	cli.Run(ctx, h, "Вариант 5: LLM + харнесс + субагенты", os.Args[1:])
	return nil
}
