// Package tenant runs agents of many tenants inside one process. Tenant
// definitions live in a store; a tenant's runtime (providers, connectors,
// agents) is built on first use, shared by concurrent runs, rebuilt when
// its definitions change and unloaded when idle.
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/app"
	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/llm"
	"github.com/getitdone-gmbh/agentd/internal/netguard"
	"github.com/getitdone-gmbh/agentd/internal/store"
	"github.com/getitdone-gmbh/agentd/internal/tools"
	"github.com/getitdone-gmbh/agentd/internal/triggers"
)

// System is the tenant id of the agents defined in the config file.
const System = "system"

var (
	ErrDisabled      = errors.New("tenant is disabled")
	ErrRateLimited   = fmt.Errorf("rate limit exceeded, retry later: %w", agent.ErrThrottled)
	ErrQuotaExceeded = fmt.Errorf("monthly token quota exceeded: %w", agent.ErrThrottled)
	ErrReadOnly      = errors.New("the system tenant is configured by file and read-only")
)

// ValidationError is a rejected change.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Manager owns all tenant runtimes.
type Manager struct {
	cfg    *config.Config
	st     store.Store
	box    *Box
	log    *slog.Logger
	ctx    context.Context
	system *app.App
	shared map[string]llm.Provider
	global chan struct{}
	// fileSlots bounds whole files held in memory at once (see Files.Hold).
	fileSlots chan struct{}
	sched     *triggers.Scheduler

	mu       sync.Mutex
	runtimes map[string]*runtime
	building map[string]*sync.Mutex
	limiters map[string]*bucket
	usage    map[string]*usageCounter
	tenants  map[string]cachedTenant

	external *userinfoAuth
	keys     *keyCache
	leader   atomic.Bool      // only the leader schedules triggers
	synced   map[string]int64 // tenant version last pushed to the scheduler
	tick     time.Duration
}

type cachedTenant struct {
	t       store.Tenant
	fetched time.Time
}

type runtime struct {
	app      *app.App
	version  int64
	refs     int
	lastUsed time.Time
	stale    bool
}

// New creates a manager. system may be nil.
func New(ctx context.Context, cfg *config.Config, st store.Store, system *app.App, log *slog.Logger) (*Manager, error) {
	box, ephemeral, err := NewBox(cfg.Tenancy.SecretKey)
	if err != nil {
		return nil, err
	}
	if ephemeral && cfg.Tenancy.Store.Type != "" && cfg.Tenancy.Store.Type != "memory" {
		return nil, errors.New("tenancy.secret_key is required with a persistent store: it encrypts tenant secrets. " +
			"With the built-in config set RUNTIME_SECRET_KEY (openssl rand -base64 32) and keep it safe")
	}
	if ephemeral && cfg.Tenancy.Enabled {
		log.Warn("tenancy.secret_key not set: using an ephemeral key, tenant secrets are lost on restart")
	}
	m := &Manager{
		cfg: cfg, st: st, box: box, log: log, ctx: ctx, system: system,
		shared:    map[string]llm.Provider{},
		global:    make(chan struct{}, max(cfg.Server.MaxConcurrentRuns, 1)),
		fileSlots: make(chan struct{}, maxFileTransfers),
		runtimes:  map[string]*runtime{},
		building:  map[string]*sync.Mutex{},
		limiters:  map[string]*bucket{},
		usage:     map[string]*usageCounter{},
		tenants:   map[string]cachedTenant{},
		synced:    map[string]int64{},
		keys:      newKeyCache(),
	}
	for _, name := range cfg.Tenancy.SharedProviders {
		pc := cfg.Providers[name]
		if pc.APIKey == "" && pc.BaseURL == "" && pc.Type != "ollama" {
			// A hosted provider without a key would fail every run; tenants
			// then bring their own key instead.
			log.Warn("shared provider has no api_key, not offered to tenants", "provider", name)
			continue
		}
		p, err := llm.New(name, pc)
		if err != nil {
			return nil, err
		}
		m.shared[name] = p
	}
	if cfg.Tenancy.ExternalAuth.Enabled {
		m.external = newUserinfoAuth(cfg.Tenancy.ExternalAuth)
	}
	if err := netguard.Allow(cfg.Tenancy.AllowNetworks); err != nil {
		return nil, err
	}
	m.sched = triggers.NewScheduler(m.env, cfg.Tenancy.SchedulerWorkers, log)
	return m, nil
}

