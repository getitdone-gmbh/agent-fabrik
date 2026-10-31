package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/llm"
)

// TenancyConfig enables multi-tenant mode: many users manage their own
// agents through the API inside one runtime process.
type TenancyConfig struct {
	Enabled bool        `yaml:"enabled"`
	Store   StoreConfig `yaml:"store"`
	// SecretKey (base64, 32 bytes) encrypts tenant secrets at rest.
	SecretKey    string             `yaml:"secret_key"`
	ExternalAuth ExternalAuthConfig `yaml:"external_auth"`
	// DefaultPlan applies to new tenants; Plans maps plan names to limits.
	DefaultPlan string            `yaml:"default_plan"`
	Plans       map[string]Limits `yaml:"plans"`
	// SharedProviders lists top-level providers tenants may use without
	// their own API key (billed to the operator, capped by plan limits).
	SharedProviders []string `yaml:"shared_providers"`
	// RuntimeIdleTTL: unused tenant runtimes are unloaded after this time.
	RuntimeIdleTTL    string `yaml:"runtime_idle_ttl"`
	MaxCachedRuntimes int    `yaml:"max_cached_runtimes"`
	SchedulerWorkers  int    `yaml:"scheduler_workers"`
	SessionTTL        string `yaml:"session_ttl"`
	// RunLogDays keeps a record of every agent run (input, tool calls,
	// answer) for this many days. Default 30; negative disables the log.
	RunLogDays int `yaml:"run_log_days"`
	// Notify posts alerts and agent messages to the operator's platform,
	// which tells the tenant's owner (see internal/notify).
	Notify NotifyConfig `yaml:"notify"`
	// Replicas is the number of runtime replicas. Rate and concurrency
	// limits are counted per process, so each replica enforces its share
	// (limit / replicas, rounded up) and the tenant gets the plan's total.
	Replicas int `yaml:"replicas"`
	// AllowNetworks lets tenant connections reach these internal CIDRs.
	AllowNetworks []string `yaml:"allow_networks"`
}

// StoreConfig selects where tenant data lives.
type StoreConfig struct {
	Type string `yaml:"type"` // memory | file | postgres
	Path string `yaml:"path"` // file
	URL  string `yaml:"url"`  // postgres
}

// ExternalAuthConfig lets users of an existing identity provider or
// platform authenticate with their own bearer token (JWT or API key). The
// token is validated by calling a userinfo endpoint (e.g. OIDC /userinfo);
// each user becomes a tenant automatically.
type ExternalAuthConfig struct {
	Enabled     bool   `yaml:"enabled"`
	UserinfoURL string `yaml:"userinfo_url"`
	CacheTTL    string `yaml:"cache_ttl"`
	// Response fields (dot paths): user id (default "sub", as in OIDC),
	// display name, and an optional field whose value selects the plan.
	IDField   string `yaml:"id_field"`
	NameField string `yaml:"name_field"`
	PlanField string `yaml:"plan_field"`
	// PlanMap maps plan_field values (e.g. "free", "pro") to plan names.
	PlanMap map[string]string `yaml:"plan_map"`
	// TenantPrefix is prepended to user ids (default "ext-").
	TenantPrefix string `yaml:"tenant_prefix"`
	// RequireField (dot path) must be true in the userinfo response, e.g.
	// "admin" to let only the IdP's admins in while a rollout is not ready
	// for everyone. Others are refused as unauthorized, and no tenant is
	// created for them.
	RequireField string `yaml:"require_field"`
}

// IDFieldOrDefault returns the id field path.
func (a ExternalAuthConfig) IDFieldOrDefault() string {
	if a.IDField == "" {
		return "sub"
	}
	return a.IDField
}

// PrefixOrDefault returns the tenant id prefix.
func (a ExternalAuthConfig) PrefixOrDefault() string {
	if a.TenantPrefix == "" {
		return "ext-"
	}
	return a.TenantPrefix
}

