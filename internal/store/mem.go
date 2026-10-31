package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
)

// Mem keeps everything in memory. With a path, tenant data (not sessions)
// is snapshotted to a JSON file after changes, so a single replica with a
// persistent volume survives restarts.
type Mem struct {
	mu       sync.RWMutex
	path     string
	data     memData
	sessions agent.Store
	runs     map[string][]Run // per tenant, oldest first; not snapshotted
	alerts   map[string]Alert // tenant/agent/kind
	dirty    chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

type memData struct {
	Tenants   map[string]*Tenant             `json:"tenants"`
	Keys      map[string]*APIKey             `json:"keys"` // by hash
	Resources map[string]map[string]Resource `json:"resources"`
	Secrets   map[string]map[string][]byte   `json:"secrets"`
	Usage     map[string]map[string]*Usage   `json:"usage"`
	Files     map[string]map[string]*memFile `json:"files,omitempty"`
}

type memFile struct {
	File
	Data []byte `json:"data"`
}

// NewMem creates an in-memory store; path may be empty.
func NewMem(path string, sessionTTL time.Duration, maxSessions int) (*Mem, error) {
	m := &Mem{
		path:     path,
		sessions: agent.NewMemoryStore(sessionTTL, maxSessions),
		runs:     map[string][]Run{},
		alerts:   map[string]Alert{},
		dirty:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		data: memData{
			Tenants: map[string]*Tenant{}, Keys: map[string]*APIKey{},
			Resources: map[string]map[string]Resource{}, Secrets: map[string]map[string][]byte{},
			Usage: map[string]map[string]*Usage{}, Files: map[string]map[string]*memFile{},
		},
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(b, &m.data); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		m.wg.Add(1)
		go m.flusher()
	}
	return m, nil
}

func (m *Mem) markDirty() {
	if m.path == "" {
		return
	}
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

func (m *Mem) flusher() {
	defer m.wg.Done()
	for {
		select {
		case <-m.dirty:
			time.Sleep(200 * time.Millisecond) // coalesce bursts
			m.flush()
		case <-m.done:
			m.flush()
			return
		}
	}
}

func (m *Mem) flush() {
	m.mu.RLock()
	b, err := json.Marshal(m.data)
	m.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.path)
	}
}

func (m *Mem) Close() error {
	if m.path != "" {
		close(m.done)
		m.wg.Wait()
	}
	return nil
}

func (m *Mem) Sessions() agent.Store       { return m.sessions }
func (m *Mem) Leader(context.Context) bool { return true }

