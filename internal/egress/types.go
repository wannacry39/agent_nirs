// Package egress — локальный контроль исходящих данных.
// Модель предлагает действие, а харнесс и адаптеры применяют эту обязательную
// проверку. Пакет не исполняет инструменты, не хранит контекст и не делает HTTP.
package egress

import (
	"context"
	"errors"
)

// Destination задаётся приложением, а не описанием инструмента от модели.
type Destination struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
}

// Operation отделяет смысл вызова от метода JSON-RPC.
type Operation struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Segment различает ключ объекта и индекс массива, включая ключ "0".
type Segment struct {
	Key   string
	Index int
	Array bool
}
type Path []Segment

func Key(s string) Segment         { return Segment{Key: s} }
func Index(i int) Segment          { return Segment{Index: i, Array: true} }
func child(p Path, s Segment) Path { q := append(Path(nil), p...); return append(q, s) }

type FragmentKind uint8

const (
	Data FragmentKind = iota
	Schema
	Description
)

// Fragment — доверенная маркировка адаптера; JSON не может создать этот тип.
// Value может содержать вложенные Fragment, но никогда не отправляется вместо
// исходного тела запроса. Пути нужны только внутреннему механизму политики.
type Fragment struct {
	Kind         FragmentKind
	Value        any
	Path         Path
	EnvelopePath Path
}
type Attempt struct {
	Destination Destination
	Operation   Operation
	Fragments   []Fragment
}
type Finding struct {
	Category string
	Rule     string
	Path     Path
}
type Decision struct {
	Allowed  bool
	Code     string
	Findings []Finding
}

var ErrDenied = errors.New("egress: передача запрещена")
var ErrConfig = errors.New("egress: некорректная конфигурация защиты")

type denied struct {
	code  string
	cause error
}

func (d *denied) Error() string {
	return "egress: передача запрещена (" + d.code + ")"
}
func (d *denied) Unwrap() []error           { return []error{ErrDenied, d.cause} }
func reject(code string, cause error) error { return &denied{code: code, cause: cause} }

// SafeError не копирует текст недоверенной сетевой/пользовательской ошибки.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrConfig) {
		return ErrConfig
	}
	var d *denied
	if errors.As(err, &d) {
		return d
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return reject("deadline", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return reject("canceled", context.Canceled)
	}
	return errors.New("защищённая операция завершилась ошибкой; содержимое скрыто")
}

func ToolOperation(name string) Operation    { return Operation{Kind: "tool_call", Name: name} }
func ModelOperation() Operation              { return Operation{Kind: "model_request", Name: "chat_completions"} }
func ControlOperation(name string) Operation { return Operation{Kind: "protocol_control", Name: name} }
func Local(name string) Destination          { return Destination{Kind: "local", Address: name} }
func SkillDestination() Destination          { return Destination{Kind: "internal", Address: "harness_context"} }
func ToolAttempt(d Destination, name string, args any) Attempt {
	if args == nil {
		args = map[string]any{}
	}
	return Attempt{Destination: d, Operation: ToolOperation(name), Fragments: []Fragment{{Value: args}}}
}
