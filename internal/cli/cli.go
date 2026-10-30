// Package cli — общий интерфейс командной строки для примеров с харнессом.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/harness"
	"github.com/wannacry39/agent_nirs/internal/memory"
	"github.com/wannacry39/agent_nirs/internal/skills"
)

// Пути относительно корня репозитория: программы запускаются из него.
const (
	MemoryPath        = "data/memory.json"
	MemoryExamplePath = "data/memory.example.json"
	SkillsDir         = "skills"
)

var stdin = bufio.NewReader(os.Stdin)

// Confirm спрашивает пользователя «да/нет». Его харнесс вызывает перед
// выполнением опасного инструмента.
func Confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "\n%s [y/N]: ", question)
	answer, _ := stdin.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	fmt.Fprintln(os.Stderr)
	return answer == "y" || answer == "yes" || answer == "д" || answer == "да"
}

// ReadOnly — права пользователя «только чтение», если задана переменная
// окружения AGENT_READ_ONLY=1.
func ReadOnly() bool { return os.Getenv("AGENT_READ_ONLY") == "1" }

// OpenMemory открывает память. При первом запуске файл памяти создаётся
// из примера data/memory.example.json (сам файл памяти не хранится в git).
func OpenMemory() (result *memory.Memory, err error) {
	defer func() {
		if os.Getenv("AGENT_EGRESS_POLICY") != "" {
			err = egress.SafeError(err)
		}
	}()
	if _, err := os.Stat(MemoryPath); errors.Is(err, os.ErrNotExist) {
		example, err := os.ReadFile(MemoryExamplePath)
		switch {
		case err == nil:
			if err := os.MkdirAll(filepath.Dir(MemoryPath), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(MemoryPath, example, 0o644); err != nil {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "[харнесс] память создана из %s\n", MemoryExamplePath)
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(os.Stderr, "[харнесс] предупреждение: нет %s — запускайте из корня репозитория; память пустая\n", MemoryExamplePath)
		default:
			return nil, err
		}
	}
	return memory.Open(MemoryPath)
}

// LoadSkills загружает skills и предупреждает, если их не нашлось.
func LoadSkills() (result *skills.Library, err error) {
	defer func() {
		if os.Getenv("AGENT_EGRESS_POLICY") != "" {
			err = egress.SafeError(err)
		}
	}()
	library, err := skills.Load(SkillsDir)
	if err != nil {
		return nil, err
	}
	if library.Empty() {
		fmt.Fprintf(os.Stderr, "[харнесс] предупреждение: в папке %s нет skills — запускайте из корня репозитория\n", SkillsDir)
	}
	return library, nil
}

// Run — если запрос передан аргументами, выполняет его один раз,
// иначе запускает диалог. В диалоге харнесс хранит историю между запросами.
func Run(ctx context.Context, h *harness.Harness, title string, args []string) {
	if len(args) > 0 {
		answer(ctx, h, strings.Join(args, " "))
		return
	}
	fmt.Printf("%s\nВведите запрос (пустая строка или Ctrl+D — выход).\n", title)
	for {
		fmt.Print("\n> ")
		line, err := stdin.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		answer(ctx, h, line)
		if err != nil {
			return
		}
	}
}

func answer(ctx context.Context, h *harness.Harness, request string) {
	reply, err := h.Run(ctx, request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		return
	}
	fmt.Println("\nОтвет:", reply)
}

// Fatal печатает ошибку и завершает программу.
func Fatal(err error) {
	fmt.Fprintln(os.Stderr, "ошибка:", err)
	os.Exit(1)
}
