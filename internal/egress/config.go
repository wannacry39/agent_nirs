package egress

import (
	"encoding/json"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type Allowance struct {
	Category string `json:"category"`
	Path     []any  `json:"path"`
}
type Rule struct {
	Operation Operation   `json:"operation"`
	Allow     []Allowance `json:"allow,omitempty"`
}
type Target struct {
	ID          string      `json:"id"`
	Destination Destination `json:"destination"`
	Rules       []Rule      `json:"rules"`
}
type Config struct {
	Version   int      `json:"version"`
	MaxBytes  int      `json:"max_bytes,omitempty"`
	MaxDepth  int      `json:"max_depth,omitempty"`
	MaxNodes  int      `json:"max_nodes,omitempty"`
	SecretEnv []string `json:"secret_env,omitempty"`
	Targets   []Target `json:"targets"`
}
type Options struct {
	Secrets     []string
	Credentials []string
	Audit       io.Writer
}
type allowance struct {
	category string
	path     Path
}
type target struct {
	id    string
	rules map[Operation][]allowance
}

// Guard неизменяем после New. Мьютекс защищает только запись журнала.
type Guard struct {
	targets                      map[Destination]target
	secrets                      []string
	credentials                  []string
	maxBytes, maxDepth, maxNodes int
	audit                        io.Writer
	auditMu                      sync.Mutex
}

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,127}$`)
var variable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Normalize не угадывает доверие по названию хоста. HTTP разрешён только
// для явно внесённых в политику loopback-адресов демонстраций.
func Normalize(d Destination) (Destination, error) {
	switch d.Kind {
	case "local", "internal":
		if !identifier.MatchString(d.Address) {
			return Destination{}, ErrConfig
		}
	case "llm", "mcp":
		u, err := url.Parse(d.Address)
		if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Host == "" {
			return Destination{}, ErrConfig
		}
		if u.Scheme != "https" && (u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback()) {
			return Destination{}, ErrConfig
		}
		if p := u.Port(); p != "" {
			n, e := strconv.Atoi(p)
			if e != nil || n < 1 || n > 65535 {
				return Destination{}, ErrConfig
			}
		}
		u.Host = strings.ToLower(u.Host)
		d.Address = u.String()
	default:
		return Destination{}, ErrConfig
	}
	return d, nil
}

func New(cfg Config, opts Options) (*Guard, error) {
	if cfg.Version != 1 {
		return nil, ErrConfig
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 1 << 20
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 32
	}
	if cfg.MaxNodes == 0 {
		cfg.MaxNodes = 100000
	}
	if cfg.MaxBytes < 1 || cfg.MaxBytes > 16<<20 || cfg.MaxDepth < 1 || cfg.MaxDepth > 128 || cfg.MaxNodes < 1 || cfg.MaxNodes > 1000000 {
		return nil, ErrConfig
	}
	g := &Guard{targets: map[Destination]target{}, maxBytes: cfg.MaxBytes, maxDepth: cfg.MaxDepth, maxNodes: cfg.MaxNodes, audit: opts.Audit}
	ids := map[string]bool{}
	for _, t := range cfg.Targets {
		d, err := Normalize(t.Destination)
		if err != nil || !identifier.MatchString(t.ID) || ids[t.ID] {
			return nil, ErrConfig
		}
		if _, exists := g.targets[d]; exists {
			return nil, ErrConfig
		}
		ids[t.ID] = true
		rt := target{id: t.ID, rules: map[Operation][]allowance{}}
		for _, r := range t.Rules {
			if !validOperation(r.Operation) {
				return nil, ErrConfig
			}
			if _, exists := rt.rules[r.Operation]; exists {
				return nil, ErrConfig
			}
			compiled := []allowance{}
			for _, a := range r.Allow {
				if a.Path == nil || (a.Category != "email" && a.Category != "phone" && a.Category != "secret") {
					return nil, ErrConfig
				}
				p := Path{}
				for _, part := range a.Path {
					switch v := part.(type) {
					case string:
						p = append(p, Key(v))
					case int:
						if v < 0 || v > 1000000 {
							return nil, ErrConfig
						}
						p = append(p, Index(v))
					case float64:
						if v < 0 || v > 1000000 || v != float64(int(v)) {
							return nil, ErrConfig
						}
						p = append(p, Index(int(v)))
					default:
						return nil, ErrConfig
					}
				}
				for _, prev := range compiled {
					if prev.category == a.Category && equalPath(prev.path, p) {
						return nil, ErrConfig
					}
				}
				compiled = append(compiled, allowance{a.Category, p})
			}
			rt.rules[r.Operation] = compiled
		}
		g.targets[d] = rt
	}
	for _, s := range opts.Secrets {
		if len(s) < 8 || len(s) > g.maxBytes {
			return nil, ErrConfig
		}
		g.secrets = append(g.secrets, s)
	}
	if len(opts.Secrets) > 128 || len(opts.Credentials) > 64 {
		return nil, ErrConfig
	}
	for _, s := range opts.Credentials {
		if s != "" {
			if len(s) > g.maxBytes {
				return nil, ErrConfig
			}
			g.credentials = append(g.credentials, s)
		}
	}
	return g, nil
}
func validOperation(o Operation) bool {
	return (o.Kind == "tool_call" || o.Kind == "model_request" || o.Kind == "protocol_control") && identifier.MatchString(o.Name)
}
func equalPath(a, b Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Load отклоняет неизвестные поля, дубликаты JSON и лишние документы.
// Имена переменных задаются доверенной политикой; значения не журналируются.
func Load(r io.Reader, lookup func(string) (string, bool), audit io.Writer, credentials ...string) (*Guard, error) {
	b, e := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, ErrConfig
	}
	parser := &Guard{maxBytes: 1 << 20, maxDepth: 32, maxNodes: 100000}
	if _, e = parser.Decode(b); e != nil {
		return nil, ErrConfig
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var cfg Config
	if dec.Decode(&cfg) != nil {
		return nil, ErrConfig
	}
	secrets := []string{}
	seen := map[string]bool{}
	for _, name := range cfg.SecretEnv {
		if lookup == nil || !variable.MatchString(name) || seen[name] {
			return nil, ErrConfig
		}
		seen[name] = true
		value, ok := lookup(name)
		if !ok {
			return nil, ErrConfig
		}
		secrets = append(secrets, value)
	}
	return New(cfg, Options{Secrets: secrets, Credentials: credentials, Audit: audit})
}

// HasCredential проверяет согласованность сборки, не раскрывая реестр секретов.
func (g *Guard) HasCredential(s string) bool {
	if s == "" {
		return true
	}
	if g == nil {
		return false
	}
	for _, v := range g.credentials {
		if v == s {
			return true
		}
	}
	return false
}

// Validate проверяет связность сборки, не разрешая передачу данных заранее.
func (g *Guard) Validate(d Destination, o Operation) error {
	if g == nil {
		return ErrConfig
	}
	d, e := Normalize(d)
	if e != nil {
		return ErrConfig
	}
	t, ok := g.targets[d]
	if !ok {
		return ErrConfig
	}
	if _, ok = t.rules[o]; !ok {
		return ErrConfig
	}
	return nil
}
