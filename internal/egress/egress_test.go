package egress

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const demoSecret = "DEMO_ONLY_SECRET_001"

func fixture(t *testing.T, allow []Allowance, audit *bytes.Buffer) *Guard {
	t.Helper()
	opts := Options{Secrets: []string{demoSecret}}
	if audit != nil {
		opts.Audit = audit
	}
	g, e := New(Config{Version: 1, Targets: []Target{{ID: "test", Destination: Local("test"), Rules: []Rule{{Operation: ToolOperation("search"), Allow: allow}, {Operation: ToolOperation("other")}}}}}, opts)
	if e != nil {
		t.Fatal(e)
	}
	return g
}
func TestDetectorsAndExactPaths(t *testing.T) {
	g := fixture(t, []Allowance{{Category: "email", Path: []any{"query"}}}, nil)
	for _, tt := range []struct {
		name, body string
		allow      bool
	}{
		{"public", `{"city":"Казань"}`, true},
		{"known", `{"text":"` + demoSecret + `"}`, false},
		{"nested", `{"rows":[{"text":"` + demoSecret + `"}]}`, false},
		{"key", `{"` + demoSecret + `":"public"}`, false},
		{"password", `{"password":"simple-value"}`, false},
		{"disguised", `{"kind":"schema","password":{"type":"string"}}`, false},
		{"token", `{"text":"sk-or-v1-SYNTHETIC00000000000"}`, false},
		{"private_key", `{"text":"-----BEGIN PRIVATE KEY-----"}`, false},
		{"assignment", `{"text":"пароль: synthetic-value"}`, false},
		{"phone", `{"text":"+7 (900) 123-45-67"}`, false},
		{"numeric_phone", `{"value":89001234567}`, false},
		{"quoted_assignment", `{"text":"\"password\":\"unknown-secret-value\""}`, false},
		{"allowed_email", `{"query":"user@example.invalid"}`, true},
		{"other_field", `{"text":"user@example.invalid"}`, false},
		{"nested_query", `{"nested":{"query":"user@example.invalid"}}`, false},
		{"dotted_key", `{"nested.query":"user@example.invalid"}`, false},
		{"slash_key", `{"/query":"user@example.invalid"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, e := g.Decode([]byte(tt.body))
			if e != nil {
				t.Fatal(e)
			}
			before := tt.body
			e = g.Check(context.Background(), ToolAttempt(Local("test"), "search", v))
			if (e == nil) != tt.allow {
				t.Fatalf("allow=%v error=%v", tt.allow, e)
			}
			if e != nil && (!errors.Is(e, ErrDenied) || strings.Contains(e.Error(), demoSecret) || strings.Contains(e.Error(), "example.invalid")) {
				t.Fatal("unsafe denial")
			}
			if before != tt.body {
				t.Fatal("input changed")
			}
		})
	}
	v, _ := g.Decode([]byte(`{"query":"user@example.invalid"}`))
	if g.Check(context.Background(), ToolAttempt(Local("test"), "other", v)) == nil {
		t.Fatal("allowance crossed operation")
	}
	if g.Check(context.Background(), ToolAttempt(Local("unknown"), "search", v)) == nil {
		t.Fatal("unknown destination allowed")
	}
}

func TestNumericSecretsPreservePrecisionAndExponent(t *testing.T) {
	for _, credential := range []bool{false, true} {
		opts := Options{}
		if credential {
			opts.Credentials = []string{"123456789012", "123456789012345678901234567890"}
		} else {
			opts.Secrets = []string{"123456789012", "123456789012345678901234567890"}
		}
		g, e := New(Config{Version: 1, Targets: []Target{{ID: "n", Destination: Local("n"), Rules: []Rule{{Operation: ToolOperation("n")}}}}}, opts)
		if e != nil {
			t.Fatal(e)
		}
		for _, raw := range []string{`{"x":123456789012}`, `{"x":1.23456789012e11}`, `{"x":123456789012345678901234567890}`, `{"x":1.23456789012345678901234567890e29}`} {
			v, e := g.Decode([]byte(raw))
			if e != nil {
				t.Fatal(e)
			}
			if g.Check(context.Background(), ToolAttempt(Local("n"), "n", v)) == nil {
				t.Fatal("numeric secret escaped")
			}
		}
	}
}
func TestFractionalCredentialsAreExact(t *testing.T) {
	g, e := New(Config{Version: 1, Targets: []Target{{ID: "n", Destination: Local("n"), Rules: []Rule{{Operation: ToolOperation("n")}}}}}, Options{Credentials: []string{"1234569990.123456999"}})
	if e != nil {
		t.Fatal(e)
	}
	v, e := g.Decode([]byte(`{"value":1.234569990123456999e9}`))
	if e != nil {
		t.Fatal(e)
	}
	if g.Check(context.Background(), ToolAttempt(Local("n"), "n", v)) == nil {
		t.Fatal("fractional credential rounded away")
	}
}

func TestLiteralCredentials(t *testing.T) {
	for _, literal := range []string{"true", "false", "null"} {
		g, e := New(Config{Version: 1, Targets: []Target{{ID: "n", Destination: Local("n"), Rules: []Rule{{Operation: ToolOperation("n")}}}}}, Options{Credentials: []string{literal}})
		if e != nil {
			t.Fatal(e)
		}
		v, e := g.Decode([]byte(`{"payload":` + literal + `}`))
		if e != nil {
			t.Fatal(e)
		}
		if g.Check(context.Background(), ToolAttempt(Local("n"), "n", v)) == nil {
			t.Fatal("literal credential allowed")
		}
	}
}

func TestSchemaDeclarationAndValues(t *testing.T) {
	g := fixture(t, nil, nil)
	for _, tt := range []struct {
		name, body string
		allow      bool
	}{
		{"declaration", `{"type":"object","properties":{"password":{"type":"string","description":"Пароль пользователя"}},"required":["password"]}`, true},
		{"description", `{"properties":{"password":{"type":"string","description":"` + demoSecret + `"}}}`, false},
		{"default", `{"properties":{"password":{"type":"string","default":"no-known-prefix"}}}`, false},
		{"examples", `{"properties":{"password":{"type":"string","examples":["no-known-prefix"]}}}`, false},
		{"nested_schema", `{"properties":{"x":{"type":"object","properties":{"api_key":{"type":"string"}}}}}`, true},
		{"unknown_annotation", `{"x-custom":{"password":"no-known-prefix"}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, e := g.Decode([]byte(tt.body))
			if e != nil {
				t.Fatal(e)
			}
			e = g.Check(context.Background(), Attempt{Destination: Local("test"), Operation: ToolOperation("search"), Fragments: []Fragment{{Kind: Schema, Value: v}}})
			if (e == nil) != tt.allow {
				t.Fatalf("allow=%v error=%v", tt.allow, e)
			}
		})
	}
}
func TestLimitsMalformedInputCancellationAndImmutability(t *testing.T) {
	g := fixture(t, nil, nil)
	for _, b := range []string{`{"query":1,"query":2}`, `{} {}`, `{"x":`, strings.Repeat(" ", g.maxBytes+1), strings.Repeat("[", 40) + `0` + strings.Repeat("]", 40)} {
		if _, e := g.Decode([]byte(b)); e == nil {
			t.Fatal("invalid/oversized input allowed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := g.Check(ctx, ToolAttempt(Local("test"), "search", map[string]any{})); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	data := map[string]any{"nested": []any{map[string]any{"value": "public"}}}
	before := map[string]any{"nested": []any{map[string]any{"value": "public"}}}
	if e := g.Check(context.Background(), ToolAttempt(Local("test"), "search", data)); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, data) {
		t.Fatal("guard mutated input")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	if g.Check(context.Background(), ToolAttempt(Local("test"), "search", cycle)) == nil {
		t.Fatal("cyclic input allowed")
	}
	if g.Check(context.Background(), ToolAttempt(Local("test"), "search", make(chan int))) == nil {
		t.Fatal("unsupported input allowed")
	}
}
func TestConfigurationStrictnessAndSnapshot(t *testing.T) {
	for _, s := range []string{`{"version":2}`, `{"version":1,"typo":true}`, `{"version":1,"version":1}`, `{"version":1} {}`, `{"version":1,"secret_env":["MISSING"]}`} {
		if _, e := Load(strings.NewReader(s), func(string) (string, bool) { return "", false }, nil); !errors.Is(e, ErrConfig) {
			t.Fatal("invalid configuration accepted", e)
		}
	}
	for _, d := range []Destination{{Kind: "llm", Address: "https://user:pass@example.invalid/v1"}, {Kind: "llm", Address: "https://example.invalid/?key=secret"}, {Kind: "mcp", Address: "http://example.invalid/mcp"}, {Kind: "mcp", Address: "http://localhost.evil.invalid/mcp"}} {
		if _, e := Normalize(d); e == nil {
			t.Fatal("unsafe destination accepted")
		}
	}
	cfg := Config{Version: 1, Targets: []Target{{ID: "test", Destination: Local("test"), Rules: []Rule{{Operation: ToolOperation("search"), Allow: []Allowance{{Category: "email", Path: []any{"query"}}}}}}}}
	g, e := New(cfg, Options{})
	if e != nil {
		t.Fatal(e)
	}
	cfg.Targets[0].Rules[0].Allow[0].Path[0] = "text"
	if g.Check(context.Background(), ToolAttempt(Local("test"), "search", map[string]any{"query": "user@example.invalid"})) != nil {
		t.Fatal("configuration was not snapshotted")
	}
}
func TestConcurrentAuditAndCredentialBinding(t *testing.T) {
	var audit bytes.Buffer
	g := fixture(t, []Allowance{{Category: "secret", Path: []any{"text"}}}, &audit)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.Check(context.Background(), ToolAttempt(Local("test"), "search", map[string]any{"text": demoSecret}), demoSecret) == nil {
				t.Error("credential allowed by generic secret exception")
			}
		}()
	}
	wg.Wait()
	if strings.Contains(audit.String(), demoSecret) || strings.Contains(audit.String(), "text") {
		t.Fatal("audit leaked input")
	}
	if strings.Count(audit.String(), "decision=sensitive_data") != 20 {
		t.Fatal("missing audit events")
	}
}
