// Вариант 1. LLM + инструменты, без харнесса.
//
// Модели передаются системный промпт и описания инструментов — так же, как
// в вариантах 3 и 4, чтобы отличие было только в отсутствии харнесса.
// Модель отвечает вызовом инструмента: именем и параметрами. Но вызов
// инструмента — это только предложение: сама LLM ничего не выполняет.
// Харнесса нет, поэтому:
//   - вызов никто не выполнит, результат инструмента не появится;
//   - цикла агента нет: после одного ответа модели всё заканчивается;
//   - нет истории и памяти: LLM stateless, второй вызов не знает о первом;
//   - нет проверок запроса, лимитов и подтверждения опасных действий.
//
// Запуск: go run ./cmd/01_llm_tools "Какая погода в Казани?"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wannacry39/agent_nirs/internal/cli"
	"github.com/wannacry39/agent_nirs/internal/llm"
	"github.com/wannacry39/agent_nirs/internal/localtools"
	"github.com/wannacry39/agent_nirs/internal/prompt"
)

func main() {
	if os.Getenv("AGENT_EGRESS_POLICY") != "" {
		cli.Fatal(fmt.Errorf("вариант 01 не подключает защиту; используйте вариант с харнессом"))
	}
	fmt.Fprintln(os.Stderr, "[egress] контроль исходящих данных выключен (вариант без харнесса)")
	ctx := context.Background()
	model := llm.NewFromEnv()

	request := "Какая сейчас погода в Казани?"
	if len(os.Args) > 1 {
		request = strings.Join(os.Args[1:], " ")
	}

	// Описания инструментов. Реализации у них есть (localtools), но здесь
	// их некому вызвать: модели передаются только описания.
	var specs []llm.ToolSpec
	for _, tool := range localtools.New().Tools() {
		specs = append(specs, tool.Spec())
	}
	printJSON("Список инструментов, переданный модели", specs)

	system := llm.Message{Role: "system", Content: prompt.Build(true, false)}

	// Единственный вызов модели.
	fmt.Println("\nЗапрос:", request)
	resp, err := model.Complete(ctx, []llm.Message{system, {Role: "user", Content: request}}, specs)
	if err != nil {
		cli.Fatal(err)
	}

	if len(resp.Message.ToolCalls) == 0 {
		fmt.Println("\nМодель ответила текстом:", resp.Message.Content)
	} else {
		printJSON("\nМодель вернула вызов инструмента", resp.Message.ToolCalls)
		fmt.Println("\nНа этом всё. LLM только предложила вызвать инструмент.")
		fmt.Println("Выполнить вызов, вернуть модели результат и вызвать её снова")
		fmt.Println("некому: этим занимается харнесс, а его здесь нет.")
	}

	// Второй вызов: LLM не хранит состояние между вызовами.
	followUp := "А какой город я только что спрашивал?"
	fmt.Println("\nВторой запрос (без истории, её некому хранить):", followUp)
	resp, err = model.Complete(ctx, []llm.Message{system, {Role: "user", Content: followUp}}, specs)
	if err != nil {
		cli.Fatal(err)
	}
	fmt.Println("Ответ модели:", resp.Message.Content)
	fmt.Println("\nМодель не знает про первый запрос: история передаётся только в контексте.")
}

func printJSON(title string, v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("%s:\n%s\n", title, raw)
}
