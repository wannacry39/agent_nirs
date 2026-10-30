package tools

import (
	"encoding/json"
	"fmt"
	"math"
)

// ValidateArguments проверяет обязательные поля и JSON-типы параметров.
// Поддерживается подмножество JSON Schema: type, properties, required, items.
// Это проверка для учебных примеров, а не универсальный валидатор JSON Schema.
func ValidateArguments(schema map[string]any, args map[string]any) error {
	// Отсутствие arguments допустимо для инструмента без параметров.
	if args == nil {
		args = map[string]any{}
	}
	return validateValue(schema, args, "arguments")
}

func validateValue(schema map[string]any, value any, path string) error {
	typeName, _ := schema["type"].(string)
	if declared, exists := schema["type"]; exists && declared != typeName {
		return fmt.Errorf("%s: type должен быть строкой в учебном подмножестве JSON Schema", path)
	}
	wrongType := func() error { return fmt.Errorf("%s: ожидается %s", path, typeName) }
	switch typeName {
	case "string":
		if _, ok := value.(string); !ok {
			return wrongType()
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return wrongType()
		}
	case "number", "integer":
		// Защищённый декодер сохраняет лексему числа для проверки утечек.
		number, ok := value.(float64)
		if n, isNumber := value.(json.Number); isNumber {
			var err error
			number, err = n.Float64()
			ok = err == nil
		}
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || (typeName == "integer" && math.Trunc(number) != number) {
			return wrongType()
		}
	case "null":
		if value != nil {
			return wrongType()
		}
	case "object", "":
		object, ok := value.(map[string]any)
		if !ok {
			if typeName == "" {
				return nil
			}
			return wrongType()
		}
		for _, name := range (Tool{InputSchema: schema}).Required() {
			if _, exists := object[name]; !exists {
				return fmt.Errorf("%s: не передан обязательный параметр %q", path, name)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, child := range properties {
			field, exists := object[name]
			if !exists {
				continue
			}
			rules, _ := child.(map[string]any)
			if err := validateValue(rules, field, path+"."+name); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return wrongType()
		}
		items, _ := schema["items"].(map[string]any)
		for i, item := range array {
			if err := validateValue(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: тип схемы %q не поддерживается учебным валидатором", path, typeName)
	}
	return nil
}