// Limits caps what a tenant may configure and consume. Zero means unlimited
// for counters; durations are Go duration strings.
type Limits struct {
	MaxAgents           int      `yaml:"max_agents" json:"max_agents"`
	MaxConnectors       int      `yaml:"max_connectors" json:"max_connectors"`
	MaxProviders        int      `yaml:"max_providers" json:"max_providers"`
	MaxSecrets          int      `yaml:"max_secrets" json:"max_secrets"`
	MaxTriggers         int      `yaml:"max_triggers" json:"max_triggers"`
	MaxConcurrentRuns   int      `yaml:"max_concurrent_runs" json:"max_concurrent_runs"`
	RunsPerMinute       int      `yaml:"runs_per_minute" json:"runs_per_minute"`
	MonthlyTokens       int64    `yaml:"monthly_tokens" json:"monthly_tokens"`
	MaxSteps            int      `yaml:"max_steps" json:"max_steps"`
	MinScheduleInterval string   `yaml:"min_schedule_interval" json:"min_schedule_interval"`
	MinPollInterval     string   `yaml:"min_poll_interval" json:"min_poll_interval"`
	SharedProviders     *bool    `yaml:"shared_providers" json:"shared_providers,omitempty"`
	ConnectorTypes      []string `yaml:"connector_types" json:"connector_types,omitempty"`
	// File storage (files.* tools, /v1/files). 0 = default, -1 = unlimited.
	MaxFiles     int `yaml:"max_files" json:"max_files"`
	MaxFileMB    int `yaml:"max_file_mb" json:"max_file_mb"`
	MaxStorageMB int `yaml:"max_storage_mb" json:"max_storage_mb"`
}

// Defaults for file storage when a plan does not set them.
const (
	DefaultMaxFiles     = 500
	DefaultMaxFileMB    = 5
	DefaultMaxStorageMB = 50
)

// FileLimits returns the file quota in files and bytes (0 = unlimited).
func (l Limits) FileLimits() (files, fileBytes, totalBytes int64) {
	pick := func(v, def int) int64 {
		switch {
		case v < 0:
			return 0
		case v == 0:
			return int64(def)
		}
		return int64(v)
	}
	return pick(l.MaxFiles, DefaultMaxFiles), pick(l.MaxFileMB, DefaultMaxFileMB) << 20, pick(l.MaxStorageMB, DefaultMaxStorageMB) << 20
}

// Hard caps for per-agent settings in tenant documents.
const (
	TenantMaxRunSeconds    = 300
	TenantMaxToolSeconds   = 60
	TenantMaxToolOutput    = 50_000
	TenantMaxParallelTools = 4
	TenantMaxHistory       = 100
	TenantMaxTokens        = 8192
	// Memory: tenants share one process and its limit, so what one poll or
	// tool call may hold in RAM is capped (see also netguard.TenantTransport).
	TenantMaxPerPoll      = 5
	TenantMaxMailBytes    = 5 << 20
	TenantMaxHTTPResponse = 1 << 20
)

// DefaultLimits are conservative limits used when no plan matches.
func DefaultLimits() Limits {
	return Limits{
		MaxAgents: 10, MaxConnectors: 10, MaxProviders: 5, MaxSecrets: 20, MaxTriggers: 20,
		MaxConcurrentRuns: 2, RunsPerMinute: 30, MonthlyTokens: 1_000_000, MaxSteps: 12,
		MinScheduleInterval: "5m", MinPollInterval: "2m",
	}
}

// PerReplica splits a per-process limit across n replicas (0 stays
// unlimited, and every replica keeps at least 1).
func PerReplica(limit, replicas int) int {
	if limit <= 0 || replicas <= 1 {
		return limit
	}
	return (limit + replicas - 1) / replicas
}

// AllowsShared reports whether shared providers may be used.
func (l Limits) AllowsShared() bool { return l.SharedProviders == nil || *l.SharedProviders }

