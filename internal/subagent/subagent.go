// Package subagent — субагенты.
//
// Субагент — агент, которого вызвал другой агент для выполнения части
// работы. Он работает в собственном цикле и со своим контекстом, а
// вызвавшему агенту возвращает только итоговый результат. Субагент не
// особый вид агента, а роль, которую агент выполняет в конкретной ситуации.
//
// Как это отражено в коде:
//
//   - Субагент — это тот же harness.Harness, что и у главного агента.
//     Отдельного «вида агента» нет: Definition описывает только роль
//     (системный промпт, набор инструментов, лимиты).
//   - Toolbox реализует тот же интерфейс tools.Toolbox, что LocalToolbox и
//     mcp.Client. Харнесс главного агента поэтому не меняется: для него
//     запуск субагента — ещё один инструмент, run_subagent.
//   - Модель главного агента только предлагает запустить субагента: выбирает
//     тип по описанию и формулирует задачу. Запускает его харнесс.
//   - На каждый вызов собирается новый харнесс, поэтому контекст субагента
//     пустой: в нём нет истории, памяти и skills главного агента. Всё, что
//     нужно для задачи, модель главного агента должна написать в task.
//   - Главному агенту возвращается только итоговый ответ субагента. Его
//     промежуточные шаги (вызовы инструментов, результаты, загруженные
//     skills) в контекст главного агента не попадают.
//   - Задача субагента проходит те же проверки харнесса, что и запрос
//     пользователя: на валидность и на безопасность.
package subagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wannacry39/agent_nirs/internal/harness"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/skills"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

// ToolName — имя инструмента, которым главный агент запускает субагента.
const ToolName = "run_subagent"

// Definition — роль субагента: кто он, что умеет и в каких пределах работает.
type Definition struct {
	// Name — тип субагента, по нему модель главного агента его выбирает.
	Name string
	// Description — когда звать этого субагента. Попадает в описание
	// инструмента run_subagent, то есть в контекст главного агента.
	Description string
	// SystemPrompt — системный промпт субагента. Он не меняется от задачи
	// к задаче, задача приходит как запрос.
	SystemPrompt string
	// Toolboxes — инструменты субагента: локальные, MCP или другие субагенты.
	Toolboxes []tools.Toolbox
	// Skills — skills субагента. Skill, который выбрал субагент, попадает
	// только в его контекст и главному агенту не передаётся.
	Skills *skills.Library
	// Limits — лимиты субагента на одну задачу.
	Limits harness.Limits
	// LLM — модель субагента; nil — та же, что у главного агента.
	LLM *llm.Client
}

// Config — общие настройки для всех субагентов набора.
type Config struct {
	LLM      *llm.Client                // модель по умолчанию
	ReadOnly bool                       // права пользователя действуют и в субагенте
	Confirm  func(question string) bool // опасные вызовы субагента подтверждает пользователь
	Log      io.Writer                  // куда писать ход работы; nil — в stderr
}

// Toolbox — набор субагентов, доступный главному агенту как один
// инструмент run_subagent.
type Toolbox struct {
	cfg   Config
	defs  map[string]Definition
	names []string
}

// New создаёт набор субагентов.
func New(cfg Config, defs ...Definition) (*Toolbox, error) {
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	b := &Toolbox{cfg: cfg, defs: map[string]Definition{}}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" || d.SystemPrompt == "" {
			return nil, fmt.Errorf("у субагента должны быть Name, Description и SystemPrompt")
		}
		if _, exists := b.defs[d.Name]; exists {
			return nil, fmt.Errorf("субагент %q объявлен дважды", d.Name)
		}
		if d.LLM == nil && cfg.LLM == nil {
			return nil, fmt.Errorf("субагент %q: не задана модель", d.Name)
		}
		b.defs[d.Name] = d
		b.names = append(b.names, d.Name)
	}
	if len(b.names) == 0 {
		return nil, fmt.Errorf("нет ни одного субагента")
	}
	sort.Strings(b.names)
	return b, nil
}

// Name — для логов харнесса главного агента.
func (b *Toolbox) Name() string { return "субагент" }

