// Package config loads the runtime's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
	"github.com/getitdone-gmbh/agent-fabrik/internal/llm"
)

// Config is the root configuration document.
type Config struct {
	Server     ServerConfig           `yaml:"server"`
	Memory     MemoryConfig           `yaml:"memory"`
	Log        LogConfig              `yaml:"log"`
	Providers  map[string]llm.Config  `yaml:"providers"`
	Connectors map[string]Connector   `yaml:"connectors"`
	Agents     map[string]AgentConfig `yaml:"agents"`
	Tenancy    TenancyConfig          `yaml:"tenancy"`

	// Warnings collected while loading (e.g. unset environment variables).
	Warnings []string `yaml:"-"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	Addr              string   `yaml:"addr"`
	AuthToken         string   `yaml:"auth_token"`
	AuthTokens        []string `yaml:"auth_tokens"`
	MaxConcurrentRuns int      `yaml:"max_concurrent_runs"`
	CORSOrigins       []string `yaml:"cors_origins"`
	MCP               *bool    `yaml:"mcp"`        // serve agents at /mcp (default true)
	OpenAI            *bool    `yaml:"openai_api"` // serve /v1/chat/completions (default true)
	UI                *bool    `yaml:"ui"`         // serve the web UI at /ui/ (default true)
	MaxBodyBytes      int64    `yaml:"max_body_bytes"`
}

// Tokens returns all configured bearer tokens.
func (s ServerConfig) Tokens() []string {
	var out []string
	if s.AuthToken != "" {
		out = append(out, s.AuthToken)
	}
	for _, t := range s.AuthTokens {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// MemoryConfig selects the session store.
type MemoryConfig struct {
	Type        string `yaml:"type"` // memory (default) or file
	Dir         string `yaml:"dir"`
	TTL         string `yaml:"ttl"`
	MaxSessions int    `yaml:"max_sessions"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json (default) or text
}

// Connector is a typed connector whose remaining fields are decoded by the
// connector implementation.
type Connector struct {
	Type string
	node yaml.Node
}

func (c *Connector) UnmarshalYAML(n *yaml.Node) error {
	var t struct {
		Type string `yaml:"type"`
	}
	if err := n.Decode(&t); err != nil {
		return err
	}
	c.Type = strings.ToLower(t.Type)
	c.node = *n
	return nil
}

// Decode decodes the connector's fields into v.
func (c Connector) Decode(v any) error { return c.node.Decode(v) }

// AgentConfig is an agent plus its triggers.
type AgentConfig struct {
	agent.Config `yaml:",inline"`
	Triggers     []Trigger `yaml:"triggers"`
}

// Trigger starts agent runs automatically.
type Trigger struct {
	Type string `yaml:"type"` // cron, interval, imap, webhook

	// cron / interval
	Schedule string `yaml:"schedule"` // 5-field cron, e.g. "0 8 * * 1-5"
	Every    string `yaml:"every"`    // Go duration, e.g. "15m"
	Timezone string `yaml:"timezone"`
	Prompt   string `yaml:"prompt"`
	Session  string `yaml:"session"` // fixed session id to keep memory across runs

	// imap
	Connector  string   `yaml:"connector"`
	Mailbox    string   `yaml:"mailbox"`
	Reply      *bool    `yaml:"reply"`     // reply with the agent's answer (default true)
	MarkRead   *bool    `yaml:"mark_read"` // default true
	MaxPerPoll int      `yaml:"max_per_poll"`
	AllowFrom  []string `yaml:"allow_from"` // sender globs; empty = anyone

	// webhook
	Name   string `yaml:"name"`   // path segment: /v1/hooks/{agent}/{name}
	Secret string `yaml:"secret"` // shared secret (header X-Webhook-Secret or ?secret=) or HMAC key
	Async  bool   `yaml:"async"`  // respond 202 immediately
}