// Scheduler returns the trigger scheduler.
func (m *Manager) Scheduler() *triggers.Scheduler { return m.sched }

// Store returns the backing store.
func (m *Manager) Store() store.Store { return m.st }

// SharedProviders lists the shared providers this tenant may use.
func (m *Manager) SharedProviders(t *store.Tenant) []string {
	names := []string{}
	if m.Limits(t).AllowsShared() {
		for n := range m.shared {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	return names
}

// IsLeader reports whether this replica currently runs the scheduler.
func (m *Manager) IsLeader() bool { return m.leader.Load() }

// Enabled reports whether multi-tenancy is on.
func (m *Manager) Enabled() bool { return m.cfg.Tenancy.Enabled && m.st != nil }

// ---------- tenants ----------

// Tenant returns a tenant, cached for one second to keep hot paths cheap.
func (m *Manager) Tenant(ctx context.Context, id string) (*store.Tenant, error) {
	if id == System {
		return &store.Tenant{ID: System, Name: "system", Plan: "system"}, nil
	}
	if !m.Enabled() {
		return nil, store.ErrNotFound
	}
	m.mu.Lock()
	c, ok := m.tenants[id]
	m.mu.Unlock()
	if ok && time.Since(c.fetched) < time.Second {
		t := c.t
		return &t, nil
	}
	t, err := m.st.GetTenant(ctx, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.tenants[id] = cachedTenant{t: *t, fetched: time.Now()}
	m.mu.Unlock()
	return t, nil
}

func (m *Manager) forgetTenant(id string) {
	m.mu.Lock()
	delete(m.tenants, id)
	if rt := m.runtimes[id]; rt != nil {
		rt.stale = true
		if rt.refs == 0 {
			rt.app.Close()
			delete(m.runtimes, id)
		}
	}
	m.mu.Unlock()
}

// Limits returns the effective limits of a tenant.
func (m *Manager) Limits(t *store.Tenant) config.Limits {
	if t.ID == System {
		return config.Limits{}
	}
	if t.Limits != nil {
		return *t.Limits
	}
	return m.cfg.Tenancy.PlanLimits(t.Plan)
}

var tenantIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

// CreateTenant registers a tenant.
func (m *Manager) CreateTenant(ctx context.Context, t store.Tenant) (*store.Tenant, error) {
	if !tenantIDRe.MatchString(t.ID) || t.ID == System {
		return nil, &ValidationError{"tenant id must match " + tenantIDRe.String() + " and not be \"system\""}
	}
	if t.Plan == "" {
		t.Plan = m.cfg.Tenancy.DefaultPlan
	}
	if err := m.st.CreateTenant(ctx, t); err != nil {
		return nil, err
	}
	return m.st.GetTenant(ctx, t.ID)
}

// UpdateTenant changes name, plan, limits or disabled state.
func (m *Manager) UpdateTenant(ctx context.Context, t store.Tenant) error {
	if err := m.st.UpdateTenant(ctx, t); err != nil {
		return err
	}
	m.keys.clear()
	m.forgetTenant(t.ID)
	m.resync(ctx, t.ID)
	return nil
}

// DeleteTenant removes a tenant and everything it owns.
func (m *Manager) DeleteTenant(ctx context.Context, id string) error {
	if err := m.st.DeleteTenant(ctx, id); err != nil {
		return err
	}
	m.keys.clear()
	m.forgetTenant(id)
	m.sched.Remove(id)
	return nil
}

// ---------- configuration ----------

// secretResolver only allows ${secret:NAME}; tenants never see the
// server's environment.
func secretResolver(secrets map[string]string) config.Resolver {
	return func(expr string) (string, error) {
		name, ok := strings.CutPrefix(expr, "secret:")
		if !ok {
			return "", fmt.Errorf("${%s}: only ${secret:NAME} references are allowed", expr)
		}
		v, ok := secrets[name]
		if !ok {
			return "", fmt.Errorf("secret %q is not defined", name)
		}
		return v, nil
	}
}

func buildDoc(res []store.Resource) ([]byte, error) {
	doc := map[string]map[string]json.RawMessage{"providers": {}, "connectors": {}, "agents": {}}
	for _, r := range res {
		switch r.Kind {
		case store.KindProvider:
			doc["providers"][r.Name] = r.Spec
		case store.KindConnector:
			doc["connectors"][r.Name] = r.Spec
		case store.KindAgent:
			doc["agents"][r.Name] = r.Spec
		}
	}
	return json.Marshal(doc)
}

func (m *Manager) decryptAll(tenant string, enc map[string][]byte) (map[string]string, error) {
	out := make(map[string]string, len(enc))
	for n, v := range enc {
		pt, err := m.box.Open(tenant, n, v)
		if err != nil {
			return nil, err
		}
		out[n] = string(pt)
	}
	return out, nil
}

func (m *Manager) parseTenant(t *store.Tenant, res []store.Resource, secrets map[string]string) (*config.Config, error) {
	doc, err := buildDoc(res)
	if err != nil {
		return nil, err
	}
	lim := m.Limits(t)
	var shared []string
	if lim.AllowsShared() {
		for n := range m.shared {
			shared = append(shared, n)
		}
		sort.Strings(shared)
	}
	cfg, err := config.ParseWith(doc, config.ParseOptions{Resolve: secretResolver(secrets), Tenant: true, Limits: &lim, ExtraProviders: shared})
	if err != nil {
		return nil, &ValidationError{err.Error()}
	}
	if lim.MaxSecrets > 0 && len(secrets) > lim.MaxSecrets {
		return nil, &ValidationError{fmt.Sprintf("limit exceeded: %d secrets (max %d)", len(secrets), lim.MaxSecrets)}
	}
	return cfg, nil
}

// TenantConfig loads and validates a tenant's configuration (no network).
func (m *Manager) TenantConfig(ctx context.Context, id string) (*config.Config, error) {
	cfg, _, err := m.tenantConfig(ctx, id)
	return cfg, err
}

// tenantConfig also returns the tenant the config belongs to.
func (m *Manager) tenantConfig(ctx context.Context, id string) (*config.Config, *store.Tenant, error) {
	if id == System {
		if m.system == nil {
			return &config.Config{}, &store.Tenant{ID: System}, nil
		}
		return m.system.Config, &store.Tenant{ID: System}, nil
	}
	t, err := m.st.GetTenant(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	res, err := m.st.ListResources(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	enc, err := m.st.Secrets(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := m.decryptAll(id, enc)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := m.parseTenant(t, res, secrets)
	return cfg, t, err
}

// Resources lists a tenant's stored definitions.
func (m *Manager) Resources(ctx context.Context, id string) ([]store.Resource, error) {
	if id == System {
		return nil, ErrReadOnly
	}
	return m.st.ListResources(ctx, id)
}

// SecretNames lists a tenant's secret names (values are never returned).
func (m *Manager) SecretNames(ctx context.Context, id string) ([]string, error) {
	if id == System {
		return nil, ErrReadOnly
	}
	enc, err := m.st.Secrets(ctx, id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(enc))
	for n := range enc {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Mutation describes a change to a tenant's definitions.
type Mutation struct {
	Put           []store.Resource
	Delete        []store.Resource
	PutSecrets    map[string]string
	DeleteSecrets []string
	ReplaceAll    bool
}

// Apply validates and stores a change atomically, then refreshes the
// tenant's runtime and triggers.
func (m *Manager) Apply(ctx context.Context, id string, mu Mutation) error {
	if id == System {
		return ErrReadOnly
	}
	t, err := m.st.GetTenant(ctx, id)
	if err != nil {
		return err
	}
	for _, r := range mu.Put {
		if !json.Valid(r.Spec) {
			return &ValidationError{fmt.Sprintf("%s %q: spec is not valid JSON", r.Kind, r.Name)}
		}
	}
	enc := map[string][]byte{}
	for n, v := range mu.PutSecrets {
		if !secretNameRe.MatchString(n) {
			return &ValidationError{fmt.Sprintf("secret name %q must match %s", n, secretNameRe)}
		}
		enc[n] = m.box.Seal(id, n, []byte(v))
	}
	cur, err := m.st.Secrets(ctx, id)
	if err != nil {
		return err
	}
	secrets, err := m.decryptAll(id, cur)
	if err != nil {
		return err
	}
	for n, v := range mu.PutSecrets {
		secrets[n] = v
	}
	for _, n := range mu.DeleteSecrets {
		delete(secrets, n)
	}
	ch := store.Change{Put: mu.Put, Delete: mu.Delete, PutSecrets: enc, DeleteSecrets: mu.DeleteSecrets, ReplaceAll: mu.ReplaceAll}
	err = m.st.Apply(ctx, id, ch, func(st store.State) error {
		_, err := m.parseTenant(t, st.Resources, secrets)
		return err
	})
	if err != nil {
		return err
	}
	m.forgetTenant(id)
	m.resync(ctx, id)
	return nil
}

// resync updates the scheduler with a tenant's triggers. Non-leaders do
// nothing; the leader picks changes up via tenant versions.
func (m *Manager) resync(ctx context.Context, id string) {
	if !m.leader.Load() {
		return
	}
	cfg, t, err := m.tenantConfig(ctx, id)
	if err != nil {
		m.sched.Remove(id)
		m.setSynced(id, -1)
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("tenant config invalid, triggers disabled", "tenant", id, "err", err)
		}
		return
	}
	if t.Disabled {
		m.sched.Remove(id)
	} else {
		m.sched.Sync(id, cfg.Agents)
	}
	m.setSynced(id, t.Version)
}

func (m *Manager) setSynced(id string, v int64) {
	m.mu.Lock()
	if v < 0 {
		delete(m.synced, id)
	} else {
		m.synced[id] = v
	}
	m.mu.Unlock()
}

func (m *Manager) isSynced(id string, v int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sv, ok := m.synced[id]
	return ok && sv == v
}

// ---------- runtimes ----------

// Acquire returns a tenant's runtime; call release when done.
func (m *Manager) Acquire(ctx context.Context, id string) (*app.App, func(), error) {
	if id == System {
		if m.system == nil {
			return nil, nil, store.ErrNotFound
		}
		return m.system, func() {}, nil
	}
	t, err := m.Tenant(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if t.Disabled {
		return nil, nil, ErrDisabled
	}
	if a, rel := m.lookup(id, t.Version); a != nil {
		return a, rel, nil
	}
	// One build per tenant at a time; others wait and reuse it.
	m.mu.Lock()
	bmu := m.building[id]
	if bmu == nil {
		bmu = &sync.Mutex{}
		m.building[id] = bmu
	}
	m.mu.Unlock()
	bmu.Lock()
	defer bmu.Unlock()
	if a, rel := m.lookup(id, t.Version); a != nil {
		return a, rel, nil
	}
	start := time.Now()
	cfg, err := m.TenantConfig(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	lim := m.Limits(t)
	shared := map[string]llm.Provider{}
	if lim.AllowsShared() {
		shared = m.shared
	}
	a, err := app.BuildWith(m.ctx, cfg, m.log.With("tenant", id), app.Options{
		Guarded:            true,
		Shared:             shared,
		Store:              agent.Prefixed(m.st.Sessions(), id+"/"),
		MaxConcurrentRuns:  max(config.PerReplica(lim.MaxConcurrentRuns, m.cfg.Tenancy.Replicas), 1),
		Files:              m.filesFor(t),
		ConnectorsOptional: true,
		MaxSteps:           lim.MaxSteps,
	})
	if err != nil {
		return nil, nil, err
	}
	m.log.Debug("tenant runtime built", "tenant", id, "version", t.Version, "agents", len(a.Agents), "ms", time.Since(start).Milliseconds())
	m.mu.Lock()
	if old := m.runtimes[id]; old != nil {
		old.stale = true
		if old.refs == 0 {
			old.app.Close()
		}
	}
	rt := &runtime{app: a, version: t.Version, refs: 1, lastUsed: time.Now()}
	m.runtimes[id] = rt
	m.evictLocked()
	m.mu.Unlock()
	return a, m.releaser(id, rt), nil
}

func (m *Manager) lookup(id string, version int64) (*app.App, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil || rt.stale || rt.version != version {
		return nil, nil
	}
	rt.refs++
	rt.lastUsed = time.Now()
	return rt.app, m.releaser(id, rt)
}

func (m *Manager) releaser(id string, rt *runtime) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			rt.refs--
			rt.lastUsed = time.Now()
			if rt.stale && rt.refs == 0 {
				rt.app.Close()
				if m.runtimes[id] == rt {
					delete(m.runtimes, id)
				}
			}
		})
	}
}

// evictLocked unloads least recently used idle runtimes over the cap.
func (m *Manager) evictLocked() {
	limit := m.cfg.Tenancy.MaxCachedRuntimes
	if limit <= 0 {
		limit = 10000
	}
	if len(m.runtimes) <= limit {
		return
	}
	type cand struct {
		id string
		t  time.Time
	}
	var idle []cand
	for id, rt := range m.runtimes {
		if rt.refs == 0 {
			idle = append(idle, cand{id, rt.lastUsed})
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].t.Before(idle[j].t) })
	for _, c := range idle {
		if len(m.runtimes) <= limit {
			break
		}
		m.runtimes[c.id].app.Close()
		delete(m.runtimes, c.id)
	}
}

// Janitor unloads idle runtimes and resyncs triggers changed by other
// replicas. It runs until ctx ends.
func (m *Manager) Janitor(ctx context.Context) {
	if m.tick <= 0 {
		m.tick = 30 * time.Second
	}
	t := time.NewTicker(m.tick)
	defer t.Stop()
	versions := map[string]int64{}
	leader := false
	for {
		m.sweepIdle()
		if m.Enabled() {
			isLeader := m.st.Leader(ctx)
			switch {
			case isLeader && !leader:
				m.log.Info("scheduler leadership acquired")
				versions = map[string]int64{}
				if m.system != nil {
					m.sched.Sync(System, m.system.Config.Agents)
				}
			case !isLeader && leader:
				m.log.Warn("scheduler leadership lost")
				m.sched.Clear()
				m.mu.Lock()
				m.synced = map[string]int64{}
				m.mu.Unlock()
			}
			leader = isLeader
			m.leader.Store(isLeader)
			if leader {
				m.syncChanged(ctx, versions)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) syncChanged(ctx context.Context, known map[string]int64) {
	cur, err := m.st.TenantVersions(ctx)
	if err != nil {
		m.log.Warn("listing tenant versions failed", "err", err)
		return
	}
	for id, v := range cur {
		if old, ok := known[id]; ok && old == v {
			continue
		}
		known[id] = v
		if m.isSynced(id, v) {
			continue // already pushed by a local change
		}
		m.forgetTenantCache(id)
		m.resync(ctx, id)
	}
	for id := range known {
		if _, ok := cur[id]; !ok {
			delete(known, id)
			m.sched.Remove(id)
			m.setSynced(id, -1)
		}
	}
}

func (m *Manager) forgetTenantCache(id string) {
	m.mu.Lock()
	delete(m.tenants, id)
	m.mu.Unlock()
}

func (m *Manager) sweepIdle() {
	ttl := m.cfg.Tenancy.IdleTTL()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rt := range m.runtimes {
		if rt.refs == 0 && time.Since(rt.lastUsed) > ttl {
			rt.app.Close()
			delete(m.runtimes, id)
		}
	}
	for id, l := range m.limiters {
		if l.idle() {
			delete(m.limiters, id)
		}
	}
}

// Stats reports runtime counters.
func (m *Manager) Stats() map[string]any {
	m.mu.Lock()
	loaded, busy := len(m.runtimes), 0
	for _, rt := range m.runtimes {
		if rt.refs > 0 {
			busy++
		}
	}
	m.mu.Unlock()
	return map[string]any{"runtimes_loaded": loaded, "runtimes_busy": busy, "scheduler": m.sched.Stats(), "global_running": len(m.global)}
}

// ---------- running ----------

// Run executes an agent of a tenant, enforcing its limits.
func (m *Manager) Run(ctx context.Context, tenantID, agentName string, in agent.RunInput) (*agent.RunResult, error) {
	t, err := m.Tenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if t.Disabled {
		return nil, ErrDisabled
	}
	lim := m.Limits(t)
	if tenantID != System {
		if !m.allow(tenantID, config.PerReplica(lim.RunsPerMinute, m.cfg.Tenancy.Replicas)) {
			return nil, ErrRateLimited
		}
		if lim.MonthlyTokens > 0 {
			used, err := m.usedTokens(ctx, tenantID)
			if err != nil {
				return nil, err
			}
			if used >= lim.MonthlyTokens {
				return nil, ErrQuotaExceeded
			}
		}
	}
	a, release, err := m.Acquire(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer release()
	var gate func(context.Context) (func(), error)
	if tenantID != System {
		// The tenant's own slot is taken first (inside RunThen), the shared
		// one only after it: a run waiting on its tenant's cap holds nothing
		// other tenants need.
		gate = func(ctx context.Context) (func(), error) {
			select {
			case m.global <- struct{}{}:
				return func() { <-m.global }, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	res, err := a.RunThen(ctx, agentName, in, gate)
	if res != nil && tenantID != System {
		m.recordUsage(tenantID, res.Usage)
	}
	return res, err
}

// env adapts a tenant for the scheduler.
func (m *Manager) env(tenantID string) triggers.Env { return &tenantEnv{m: m, id: tenantID} }

type tenantEnv struct {
	m  *Manager
	id string
}

func (e *tenantEnv) Run(ctx context.Context, agentName string, in agent.RunInput) (*agent.RunResult, error) {
	return e.m.Run(ctx, e.id, agentName, in)
}

func (e *tenantEnv) Email(ctx context.Context, name string) (*tools.EmailConnector, func(), error) {
	a, release, err := e.m.Acquire(ctx, e.id)
	if err != nil {
		return nil, nil, err
	}
	c, ok := a.Connectors[name].(*tools.EmailConnector)
	if !ok {
		release()
		return nil, nil, fmt.Errorf("connector %q is not an available email connector", name)
	}
	return c, release, nil
}

// Start syncs all triggers and launches the scheduler and janitor.
func (m *Manager) Start(ctx context.Context, wg *sync.WaitGroup) {
	if m.system != nil && !m.Enabled() {
		// Single-config mode: no coordination needed. With tenancy the
		// janitor syncs everything once this replica is leader.
		m.sched.Sync(System, m.system.Config.Agents)
	}
	if !m.Enabled() {
		m.leader.Store(true)
	}
	// Leadership is decided before serving so early changes are scheduled;
	// the janitor then syncs all tenants on its first tick.
	if m.Enabled() && m.st.Leader(ctx) {
		m.leader.Store(true)
	}
	wg.Add(2)
	go func() { defer wg.Done(); m.sched.Run(ctx) }()
	go func() { defer wg.Done(); m.Janitor(ctx) }()
}