// Tools возвращает описание run_subagent. Список субагентов с описаниями
// модель главного агента видит в описании инструмента.
func (b *Toolbox) Tools() []tools.Tool {
	var list strings.Builder
	list.WriteString("Передаёт часть работы субагенту. Субагент работает в своём цикле и со своим контекстом: " +
		"он не видит этот диалог, память и загруженные skills, поэтому в task нужно написать всё, что ему нужно знать. " +
		"Возвращает только итоговый ответ субагента.\nДоступные субагенты:")
	for _, name := range b.names {
		fmt.Fprintf(&list, "\n- %s: %s", name, b.defs[name].Description)
	}
	return []tools.Tool{{
		Name:        ToolName,
		Description: list.String(),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{"type": "string", "description": "Тип субагента из списка", "enum": b.names},
				"task":  map[string]any{"type": "string", "description": "Задача субагенту обычным текстом (строка, не JSON) со всеми нужными данными: города, адреса, имена"},
			},
			"required": []string{"agent", "task"},
		},
		// Сам запуск ничего не меняет. Опасные инструменты внутри субагента
		// подтверждает пользователь, как и у главного агента.
		Dangerous: false,
	}}
}

// Call запускает субагента: собирает для него новый харнесс (свой цикл,
// пустой контекст), выполняет задачу и возвращает только итоговый ответ.
func (b *Toolbox) Call(ctx context.Context, name string, args map[string]any) tools.Result {
	if name != ToolName {
		return tools.Result{Content: fmt.Sprintf("инструмент %q не найден", name), IsError: true}
	}
	agent, _ := args["agent"].(string)
	task, _ := args["task"].(string)
	def, ok := b.defs[agent]
	if !ok {
		return tools.Result{Content: fmt.Sprintf("субагента %q нет, доступны: %s", agent, strings.Join(b.names, ", ")), IsError: true}
	}

	model := def.LLM
	if model == nil {
		model = b.cfg.LLM
	}
	h, err := harness.New(harness.Config{
		LLM:          model,
		SystemPrompt: def.SystemPrompt,
		Toolboxes:    def.Toolboxes,
		Skills:       def.Skills,
		Limits:       def.Limits,
		// Памяти у субагента нет: к памяти обращается харнесс главного
		// агента, а нужные факты модель главного агента передаёт в task.
		Memory:   nil,
		ReadOnly: b.cfg.ReadOnly,
		Confirm:  b.confirm(agent),
		Log:      &prefixWriter{prefix: "    [субагент " + agent + "] ", out: b.cfg.Log},
	})
	if err != nil {
		return tools.Result{Content: "не удалось запустить субагента: " + err.Error(), IsError: true}
	}

	b.logf("[харнесс] запускаю субагента %s: свой цикл, пустой контекст", agent)
	answer, err := h.Run(ctx, task)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("субагент %s завершился с ошибкой: %v", agent, err), IsError: true}
	}
	b.logf("[харнесс] субагент %s вернул итог, его промежуточные шаги в контекст не попадают", agent)

	// Остановка по лимиту или отказ в проверке — это не решённая задача.
	if strings.HasPrefix(answer, "Остановлено:") || strings.HasPrefix(answer, "Запрос отклонён:") {
		return tools.Result{Content: fmt.Sprintf("субагент %s не решил задачу: %s", agent, answer), IsError: true}
	}
	return tools.Result{Content: answer}
}

// confirm — подтверждение опасного вызова субагента. Вопрос задаётся
// пользователю с пометкой, какой субагент хочет выполнить вызов.
func (b *Toolbox) confirm(agent string) func(string) bool {
	if b.cfg.Confirm == nil {
		return nil
	}
	return func(question string) bool {
		return b.cfg.Confirm("[субагент " + agent + "] " + question)
	}
}

func (b *Toolbox) logf(format string, args ...any) {
	fmt.Fprintf(b.cfg.Log, format+"\n", args...)
}

// prefixWriter добавляет отступ и метку к каждой строке лога субагента,
// чтобы в выводе было видно, где работает он, а где главный агент.
type prefixWriter struct {
	prefix string
	out    io.Writer
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.SplitAfter(string(p), "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(w.out, w.prefix+line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