// Load reads the config from path, or from the AGENT_CONFIG_YAML environment
// variable when path is empty.
func Load(path string) (*Config, error) {
	var data []byte
	var err error
	if path == "" {
		inline := os.Getenv("AGENT_CONFIG_YAML")
		if inline == "" {
			return nil, errors.New("no config: pass -config or set AGENT_CONFIG / AGENT_CONFIG_YAML")
		}
		data = []byte(inline)
	} else if data, err = os.ReadFile(path); err != nil {
		return nil, err
	}
	return Parse(data)
}

// Resolver resolves the expression inside ${...}.
type Resolver func(expr string) (string, error)

// ParseOptions customizes parsing (used for tenant documents).
type ParseOptions struct {
	// Resolve replaces ${...}; nil means environment variables and files.
	Resolve Resolver
	// Tenant documents: no server/tenancy sections, may have zero agents,
	// and are checked against Limits.
	Tenant         bool
	Limits         *Limits
	ExtraProviders []string // shared providers agents may reference
}

// Parse parses and validates the main config document.
func Parse(data []byte) (*Config, error) { return ParseWith(data, ParseOptions{}) }

// ParseWith parses and validates a config document with options.
func ParseWith(data []byte, opts ParseOptions) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var warnings []string
	resolve := opts.Resolve
	if resolve == nil {
		resolve = func(expr string) (string, error) { return resolveEnv(expr, &warnings) }
	}
	if err := expandNode(&root, resolve); err != nil {
		return nil, err
	}
	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.Warnings = warnings
	if opts.Tenant {
		cfg.Server, cfg.Tenancy, cfg.Memory, cfg.Log = ServerConfig{}, TenancyConfig{}, MemoryConfig{}, LogConfig{}
	}
	cfg.applyDefaults()
	if err := cfg.validate(opts); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		c.Server.Addr = ":" + port
	}
	if c.Server.MaxConcurrentRuns <= 0 {
		c.Server.MaxConcurrentRuns = 16
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = 1 << 20
	}
	for name, a := range c.Agents {
		a.Name = name
		c.Agents[name] = a
	}
}

// TTL parses the memory TTL.
func (m MemoryConfig) TTLDuration() time.Duration {
	d, _ := time.ParseDuration(m.TTL)
	return d
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)

// Validate checks cross references.
func (c *Config) Validate() error { return c.validate(ParseOptions{}) }

