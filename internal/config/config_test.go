package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpand(t *testing.T) {
	t.Setenv("A", "alpha")
	t.Setenv("EMPTY", "")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret"), []byte("s3cr3t\n"), 0o600)
	var warn []string
	cases := map[string]string{
		"${A}":                       "alpha",
		"x-${A}-y":                   "x-alpha-y",
		"${MISSING:-def}":            "def",
		"${EMPTY:-def}":              "def",
		"$${A}":                      "${A}",
		"${file:" + dir + "/secret}": "s3cr3t",
		"${MISSING}":                 "",
	}
	for in, want := range cases {
		got, err := Expand(in, &warn)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "MISSING") {
		t.Errorf("warnings: %v", warn)
	}
	if _, err := Expand("${NEED:?must be set}", nil); err == nil || !strings.Contains(err.Error(), "must be set") {
		t.Errorf("required: %v", err)
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("KEY", "sk-test")
	t.Setenv("STEPS", "7")
	t.Setenv("TRICKY", "a: b # not yaml")
	doc := `
server:
  auth_token: ${KEY}
providers:
  claude:
    type: anthropic
    api_key: ${KEY}
connectors:
  web:
    type: http
    base_url: https://api.example.com
    headers:
      X-Tricky: ${TRICKY}
agents:
  helper:
    description: test
    system: hi
    max_steps: ${STEPS}
    tools: ["web.*"]
    triggers:
      - type: cron
        schedule: "*/5 * * * *"
      - type: interval
        every: 10m
      - type: webhook
        name: gh
        secret: x
`
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["helper"]
	if a.Name != "helper" || a.Provider != "claude" || a.MaxSteps != 7 || len(a.Triggers) != 3 {
		t.Errorf("agent: %+v", a)
	}
	if cfg.Server.Addr == "" || cfg.Server.Tokens()[0] != "sk-test" {
		t.Errorf("server: %+v", cfg.Server)
	}
	var h struct {
		Headers map[string]string `yaml:"headers"`
	}
	cfg.Connectors["web"].Decode(&h)
	if h.Headers["X-Tricky"] != "a: b # not yaml" {
		t.Errorf("env value must not be parsed as YAML: %q", h.Headers["X-Tricky"])
	}
}

func TestValidateErrors(t *testing.T) {
	doc := `
providers:
  a: {type: anthropic}
  b: {type: nope}
agents:
  x:
    triggers:
      - type: cron
        schedule: "bad"
      - type: imap
        connector: missing
`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"unknown type", "provider is required", "5 fields", "email/imap connector"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

func TestCron(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 9, 25, 10, 7, 30, 0, loc) // Friday
	cases := []struct {
		expr string
		want time.Time
	}{
		{"*/15 * * * *", time.Date(2026, 9, 25, 10, 15, 0, 0, loc)},
		{"0 8 * * 1-5", time.Date(2026, 9, 28, 8, 0, 0, 0, loc)}, // next Monday
		{"30 10 * * *", time.Date(2026, 9, 25, 10, 30, 0, 0, loc)},
		{"0 0 1 * *", time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		{"@hourly", time.Date(2026, 9, 25, 11, 0, 0, 0, loc)},
		{"0 9 * * 7", time.Date(2026, 9, 27, 9, 0, 0, 0, loc)}, // Sunday as 7
		{"0 12 29 2 *", time.Date(2028, 2, 29, 12, 0, 0, 0, loc)},
		{"0 6 * * */7", time.Date(2026, 9, 27, 6, 0, 0, 0, loc)}, // Sundays via step
		{"0 6 * * 5-7", time.Date(2026, 9, 26, 6, 0, 0, 0, loc)}, // Fri-Sun
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := cr.Next(base); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", c.expr, got, c.want)
		}
	}
	for _, bad := range []string{"* * *", "61 * * * *", "*/0 * * * *", "a b c d e"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestShippedConfigsParse(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_TOKEN", "x")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("RUNTIME_SECRET_KEY", "k")
	for _, p := range []string{"../../agents.yaml", "../../examples/full.yaml", "../../examples/multitenant.yaml"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestEmbeddedConfigTenancyFromEnv(t *testing.T) {
	t.Setenv("TENANCY_ENABLED", "true")
	t.Setenv("DATABASE_URL", "postgres://db")
	b, _ := os.ReadFile("../../agents.yaml")
	// Without the key the error says which variable to set, before connecting.
	t.Setenv("RUNTIME_SECRET_KEY", "")
	if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), "RUNTIME_SECRET_KEY") {
		t.Fatalf("missing key not reported: %v", err)
	}
	t.Setenv("RUNTIME_SECRET_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	cfg, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tenancy.Enabled || cfg.Tenancy.Store.URL != "postgres://db" || cfg.Tenancy.PlanLimits("pro").MaxAgents != 100 || cfg.Tenancy.PlanLimits("free").AllowsShared() {
		t.Errorf("tenancy: %+v", cfg.Tenancy)
	}
	t.Setenv("TENANCY_ENABLED", "false")
	t.Setenv("DATABASE_URL", "")
	if _, err := Parse(b); err != nil {
		t.Errorf("single-tenant default must stay valid: %v", err)
	}
}

func TestTenantCapsAndSharedModel(t *testing.T) {
	doc := `
agents:
  a:
    provider: shared
    model: the-most-expensive-one
    run_timeout_seconds: 99999
    parallel_tools: 64
`
	lim := DefaultLimits()
	_, err := ParseWith([]byte(doc), ParseOptions{Tenant: true, Limits: &lim, ExtraProviders: []string{"shared"}})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"model cannot be chosen", "run_timeout_seconds", "parallel_tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
	ok := "agents:\n  a: {provider: shared, system: hi}\n"
	if _, err := ParseWith([]byte(ok), ParseOptions{Tenant: true, Limits: &lim, ExtraProviders: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
}
