package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z0-9-]{2,}`)
var phonePattern = regexp.MustCompile(`(?:\+?7|8)[ ()-]*[0-9]{3}[ ()-]*[0-9]{3}[ -]*[0-9]{2}[ -]*[0-9]{2}|\+1[ ()-]*[0-9]{3}[ ()-]*555[ -]*01[0-9]{2}`)
var tokenPattern = regexp.MustCompile(`\b(?:sk-(?:or-v1-)?[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9]{16,})\b|-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----`)
var assignmentPattern = regexp.MustCompile(`(?i)(?:password|passwd|api[_ -]?key|access[_ -]?token|client[_ -]?secret|пароль|ключ[_ -]?доступа)["']?\s*[:=]\s*["']?[^\s"',;{}]+`)

func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	k = strings.NewReplacer("_", "", "-", "", " ", "").Replace(k)
	switch k {
	case "password", "passwd", "pwd", "apikey", "accesstoken", "refreshtoken", "clientsecret", "privatekey", "пароль", "ключдоступа":
		return true
	}
	return false
}

// Decode ограничивает вход до разбора, глубину и число узлов; запрещает
// дубликаты ключей, чтобы детектор и получатель не выбирали разные значения.
func (g *Guard) Decode(b []byte) (any, error) {
	if g == nil || len(b) > g.maxBytes {
		return nil, reject("size", nil)
	}
	if !utf8.Valid(b) {
		return nil, reject("invalid_data", nil)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	nodes := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		nodes++
		if depth > g.maxDepth || nodes > g.maxNodes {
			return nil, reject("complexity", nil)
		}
		t, e := dec.Token()
		if e != nil {
			return nil, reject("invalid_data", nil)
		}
		switch v := t.(type) {
		case json.Delim:
			switch v {
			case '{':
				m := map[string]any{}
				for dec.More() {
					k, e := dec.Token()
					if e != nil {
						return nil, reject("invalid_data", nil)
					}
					key, ok := k.(string)
					if !ok {
						return nil, reject("invalid_data", nil)
					}
					if _, ok = m[key]; ok {
						return nil, reject("duplicate_key", nil)
					}
					x, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					m[key] = x
				}
				end, e := dec.Token()
				if e != nil || end != json.Delim('}') {
					return nil, reject("invalid_data", nil)
				}
				return m, nil
			case '[':
				a := []any{}
				for dec.More() {
					x, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, x)
				}
				end, e := dec.Token()
				if e != nil || end != json.Delim(']') {
					return nil, reject("invalid_data", nil)
				}
				return a, nil
			}
			return nil, reject("invalid_data", nil)
		case json.Number:
			if _, e := numericText(v); e != nil {
				return nil, reject("invalid_data", nil)
			}
			return v, nil
		default:
			return v, nil
		}
	}
	v, e := read(0)
	if e != nil {
		return nil, e
	}
	if _, e = dec.Token(); e != io.EOF {
		return nil, reject("invalid_data", nil)
	}
	return v, nil
}

type scanner struct {
	g            *Guard
	ctx          context.Context
	nodes, bytes int
	found        []Finding
	credentials  []string
}

func (s *scanner) tick(depth, bytes int) error {
	if e := s.ctx.Err(); e != nil {
		return reject("canceled", e)
	}
	s.nodes++
	s.bytes += bytes
	if depth > s.g.maxDepth || s.nodes > s.g.maxNodes || s.bytes > s.g.maxBytes || len(s.found) > 1000 {
		return reject("complexity", nil)
	}
	return nil
}
func (s *scanner) add(category, rule string, p Path) {
	s.found = append(s.found, Finding{category, rule, append(Path(nil), p...)})
}
func (s *scanner) text(v string, p Path) {
	for _, c := range s.g.credentials {
		if strings.Contains(v, c) {
			s.add("credential", "credential", p)
			break
		}
	}
	for _, c := range s.credentials {
		if c != "" && strings.Contains(v, c) {
			s.add("credential", "credential", p)
			break
		}
	}
	for _, c := range s.g.secrets {
		if strings.Contains(v, c) {
			s.add("secret", "known_secret", p)
			break
		}
	}
	if tokenPattern.MatchString(v) {
		s.add("secret", "token_format", p)
	}
	if assignmentPattern.MatchString(v) {
		s.add("secret", "assignment", p)
	}
	if emailPattern.MatchString(v) {
		s.add("email", "email", p)
	}
	if phonePattern.MatchString(v) {
		s.add("phone", "phone", p)
	}
}
func (s *scanner) walk(v any, p Path, depth int, secret bool) error {
	if e := s.tick(depth, 1); e != nil {
		return e
	}
	if f, ok := v.(Fragment); ok {
		if f.Kind == Schema {
			return s.schema(f.Value, p, depth+1, false)
		}
		if f.Kind != Data && f.Kind != Description {
			return reject("invalid_fragment", nil)
		}
		return s.walk(f.Value, p, depth+1, secret)
	}
	if secret && v != nil {
		_, object := v.(map[string]any)
		_, array := v.([]any)
		if t, ok := v.(string); !object && !array && (!ok || t != "") {
			s.add("secret", "sensitive_field", p)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q := child(p, Key(k))
			if e := s.tick(depth, len(k)); e != nil {
				return e
			}
			s.text(k, q)
			if e := s.walk(x[k], q, depth+1, secret || sensitiveKey(k)); e != nil {
				return e
			}
		}
	case []any:
		for i, item := range x {
			if e := s.walk(item, child(p, Index(i)), depth+1, secret); e != nil {
				return e
			}
		}
	case string:
		if e := s.tick(depth, len(x)); e != nil {
			return e
		}
		s.text(x, p)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return reject("invalid_data", nil)
		}
		s.text(strconv.FormatFloat(x, 'f', -1, 64), p)
	case json.Number:
		canonical, e := numericText(x)
		if e != nil {
			return e
		}
		if e = s.tick(depth, len(x)+len(canonical)); e != nil {
			return e
		}
		s.text(string(x), p)
		if canonical != string(x) {
			s.text(canonical, p)
		}
	case bool:
		s.text(strconv.FormatBool(x), p)
	case nil:
		s.text("null", p)
	default:
		return reject("invalid_data", nil)
	}
	return nil
}

// Числовой JSON тоже может содержать телефон или зарегистрированный секрет.
// Сохраняем лексему и проверяем точную целую запись, включая экспоненты,
// без округления float64 и без неограниченного раздувания больших чисел.
func numericText(n json.Number) (string, error) {
	raw := string(n)
	exponent := 0
	mantissa := raw
	if len(raw) > 4096 {
		return "", reject("number_range", nil)
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exp, e := strconv.Atoi(raw[i+1:])
		if e != nil || exp < -1024 || exp > 1024 {
			return "", reject("number_range", nil)
		}
		exponent = exp
		mantissa = raw[:i]
	}
	f, e := strconv.ParseFloat(raw, 64)
	if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", reject("invalid_data", nil)
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return "", reject("invalid_data", nil)
	}
	if r.IsInt() {
		return r.Num().String(), nil
	}
	scale := -exponent
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		scale += len(mantissa) - i - 1
	}
	if scale < 0 || scale > 5120 {
		return "", reject("number_range", nil)
	}
	// У исходного JSON конечная десятичная дробь; при таком scale
	// знаменатель делит 10^scale, поэтому FloatString не округляет значение.
	return strings.TrimRight(strings.TrimRight(r.FloatString(scale), "0"), "."), nil
}
func (s *scanner) schema(v any, p Path, depth int, propertySecret bool) error {
	if e := s.tick(depth, 1); e != nil {
		return e
	}
	m, ok := v.(map[string]any)
	if !ok {
		if _, ok = v.(bool); ok {
			return s.walk(v, p, depth+1, false)
		}
		return reject("invalid_schema", nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value := m[k]
		q := child(p, Key(k))
		if e := s.tick(depth, len(k)); e != nil {
			return e
		}
		s.text(k, q)
		switch k {
		case "properties", "patternProperties", "$defs", "definitions":
			children, ok := value.(map[string]any)
			if !ok {
				return reject("invalid_schema", nil)
			}
			names := make([]string, 0, len(children))
			for n := range children {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				np := child(q, Key(n))
				if e := s.tick(depth, len(n)); e != nil {
					return e
				}
				s.text(n, np)
				if e := s.schema(children[n], np, depth+1, k == "properties" && sensitiveKey(n)); e != nil {
					return e
				}
			}
		case "items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames":
			if e := s.schema(value, q, depth+1, propertySecret); e != nil {
				return e
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			a, ok := value.([]any)
			if !ok {
				return reject("invalid_schema", nil)
			}
			for i, item := range a {
				if e := s.schema(item, child(q, Index(i)), depth+1, propertySecret); e != nil {
					return e
				}
			}
		case "default", "examples", "enum", "const":
			if e := s.walk(value, q, depth+1, propertySecret); e != nil {
				return e
			}
		default:
			if e := s.walk(value, q, depth+1, sensitiveKey(k)); e != nil {
				return e
			}
		}
	}
	return nil
}

// Evaluate не изменяет данные. Разрешение точного пути не распространяется
// на соседние поля, другие инструменты или другого получателя.
func (g *Guard) Evaluate(ctx context.Context, a Attempt, credentials ...string) (Decision, error) {
	if g == nil {
		return Decision{}, ErrConfig
	}
	d, e := Normalize(a.Destination)
	if e != nil {
		return Decision{Code: "destination"}, reject("destination", nil)
	}
	t, ok := g.targets[d]
	if !ok {
		return Decision{Code: "destination"}, reject("destination", nil)
	}
	rules, ok := t.rules[a.Operation]
	if !ok {
		return Decision{Code: "operation"}, reject("operation", nil)
	}
	s := scanner{g: g, ctx: ctx, credentials: credentials}
	for _, f := range a.Fragments {
		if f.Kind == Schema {
			e = s.schema(f.Value, f.Path, 0, false)
		} else if f.Kind == Data || f.Kind == Description {
			e = s.walk(f.Value, f.Path, 0, false)
		} else {
			e = reject("invalid_fragment", nil)
		}
		if e != nil {
			g.event(t.id, "analysis_error", len(s.found))
			return Decision{Code: "analysis_error"}, e
		}
	}
	if e = ctx.Err(); e != nil {
		return Decision{Code: "canceled"}, reject("canceled", e)
	}
	for _, f := range s.found {
		allowed := false
		for _, r := range rules {
			if f.Category != "credential" && r.category == f.Category && equalPath(r.path, f.Path) {
				allowed = true
				break
			}
		}
		if !allowed {
			g.event(t.id, "sensitive_data", len(s.found))
			return Decision{Code: "sensitive_data", Findings: s.found}, reject("sensitive_data", nil)
		}
	}
	g.event(t.id, "allow", len(s.found))
	return Decision{Allowed: true, Code: "allow", Findings: s.found}, nil
}
func (g *Guard) Check(ctx context.Context, a Attempt, credentials ...string) error {
	_, e := g.Evaluate(ctx, a, credentials...)
	return e
}
func (g *Guard) event(id, code string, count int) {
	if g.audit != nil {
		g.auditMu.Lock()
		defer g.auditMu.Unlock()
		fmt.Fprintf(g.audit, "[egress] destination=%s decision=%s findings=%d\n", id, code, count)
	}
}