func (c *Config) validate(opts ParseOptions) error {
	var errs []string
	if len(c.Agents) == 0 && !opts.Tenant && !c.Tenancy.Enabled {
		errs = append(errs, "no agents defined")
	}
	extra := map[string]bool{}
	for _, p := range opts.ExtraProviders {
		extra[p] = true
	}
	if !opts.Tenant {
		errs = append(errs, c.Tenancy.validate(c.Providers)...)
	}
	if opts.Limits != nil {
		errs = append(errs, opts.Limits.check(c)...)
	}
	for name, p := range c.Providers {
		if _, err := llm.New(name, p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, name := range sortedKeys(c.Connectors) {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf("connector name %q must match %s", name, nameRe))
		}
		if name == "system" || name == "files" {
			errs = append(errs, fmt.Sprintf("connector name %q is reserved for the built-in connector", name))
		}
		if c.Connectors[name].Type == "shell" && c.Tenancy.Enabled {
			// A child process can read the parent's environment via /proc,
			// so even an operator agent's shell would expose every tenant.
			errs = append(errs, fmt.Sprintf("connector %q: shell is not allowed with tenancy.enabled", name))
		}
	}
	if _, err := time.ParseDuration(c.Memory.TTL); c.Memory.TTL != "" && err != nil {
		errs = append(errs, "memory.ttl: "+err.Error())
	}
	if c.Memory.Type == "file" && c.Memory.Dir == "" {
		errs = append(errs, "memory.dir is required for memory.type file")
	}
	for _, name := range sortedKeys(c.Agents) {
		a := c.Agents[name]
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf("agent name %q must match %s", name, nameRe))
		}
		if a.Provider == "" {
			if len(c.Providers) == 0 && len(opts.ExtraProviders) == 1 {
				a.Provider = opts.ExtraProviders[0]
				c.Agents[name] = a
			} else if len(c.Providers) == 1 {
				for p := range c.Providers {
					a.Provider = p
				}
				c.Agents[name] = a
			} else {
				errs = append(errs, fmt.Sprintf("agent %q: provider is required", name))
			}
		} else if _, ok := c.Providers[a.Provider]; !ok && !extra[a.Provider] {
			errs = append(errs, fmt.Sprintf("agent %q: unknown provider %q", name, a.Provider))
		}
		if _, own := c.Providers[a.Provider]; opts.Tenant && !own && extra[a.Provider] && a.Model != "" {
			// Shared providers are paid by the operator: the tenant gets the
			// operator's model, never a pricier one of its own choosing.
			errs = append(errs, fmt.Sprintf("agent %q: model cannot be chosen on the shared provider %q", name, a.Provider))
		}
		for i, t := range a.Triggers {
			where := fmt.Sprintf("agent %q trigger %d (%s)", name, i, t.Type)
			switch t.Type {
			case "cron":
				if _, err := ParseCron(t.Schedule); err != nil {
					errs = append(errs, where+": "+err.Error())
				}
				if t.Timezone != "" {
					if _, err := time.LoadLocation(t.Timezone); err != nil {
						errs = append(errs, where+": "+err.Error())
					}
				}
			case "interval":
				if d, err := time.ParseDuration(t.Every); err != nil || d < time.Second {
					errs = append(errs, where+": every must be a duration >= 1s")
				}
			case "imap":
				conn, ok := c.Connectors[t.Connector]
				if !ok || conn.Type != "email" && conn.Type != "imap" {
					errs = append(errs, where+": connector must name an email/imap connector")
				}
				if t.Every != "" {
					if _, err := time.ParseDuration(t.Every); err != nil {
						errs = append(errs, where+": "+err.Error())
					}
				}
			case "webhook":
				if t.Name != "" && !nameRe.MatchString(t.Name) {
					errs = append(errs, where+": invalid name")
				}
			default:
				errs = append(errs, where+": unknown trigger type (cron, interval, imap, webhook)")
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("invalid config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// ---------- environment expansion ----------

var envRe = regexp.MustCompile(`\$\$\{|\$\{([^}]+)\}`)

func expandNode(n *yaml.Node, resolve Resolver) error {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		v, err := ExpandWith(n.Value, resolve)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		n.Value = v
		if n.Style == 0 {
			// Let yaml re-resolve plain scalars (numbers, booleans) after expansion.
			n.Tag = ""
		}
		return nil
	}
	for _, c := range n.Content {
		if err := expandNode(c, resolve); err != nil {
			return err
		}
	}
	return nil
}

// Expand replaces ${VAR}, ${VAR:-default}, ${VAR:?message} and
// ${file:/path} references. "$${" yields a literal "${".
func Expand(s string, warnings *[]string) (string, error) {
	return ExpandWith(s, func(expr string) (string, error) { return resolveEnv(expr, warnings) })
}

// ExpandWith replaces ${...} using resolve. "$${" yields a literal "${".
func ExpandWith(s string, resolve Resolver) (string, error) {
	var firstErr error
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$${" {
			return "${"
		}
		v, err := resolve(m[2 : len(m)-1])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	})
	return out, firstErr
}

func resolveEnv(expr string, warnings *[]string) (string, error) {
	if strings.HasPrefix(expr, "file:") {
		b, err := os.ReadFile(strings.TrimPrefix(expr, "file:"))
		return strings.TrimRight(string(b), "\r\n"), err
	}
	name, def, mode := expr, "", ""
	if i := strings.Index(expr, ":-"); i >= 0 {
		name, def, mode = expr[:i], expr[i+2:], "default"
	} else if i := strings.Index(expr, ":?"); i >= 0 {
		name, def, mode = expr[:i], expr[i+2:], "required"
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v, nil
	}
	switch mode {
	case "default":
		return def, nil
	case "required":
		if def == "" {
			def = "is required"
		}
		return "", fmt.Errorf("environment variable %s %s", name, def)
	}
	if warnings != nil {
		*warnings = append(*warnings, "environment variable "+name+" is not set")
	}
	return "", nil
}