func (m *Mem) CreateTenant(_ context.Context, t Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Tenants[t.ID]; ok {
		return ErrExists
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	m.data.Tenants[t.ID] = &t
	m.markDirty()
	return nil
}

func (m *Mem) GetTenant(_ context.Context, id string) (*Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.data.Tenants[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *t
	return &c, nil
}

func (m *Mem) UpdateTenant(_ context.Context, t Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.data.Tenants[t.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Name, cur.Plan, cur.Limits, cur.Disabled = t.Name, t.Plan, t.Limits, t.Disabled
	cur.Version++
	m.markDirty()
	return nil
}

func (m *Mem) DeleteTenant(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Tenants[id]; !ok {
		return ErrNotFound
	}
	delete(m.data.Tenants, id)
	delete(m.data.Resources, id)
	delete(m.data.Secrets, id)
	delete(m.data.Usage, id)
	delete(m.data.Files, id)
	delete(m.runs, id)
	for k, a := range m.alerts {
		if a.TenantID == id {
			delete(m.alerts, k)
		}
	}
	for h, k := range m.data.Keys {
		if k.TenantID == id {
			delete(m.data.Keys, h)
		}
	}
	// A tenant recreated with the same id (external auth does that on the
	// next sign-in) must not inherit the old conversations.
	if ms, ok := m.sessions.(*agent.MemoryStore); ok {
		ms.DeleteAgents(id + "/")
	}
	m.markDirty()
	return nil
}

func (m *Mem) ListTenants(context.Context) ([]Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Tenant, 0, len(m.data.Tenants))
	for _, t := range m.data.Tenants {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Mem) TenantVersions(context.Context) (map[string]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int64, len(m.data.Tenants))
	for id, t := range m.data.Tenants {
		out[id] = t.Version
	}
	return out, nil
}

func (m *Mem) CreateKey(_ context.Context, k APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Tenants[k.TenantID]; !ok {
		return ErrNotFound
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	k.Scope = keyScope(k.Scope)
	m.data.Keys[k.Hash] = &k
	m.markDirty()
	return nil
}

func (m *Mem) KeyByHash(_ context.Context, hash string) (*APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.data.Keys[hash]
	if !ok {
		return nil, ErrNotFound
	}
	c := *k
	c.Scope = keyScope(c.Scope)
	return &c, nil
}

func (m *Mem) ListKeys(_ context.Context, tenant string) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []APIKey
	for _, k := range m.data.Keys {
		if k.TenantID == tenant {
			c := *k
			c.Scope = keyScope(c.Scope)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Mem) DeleteKey(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, k := range m.data.Keys {
		if k.TenantID == tenant && k.ID == id {
			delete(m.data.Keys, h)
			m.markDirty()
			return nil
		}
	}
	return ErrNotFound
}

func (m *Mem) Apply(_ context.Context, tenant string, ch Change, check func(State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.data.Tenants[tenant]
	if !ok {
		return ErrNotFound
	}
	var cur []Resource
	for _, r := range m.data.Resources[tenant] {
		cur = append(cur, r)
	}
	var names []string
	for n := range m.data.Secrets[tenant] {
		names = append(names, n)
	}
	st := merge(cur, names, ch)
	if check != nil {
		if err := check(st); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	res := map[string]Resource{}
	for _, r := range st.Resources {
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = now
		}
		r.TenantID = tenant
		res[resKey(r.Kind, r.Name)] = r
	}
	m.data.Resources[tenant] = res
	sec := m.data.Secrets[tenant]
	if sec == nil {
		sec = map[string][]byte{}
		m.data.Secrets[tenant] = sec
	}
	for n, v := range ch.PutSecrets {
		sec[n] = v
	}
	for _, n := range ch.DeleteSecrets {
		delete(sec, n)
	}
	t.Version++
	m.markDirty()
	return nil
}

func (m *Mem) ListResources(_ context.Context, tenant string) ([]Resource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Resource, 0, len(m.data.Resources[tenant]))
	for _, r := range m.data.Resources[tenant] {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return resKey(out[i].Kind, out[i].Name) < resKey(out[j].Kind, out[j].Name) })
	return out, nil
}

func (m *Mem) Secrets(_ context.Context, tenant string) (map[string][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]byte{}
	for n, v := range m.data.Secrets[tenant] {
		out[n] = v
	}
	return out, nil
}

func (m *Mem) AddUsage(_ context.Context, tenant, month string, in, out, runs int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.data.Usage[tenant]
	if u == nil {
		u = map[string]*Usage{}
		m.data.Usage[tenant] = u
	}
	cur := u[month]
	if cur == nil {
		cur = &Usage{Month: month}
		u[month] = cur
	}
	cur.InputTokens += in
	cur.OutputTokens += out
	cur.Runs += runs
	m.markDirty()
	return nil
}

func (m *Mem) GetUsage(_ context.Context, tenant, month string) (Usage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if u := m.data.Usage[tenant][month]; u != nil {
		return *u, nil
	}
	return Usage{Month: month}, nil
}

// ---------- files ----------

func (m *Mem) fileUsageLocked(tenant, except string) FileUsage {
	var u FileUsage
	for p, f := range m.data.Files[tenant] {
		if p != except {
			u.Files++
			u.Bytes += f.Size
		}
	}
	return u
}

func (m *Mem) PutFile(_ context.Context, tenant, path string, data []byte, contentType string, q FileQuota) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Tenants[tenant]; !ok {
		return File{}, ErrNotFound
	}
	if err := checkQuota(q, m.fileUsageLocked(tenant, path), int64(len(data))); err != nil {
		return File{}, err
	}
	if m.data.Files == nil {
		m.data.Files = map[string]map[string]*memFile{}
	}
	if m.data.Files[tenant] == nil {
		m.data.Files[tenant] = map[string]*memFile{}
	}
	f := File{Path: path, Size: int64(len(data)), ContentType: contentType, UpdatedAt: time.Now().UTC()}
	m.data.Files[tenant][path] = &memFile{File: f, Data: append([]byte(nil), data...)}
	m.markDirty()
	return f, nil
}

func (m *Mem) GetFile(_ context.Context, tenant, path string) (File, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.data.Files[tenant][path]
	if !ok {
		return File{}, nil, ErrNotFound
	}
	return f.File, append([]byte(nil), f.Data...), nil
}

func (m *Mem) ListFiles(_ context.Context, tenant, prefix string, limit int) ([]File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []File{}
	for p, f := range m.data.Files[tenant] {
		if strings.HasPrefix(p, prefix) {
			out = append(out, f.File)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Mem) DeleteFile(_ context.Context, tenant, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Files[tenant][path]; !ok {
		return ErrNotFound
	}
	delete(m.data.Files[tenant], path)
	m.markDirty()
	return nil
}

func (m *Mem) FileUsage(_ context.Context, tenant string) (FileUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fileUsageLocked(tenant, "\x00"), nil
}

// ---------- runs ----------

// memRunsPerTenant bounds the in-memory run log; the file snapshot does not
// include it (a run log is for looking back a few days, not for keeping).
const memRunsPerTenant = 1000

func (m *Mem) RecordRun(_ context.Context, r Run) error {
	r = r.Bound()
	m.mu.Lock()
	defer m.mu.Unlock()
	l := append(m.runs[r.TenantID], r)
	if len(l) > memRunsPerTenant {
		l = append([]Run(nil), l[len(l)-memRunsPerTenant:]...)
	}
	m.runs[r.TenantID] = l
	return nil
}

func (m *Mem) ListRuns(_ context.Context, tenant string, f RunFilter) ([]Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []Run
	if f.AllTenants {
		for _, l := range m.runs {
			all = append(all, l...)
		}
	} else {
		all = append(all, m.runs[tenant]...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].StartedAt.After(all[j].StartedAt) })
	out := []Run{}
	for i := range all {
		if f.match(&all[i]) {
			r := all[i]
			if f.SessionID == "" {
				r = r.Preview()
			} else {
				r.Trace = nil
			}
			out = append(out, r)
			if len(out) == f.limit() {
				break
			}
		}
	}
	return out, nil
}

func (m *Mem) GetRun(_ context.Context, tenant, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.runs[tenant] {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Mem) PruneRuns(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for t, l := range m.runs {
		keep := l[:0]
		for _, r := range l {
			if r.StartedAt.Before(before) {
				n++
				continue
			}
			keep = append(keep, r)
		}
		if len(keep) == 0 {
			delete(m.runs, t)
		} else {
			m.runs[t] = keep
		}
	}
	return n, nil
}

func (m *Mem) RunStats(_ context.Context, tenant, agent string, since time.Time) (*RunStats, error) {
	m.mu.RLock()
	var sel []Run
	for _, r := range m.runs[tenant] {
		if !r.StartedAt.Before(since) && (agent == "" || r.Agent == agent) {
			sel = append(sel, r)
		}
	}
	m.mu.RUnlock()
	return statsOf(sel, since), nil
}

func (m *Mem) RunHealth(_ context.Context, since time.Time) ([]AgentHealth, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	by := map[string]*AgentHealth{}
	var out []AgentHealth
	for t, l := range m.runs {
		for i := len(l) - 1; i >= 0; i-- { // newest first, for LastFailedID
			r := &l[i]
			if r.StartedAt.Before(since) {
				continue
			}
			k := t + "/" + r.Agent
			if by[k] == nil {
				by[k] = &AgentHealth{TenantID: t, Agent: r.Agent}
			}
			r.health(by[k])
		}
	}
	for _, h := range by {
		out = append(out, *h)
	}
	return out, nil
}

func alertKey(tenant, agent, kind string) string { return tenant + "/" + agent + "/" + kind }

func (m *Mem) ListAlerts(_ context.Context, tenant string, all bool) ([]Alert, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Alert{}
	for _, a := range m.alerts {
		if all || a.TenantID == tenant {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out, nil
}

func (m *Mem) PutAlert(_ context.Context, a Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := alertKey(a.TenantID, a.Agent, a.Kind)
	if old, ok := m.alerts[k]; ok {
		a.Since = old.Since // an alert keeps the time it started
	}
	m.alerts[k] = a
	return nil
}

func (m *Mem) DeleteAlert(_ context.Context, tenant, agent, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.alerts, alertKey(tenant, agent, kind))
	return nil
}

func (m *Mem) ListSessions(_ context.Context, tenant string, f SessionFilter) ([]SessionSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sessionsOf(m.runs[tenant], f), nil
}