func dur(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// MinSchedule returns the minimum schedule interval.
func (l Limits) MinSchedule() time.Duration { return dur(l.MinScheduleInterval, 0) }

// MinPoll returns the minimum IMAP poll interval.
func (l Limits) MinPoll() time.Duration { return dur(l.MinPollInterval, 0) }

// check validates a tenant document against the limits and the tenant
// security policy.
func (l Limits) check(c *Config) []string {
	var errs []string
	over := func(what string, n, max int) {
		if max > 0 && n > max {
			errs = append(errs, fmt.Sprintf("limit exceeded: %d %s (max %d)", n, what, max))
		}
	}
	over("agents", len(c.Agents), l.MaxAgents)
	over("connectors", len(c.Connectors), l.MaxConnectors)
	over("providers", len(c.Providers), l.MaxProviders)
	triggers := 0
	allowedTypes := map[string]bool{}
	for _, t := range l.ConnectorTypes {
		allowedTypes[strings.ToLower(t)] = true
	}
	for name, cc := range c.Connectors {
		if cc.Type == "shell" {
			// Never on a plan: tenants share one process.
			errs = append(errs, fmt.Sprintf("connector %q: type shell is not allowed in multi-tenant mode", name))
			continue
		}
		if len(allowedTypes) > 0 && !allowedTypes[cc.Type] {
			errs = append(errs, fmt.Sprintf("connector %q: type %q is not allowed on your plan", name, cc.Type))
		}
		var pol struct {
			Command          []string `yaml:"command"`
			AllowPrivate     bool     `yaml:"allow_private"`
			MaxResponseBytes int      `yaml:"max_response_bytes"`
		}
		_ = cc.Decode(&pol)
		if len(pol.Command) > 0 {
			errs = append(errs, fmt.Sprintf("connector %q: stdio MCP commands are not allowed; use an HTTP MCP server url", name))
		}
		if pol.AllowPrivate {
			errs = append(errs, fmt.Sprintf("connector %q: allow_private is not allowed", name))
		}
		if pol.MaxResponseBytes > TenantMaxHTTPResponse {
			errs = append(errs, fmt.Sprintf("connector %q: max_response_bytes %d exceeds limit %d", name, pol.MaxResponseBytes, TenantMaxHTTPResponse))
		}
	}
	minSched, minPoll := l.MinSchedule(), l.MinPoll()
	for name, a := range c.Agents {
		if l.MaxSteps > 0 && a.MaxSteps > l.MaxSteps {
			errs = append(errs, fmt.Sprintf("agent %q: max_steps %d exceeds limit %d", name, a.MaxSteps, l.MaxSteps))
		}
		// Tenants share one process: settings that hold a run slot or
		// memory for longer are capped so one tenant cannot starve the rest.
		for _, c := range []struct {
			field  string
			n, max int
		}{
			{"run_timeout_seconds", a.RunTimeoutSec, TenantMaxRunSeconds},
			{"tool_timeout_seconds", a.ToolTimeoutSec, TenantMaxToolSeconds},
			{"max_tool_output", a.MaxToolOutput, TenantMaxToolOutput},
			{"parallel_tools", a.ParallelTools, TenantMaxParallelTools},
			{"history_limit", a.HistoryLimit, TenantMaxHistory},
			{"max_tokens", a.MaxTokens, TenantMaxTokens},
		} {
			if c.n > c.max {
				errs = append(errs, fmt.Sprintf("agent %q: %s %d exceeds limit %d", name, c.field, c.n, c.max))
			}
		}
		for i, t := range a.Triggers {
			triggers++
			where := fmt.Sprintf("agent %q trigger %d", name, i)
			switch t.Type {
			case "cron":
				if cr, err := ParseCron(t.Schedule); err == nil && minSched > 0 {
					if gap := cr.MinGap(); gap < minSched {
						errs = append(errs, fmt.Sprintf("%s: schedule fires every %v, minimum is %v", where, gap, minSched))
					}
				}
			case "interval":
				if d, err := time.ParseDuration(t.Every); err == nil && minSched > 0 && d < minSched {
					errs = append(errs, fmt.Sprintf("%s: interval %v below minimum %v", where, d, minSched))
				}
			case "imap":
				d := dur(t.Every, time.Minute)
				if minPoll > 0 && d < minPoll {
					errs = append(errs, fmt.Sprintf("%s: poll interval %v below minimum %v", where, d, minPoll))
				}
				if t.MaxPerPoll > TenantMaxPerPoll {
					errs = append(errs, fmt.Sprintf("%s: max_per_poll %d exceeds limit %d", where, t.MaxPerPoll, TenantMaxPerPoll))
				}
			case "webhook":
				if t.Secret == "" {
					errs = append(errs, where+": webhooks need a secret")
				}
			}
		}
	}
	over("triggers", triggers, l.MaxTriggers)
	return errs
}

func (t TenancyConfig) validate(providers map[string]llm.Config) []string {
	if !t.Enabled {
		return nil
	}
	var errs []string
	switch t.Store.Type {
	case "", "memory":
	case "file":
		if t.Store.Path == "" {
			errs = append(errs, "tenancy.store.path is required for type file")
		}
	case "postgres":
		if t.Store.URL == "" {
			errs = append(errs, "tenancy.store.url is required for type postgres (built-in config: DATABASE_URL)")
		}
	default:
		errs = append(errs, "tenancy.store.type must be memory, file or postgres")
	}
	if (t.Store.Type == "postgres" || t.Store.Type == "file") && t.SecretKey == "" {
		// Checked here, before any connection, so a missing key is the one
		// clear error at startup instead of a crash after connecting.
		errs = append(errs, "tenancy.secret_key is required with a persistent store: it encrypts tenant secrets "+
			"(built-in config: RUNTIME_SECRET_KEY, create with: openssl rand -base64 32, keep it safe)")
	}
	if a := t.ExternalAuth; a.Enabled {
		if !strings.HasPrefix(a.UserinfoURL, "https://") && !strings.HasPrefix(a.UserinfoURL, "http://") {
			errs = append(errs, "tenancy.external_auth.userinfo_url must be an http(s) URL")
		}
		if p := a.PrefixOrDefault(); !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}$`).MatchString(p) {
			errs = append(errs, "tenancy.external_auth.tenant_prefix must be lowercase letters, digits or '-' (max 16)")
		}
	}
	for _, c := range t.AllowNetworks {
		if _, _, err := net.ParseCIDR(c); err != nil {
			errs = append(errs, "tenancy.allow_networks: "+err.Error())
		}
	}
	for _, p := range t.SharedProviders {
		if _, ok := providers[p]; !ok {
			errs = append(errs, fmt.Sprintf("tenancy.shared_providers: unknown provider %q", p))
		}
	}
	if t.DefaultPlan != "" && len(t.Plans) > 0 {
		if _, ok := t.Plans[t.DefaultPlan]; !ok {
			errs = append(errs, fmt.Sprintf("tenancy.default_plan %q is not defined in plans", t.DefaultPlan))
		}
	}
	for name, l := range t.Plans {
		for _, d := range []string{l.MinScheduleInterval, l.MinPollInterval} {
			if _, err := time.ParseDuration(d); d != "" && err != nil {
				errs = append(errs, fmt.Sprintf("tenancy.plans.%s: %v", name, err))
			}
		}
	}
	for _, d := range []string{t.RuntimeIdleTTL, t.SessionTTL, t.ExternalAuth.CacheTTL} {
		if _, err := time.ParseDuration(d); d != "" && err != nil {
			errs = append(errs, "tenancy: "+err.Error())
		}
	}
	return errs
}

// PlanLimits returns the limits for a plan name.
func (t TenancyConfig) PlanLimits(plan string) Limits {
	if l, ok := t.Plans[plan]; ok {
		return l
	}
	if l, ok := t.Plans[t.DefaultPlan]; ok {
		return l
	}
	return DefaultLimits()
}

// Duration helpers with defaults.
func (t TenancyConfig) IdleTTL() time.Duration       { return dur(t.RuntimeIdleTTL, 10*time.Minute) }
func (t TenancyConfig) SessionTTLDur() time.Duration { return dur(t.SessionTTL, 24*time.Hour) }
func (a ExternalAuthConfig) TTL() time.Duration      { return dur(a.CacheTTL, 5*time.Minute) }

// NotifyConfig configures owner notifications. Without a URL there are no
// alerts to owners and agents get no notify tool.
type NotifyConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
	// TokenSpikePerHour: an agent using more tokens than this in an hour is
	// reported (default 2,000,000).
	TokenSpikePerHour int64 `yaml:"token_spike_per_hour"`
	// MessagesPerDay caps notify.send per agent and replica (default 5).
	MessagesPerDay int `yaml:"messages_per_day"`
}
