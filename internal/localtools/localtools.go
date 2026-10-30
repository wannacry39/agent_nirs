// Package localtools — набор локальных инструментов для примеров.
//
// Локальный инструмент реализован и выполняется внутри самой программы:
// харнесс вызывает его без MCP-сервера и без сети. Данные о погоде
// выдуманные: важно не содержание, а то, как инструмент вызывается.
package localtools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

var weather = map[string]string{
	"москва":          "+8 °C, облачно, ветер 4 м/с",
	"казань":          "+5 °C, дождь, ветер 6 м/с",
	"санкт-петербург": "+6 °C, туман, ветер 3 м/с",
}

// NotesPath — файл, в который save_note записывает заметки.
const NotesPath = "data/notes.txt"

// New возвращает локальные инструменты get_weather, get_current_time
// и save_note (опасный: меняет данные пользователя).
func New() *tools.LocalToolbox {
	box := tools.NewLocalToolbox()

	box.Register(tools.Tool{
		Name:        "get_weather",
		Destination: egress.Local("weather"),
		Description: "Возвращает текущую погоду в городе",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "description": "Название города"},
			},
			"required": []string{"city"},
		},
	}, func(_ context.Context, args map[string]any) (string, error) {
		city, _ := args["city"].(string)
		if forecast, ok := weather[strings.ToLower(strings.TrimSpace(city))]; ok {
			return fmt.Sprintf("%s: %s", city, forecast), nil
		}
		return "", fmt.Errorf("нет данных о погоде для города %q", city)
	})

	box.Register(tools.Tool{
		Name:        "get_current_time",
		Destination: egress.Local("clock"),
		Description: "Возвращает текущие дату и время",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ map[string]any) (string, error) {
		return time.Now().Format("02.01.2006 15:04"), nil
	})

	box.Register(tools.Tool{
		Name:        "save_note",
		Destination: egress.Local("notes"),
		Description: "Сохраняет заметку пользователя в файл заметок",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "Текст заметки"},
			},
			"required": []string{"text"},
		},
		// Инструмент меняет данные, поэтому харнесс спросит подтверждение.
		Dangerous: true,
	}, func(_ context.Context, args map[string]any) (string, error) {
		text, _ := args["text"].(string)
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("пустая заметка")
		}
		if err := os.MkdirAll(filepath.Dir(NotesPath), 0o755); err != nil {
			return "", err
		}
		file, err := os.OpenFile(NotesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		defer file.Close()
		line := time.Now().Format("02.01.2006 15:04") + " " + text + "\n"
		if _, err := file.WriteString(line); err != nil {
			return "", err
		}
		return "заметка сохранена в " + NotesPath, nil
	})

	return box
}
