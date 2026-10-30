// Вариант 3. LLM + харнесс + локальные инструменты.
//
// Всё из варианта 2, плюс локальные инструменты: их реализация находится
// в этой же программе (internal/localtools), и харнесс выполняет их сам,
// без сети. Теперь работает полный цикл агента: модель предлагает вызов,
// харнесс проверяет и выполняет его, результат возвращается в контекст,
// модель вызывается снова — пока не ответит текстом или не кончатся лимиты.
//
// Вместе с инструментами появляются:
//   - skills: в контексте только их названия и описания, полный текст
//     модель загружает встроенным инструментом харнесса load_skill;
//   - подтверждение опасных вызовов: save_note меняет данные пользователя,
//     поэтому харнесс спрашивает разрешение перед его выполнением;
//   - права пользователя: при AGENT_READ_ONLY=1 запрос на изменение данных
//     отклоняется до цикла, а save_note не передаётся модели.
//
// Запуск: go run ./cmd/03_harness_local_tools
package main

import (
	"context"
	"os"

	"github.com/wannacry39/agent_nirs/internal/cli"
	"github.com/wannacry39/agent_nirs/internal/harness"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/localtools"
	"github.com/wannacry39/agent_nirs/internal/prompt"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

func main() {
	guard, err := cli.LoadSecurity()
	if err != nil {
		cli.Fatal(err)
	}
	model := llm.NewFromEnv()
	if err := cli.BindModel(model, guard); err != nil {
		cli.Fatal(err)
	}
	mem, err := cli.OpenMemory()
	if err != nil {
		cli.Fatal(err)
	}
	library, err := cli.LoadSkills()
	if err != nil {
		cli.Fatal(err)
	}
	if err := cli.ValidateSkills(guard, !library.Empty()); err != nil {
		cli.Fatal(err)
	}

	h, err := harness.New(harness.Config{
		LLM:          model,
		Egress:       guard,
		SystemPrompt: prompt.Build(true, !library.Empty()),
		Toolboxes:    []tools.Toolbox{localtools.New()}, // локальные инструменты
		Memory:       mem,
		Skills:       library,
		ReadOnly:     cli.ReadOnly(),
		Confirm:      cli.Confirm,
	})
	if err != nil {
		cli.Fatal(err)
	}

	cli.Run(context.Background(), h, "Вариант 3: LLM + харнесс + локальные инструменты", os.Args[1:])
}
