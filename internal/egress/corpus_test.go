package egress

import (
	"context"
	"testing"
)

// Небольшой фиксированный синтетический корпус: проверяет и обнаружение,
// и ложные срабатывания. Он не измеряет качество на произвольных документах.
func TestDetectorCorpus(t *testing.T) {
	g, e := New(Config{Version: 1, Targets: []Target{{ID: "corpus", Destination: Local("corpus"), Rules: []Rule{{Operation: ToolOperation("scan")}}}}}, Options{Secrets: []string{demoSecret}, Credentials: []string{"123456789012"}})
	if e != nil {
		t.Fatal(e)
	}
	corpus := []struct {
		name, body string
		schema     bool
		want       string
	}{
		{"public_city", `{"city":"Москва"}`, false, ""},
		{"public_number", `{"count":42}`, false, ""},
		{"public_schema", `{"properties":{"password":{"type":"string","description":"Пароль пользователя"}}}`, true, ""},
		{"public_defaults", `{"properties":{"color":{"type":"string","default":"red","enum":["red","blue"]}}}`, true, ""},
		{"email", `{"text":"user@example.invalid"}`, false, "email"},
		{"phone", `{"text":"+7 (900) 123-45-67"}`, false, "phone"},
		{"numeric_phone", `{"value":89001234567}`, false, "phone"},
		{"literal_secret", `{"text":"DEMO_ONLY_SECRET_001"}`, false, "secret"},
		{"password_field", `{"password":"unknown-synthetic-value"}`, false, "secret"},
		{"quoted_assignment", `{"text":"\"password\":\"unknown-synthetic-value\""}`, false, "secret"},
		{"token", `{"text":"sk-or-v1-SYNTHETIC00000000000"}`, false, "secret"},
		{"pem", `{"text":"-----BEGIN PRIVATE KEY-----"}`, false, "secret"},
		{"schema_default", `{"properties":{"password":{"type":"string","default":"unknown-synthetic-value"}}}`, true, "secret"},
		{"numeric_credential", `{"value":123456789012}`, false, "credential"},
		{"scientific_credential", `{"value":1.23456789012e11}`, false, "credential"},
	}
	type metric struct{ tp, tn, fp, fn int }
	metrics := map[string]*metric{}
	for _, c := range []string{"secret", "credential", "email", "phone"} {
		metrics[c] = &metric{}
	}
	for _, tt := range corpus {
		t.Run(tt.name, func(t *testing.T) {
			v, e := g.Decode([]byte(tt.body))
			if e != nil {
				t.Fatal(e)
			}
			kind := Data
			if tt.schema {
				kind = Schema
			}
			d, _ := g.Evaluate(context.Background(), Attempt{Destination: Local("corpus"), Operation: ToolOperation("scan"), Fragments: []Fragment{{Kind: kind, Value: v}}})
			found := map[string]bool{}
			for _, f := range d.Findings {
				found[f.Category] = true
			}
			for category, m := range metrics {
				want, actual := tt.want == category, found[category]
				switch {
				case want && actual:
					m.tp++
				case !want && !actual:
					m.tn++
				case !want && actual:
					m.fp++
				case want && !actual:
					m.fn++
				}
				if want != actual {
					t.Errorf("category %s: expected=%v actual=%v", category, want, actual)
				}
			}
		})
	}
	for _, c := range []string{"secret", "credential", "email", "phone"} {
		m := metrics[c]
		t.Logf("corpus category=%s cases=%d TP=%d TN=%d FP=%d FN=%d", c, len(corpus), m.tp, m.tn, m.fp, m.fn)
	}
}
