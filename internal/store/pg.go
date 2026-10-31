package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/llm"
)

// Postgres stores everything in PostgreSQL, so several replicas can share
// the load. One replica at a time is leader (advisory lock) and runs the
// schedulers.
type Postgres struct {
	db         *sql.DB
	sessionTTL time.Duration
	sessions   *pgSessions

	lockMu sync.Mutex
	lockTx *sql.Tx
}

const leaderLockID = 0x41525254 // "ARRT"

const schema = `
CREATE TABLE IF NOT EXISTS ar_tenants (
  id text PRIMARY KEY,
  name text NOT NULL DEFAULT '',
  plan text NOT NULL DEFAULT '',
  limits jsonb,
  disabled boolean NOT NULL DEFAULT false,
  version bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS ar_keys (
  id text PRIMARY KEY,
  tenant_id text NOT NULL REFERENCES ar_tenants(id) ON DELETE CASCADE,
  name text NOT NULL DEFAULT '',
  hash text NOT NULL UNIQUE,
  prefix text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ar_keys_tenant ON ar_keys(tenant_id);
ALTER TABLE ar_keys ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'admin';
ALTER TABLE ar_keys ADD COLUMN IF NOT EXISTS agents text[] NOT NULL DEFAULT '{}';
CREATE TABLE IF NOT EXISTS ar_resources (
  tenant_id text NOT NULL REFERENCES ar_tenants(id) ON DELETE CASCADE,
  kind text NOT NULL,
  name text NOT NULL,
  spec jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, kind, name)
);
CREATE TABLE IF NOT EXISTS ar_secrets (
  tenant_id text NOT NULL REFERENCES ar_tenants(id) ON DELETE CASCADE,
  name text NOT NULL,
  value bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, name)
);
CREATE TABLE IF NOT EXISTS ar_usage (
  tenant_id text NOT NULL REFERENCES ar_tenants(id) ON DELETE CASCADE,
  month text NOT NULL,
  input_tokens bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  runs bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, month)
);
CREATE TABLE IF NOT EXISTS ar_sessions (
  key text PRIMARY KEY,
  data bytea NOT NULL,
  expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS ar_sessions_expires ON ar_sessions(expires_at);
CREATE TABLE IF NOT EXISTS ar_session_leases (
  key text PRIMARY KEY,
  holder text NOT NULL,
  expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS ar_files (
  tenant_id text NOT NULL REFERENCES ar_tenants(id) ON DELETE CASCADE,
  path text NOT NULL,
  content_type text NOT NULL DEFAULT '',
  size bigint NOT NULL,
  data bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, path)
);
CREATE TABLE IF NOT EXISTS ar_runs (
  id text PRIMARY KEY,
  tenant_id text NOT NULL,
  agent text NOT NULL,
  source text NOT NULL DEFAULT '',
  session_id text NOT NULL DEFAULT '',
  status text NOT NULL,
  error text NOT NULL DEFAULT '',
  input text NOT NULL DEFAULT '',
  images int NOT NULL DEFAULT 0,
  output text NOT NULL DEFAULT '',
  trace jsonb,
  steps int NOT NULL DEFAULT 0,
  tool_calls int NOT NULL DEFAULT 0,
  tool_errors int NOT NULL DEFAULT 0,
  input_tokens int NOT NULL DEFAULT 0,
  output_tokens int NOT NULL DEFAULT 0,
  duration_ms bigint NOT NULL DEFAULT 0,
  started_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS ar_runs_tenant ON ar_runs(tenant_id, started_at DESC);
CREATE INDEX IF NOT EXISTS ar_runs_session ON ar_runs(tenant_id, session_id);
CREATE INDEX IF NOT EXISTS ar_runs_started ON ar_runs(started_at);
ALTER TABLE ar_runs ADD COLUMN IF NOT EXISTS error_kind text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS ar_alerts (
  tenant_id text NOT NULL,
  agent text NOT NULL,
  kind text NOT NULL,
  severity text NOT NULL DEFAULT '',
  summary text NOT NULL DEFAULT '',
  run_id text NOT NULL DEFAULT '',
  since timestamptz NOT NULL,
  last_seen timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, agent, kind)
);
`

// NewPostgres connects and migrates the schema.
func NewPostgres(ctx context.Context, url string, sessionTTL time.Duration) (*Postgres, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}
	p := &Postgres{db: db, sessionTTL: sessionTTL}
	p.sessions = &pgSessions{db: db, ttl: sessionTTL}
	return p, nil
}

func (p *Postgres) Close() error {
	p.lockMu.Lock()
	if p.lockTx != nil {
		_ = p.lockTx.Rollback()
		p.lockTx = nil
	}
	p.lockMu.Unlock()
	return p.db.Close()
}

func (p *Postgres) Sessions() agent.Store { return p.sessions }

// Leader holds a transaction-level advisory lock inside a transaction that
// stays open while this replica leads. Transaction-scoped (not session)
// locks keep working behind PgBouncer in transaction mode, where a session
// lock would stay on a pooled server connection and leak to other clients.
func (p *Postgres) Leader(ctx context.Context) bool {
	p.lockMu.Lock()
	defer p.lockMu.Unlock()
	// The caller's ctx may be a request context; a cancelled check must not
	// cost the leadership, so it uses its own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if p.lockTx != nil {
		if _, err := p.lockTx.ExecContext(ctx, `SELECT 1`); err == nil {
			return true
		}
		_ = p.lockTx.Rollback()
		p.lockTx = nil
	}
	// BeginTx's ctx would roll the transaction back when cancelled, so the
	// transaction lives on a background context.
	tx, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, leaderLockID).Scan(&ok); err != nil || !ok {
		_ = tx.Rollback()
		return false
	}
	p.lockTx = tx
	return true
}

func isUnique(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

func isFK(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (p *Postgres) CreateTenant(ctx context.Context, t Tenant) error {
	lim, _ := json.Marshal(t.Limits)
	_, err := p.db.ExecContext(ctx, `INSERT INTO ar_tenants (id, name, plan, limits, disabled) VALUES ($1,$2,$3,$4,$5)`,
		t.ID, t.Name, t.Plan, nullJSON(lim), t.Disabled)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func nullJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

const tenantCols = `id, name, plan, limits, disabled, version, created_at`

func scanTenant(sc interface{ Scan(...any) error }) (*Tenant, error) {
	var t Tenant
	var lim []byte
	if err := sc.Scan(&t.ID, &t.Name, &t.Plan, &lim, &t.Disabled, &t.Version, &t.CreatedAt); err != nil {
		return nil, err
	}
	if len(lim) > 0 {
		var l config.Limits
		if json.Unmarshal(lim, &l) == nil {
			t.Limits = &l
		}
	}
	return &t, nil
}

func (p *Postgres) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	t, err := scanTenant(p.db.QueryRowContext(ctx, `SELECT `+tenantCols+` FROM ar_tenants WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (p *Postgres) UpdateTenant(ctx context.Context, t Tenant) error {
	lim, _ := json.Marshal(t.Limits)
	res, err := p.db.ExecContext(ctx, `UPDATE ar_tenants SET name=$2, plan=$3, limits=$4, disabled=$5, version=version+1 WHERE id=$1`,
		t.ID, t.Name, t.Plan, nullJSON(lim), t.Disabled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteTenant(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM ar_tenants WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err = p.db.ExecContext(ctx, `DELETE FROM ar_sessions WHERE key LIKE $1`, escapeLike(id+"/")+"%"); err != nil {
		return err
	}
	// ar_runs has no foreign key: the system tenant records runs too and
	// has no row in ar_tenants.
	if _, err = p.db.ExecContext(ctx, `DELETE FROM ar_runs WHERE tenant_id=$1`, id); err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `DELETE FROM ar_alerts WHERE tenant_id=$1`, id)
	return err
}

func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

func (p *Postgres) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT `+tenantCols+` FROM ar_tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (p *Postgres) TenantVersions(ctx context.Context) (map[string]int64, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id, version FROM ar_tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var v int64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

func (p *Postgres) CreateKey(ctx context.Context, k APIKey) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO ar_keys (id, tenant_id, name, hash, prefix, scope, agents) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		k.ID, k.TenantID, k.Name, k.Hash, k.Prefix, keyScope(k.Scope), pq.Array(nonNil(k.Agents)))
	if isFK(err) {
		return ErrNotFound
	}
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (p *Postgres) KeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	var k APIKey
	err := p.db.QueryRowContext(ctx, `SELECT id, tenant_id, name, hash, prefix, scope, agents, created_at FROM ar_keys WHERE hash=$1`, hash).
		Scan(&k.ID, &k.TenantID, &k.Name, &k.Hash, &k.Prefix, &k.Scope, pq.Array(&k.Agents), &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

func (p *Postgres) ListKeys(ctx context.Context, tenant string) ([]APIKey, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id, tenant_id, name, hash, prefix, scope, agents, created_at FROM ar_keys WHERE tenant_id=$1 ORDER BY created_at`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Name, &k.Hash, &k.Prefix, &k.Scope, pq.Array(&k.Agents), &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteKey(ctx context.Context, tenant, id string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM ar_keys WHERE tenant_id=$1 AND id=$2`, tenant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Apply(ctx context.Context, tenant string, ch Change, check func(State) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int64
	// Row lock serializes concurrent changes of one tenant.
	if err := tx.QueryRowContext(ctx, `SELECT version FROM ar_tenants WHERE id=$1 FOR UPDATE`, tenant).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	cur, err := listResources(ctx, tx, tenant)
	if err != nil {
		return err
	}
	var names []string
	rows, err := tx.QueryContext(ctx, `SELECT name FROM ar_secrets WHERE tenant_id=$1`, tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	st := merge(cur, names, ch)
	if check != nil {
		if err := check(st); err != nil {
			return err
		}
	}
	if ch.ReplaceAll {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ar_resources WHERE tenant_id=$1`, tenant); err != nil {
			return err
		}
	}
	for _, r := range ch.Delete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ar_resources WHERE tenant_id=$1 AND kind=$2 AND name=$3`, tenant, r.Kind, r.Name); err != nil {
			return err
		}
	}
	for _, r := range ch.Put {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ar_resources (tenant_id, kind, name, spec, updated_at) VALUES ($1,$2,$3,$4,now())
			ON CONFLICT (tenant_id, kind, name) DO UPDATE SET spec=EXCLUDED.spec, updated_at=now()`, tenant, r.Kind, r.Name, string(r.Spec)); err != nil {
			return err
		}
	}
	for n, val := range ch.PutSecrets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ar_secrets (tenant_id, name, value, updated_at) VALUES ($1,$2,$3,now())
			ON CONFLICT (tenant_id, name) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, tenant, n, val); err != nil {
			return err
		}
	}
	for _, n := range ch.DeleteSecrets {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ar_secrets WHERE tenant_id=$1 AND name=$2`, tenant, n); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ar_tenants SET version=version+1 WHERE id=$1`, tenant); err != nil {
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func listResources(ctx context.Context, q querier, tenant string) ([]Resource, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, name, spec, updated_at FROM ar_resources WHERE tenant_id=$1 ORDER BY kind, name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r := Resource{TenantID: tenant}
		var spec []byte
		if err := rows.Scan(&r.Kind, &r.Name, &spec, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Spec = spec
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ListResources(ctx context.Context, tenant string) ([]Resource, error) {
	return listResources(ctx, p.db, tenant)
}

func (p *Postgres) Secrets(ctx context.Context, tenant string) (map[string][]byte, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT name, value FROM ar_secrets WHERE tenant_id=$1`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var n string
		var v []byte
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

func (p *Postgres) AddUsage(ctx context.Context, tenant, month string, in, out, runs int64) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO ar_usage (tenant_id, month, input_tokens, output_tokens, runs) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, month) DO UPDATE SET input_tokens=ar_usage.input_tokens+EXCLUDED.input_tokens,
		output_tokens=ar_usage.output_tokens+EXCLUDED.output_tokens, runs=ar_usage.runs+EXCLUDED.runs`, tenant, month, in, out, runs)
	return err
}

func (p *Postgres) GetUsage(ctx context.Context, tenant, month string) (Usage, error) {
	u := Usage{Month: month}
	err := p.db.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, runs FROM ar_usage WHERE tenant_id=$1 AND month=$2`, tenant, month).
		Scan(&u.InputTokens, &u.OutputTokens, &u.Runs)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	return u, err
}

// pgSessions implements agent.Store in the ar_sessions table.
type pgSessions struct {
	db    *sql.DB
	ttl   time.Duration
	saves atomic.Int64
}

func sessionKey(agentName, session string) string { return agentName + "|" + session }

func (s *pgSessions) Load(ctx context.Context, agentName, session string) ([]llm.Message, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM ar_sessions WHERE key=$1 AND expires_at > now()`, sessionKey(agentName, session)).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	return msgs, json.Unmarshal(data, &msgs)
}

func (s *pgSessions) Save(ctx context.Context, agentName, session string, msgs []llm.Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ar_sessions (key, data, expires_at) VALUES ($1,$2,$3)
		ON CONFLICT (key) DO UPDATE SET data=EXCLUDED.data, expires_at=EXCLUDED.expires_at`,
		sessionKey(agentName, session), data, time.Now().Add(s.ttl))
	if err == nil && s.saves.Add(1)%200 == 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM ar_sessions WHERE expires_at < now()`)
	}
	return err
}

// Session leases: how long one lasts without renewal, how often the holder
// renews it, and how often a waiting replica asks again.
var (
	leaseTTL   = 90 * time.Second
	leaseRenew = 30 * time.Second
	leasePoll  = time.Second
)

// LockSession serializes runs of one session across replicas (otherwise the
// second run would overwrite the first one's turn). It takes a lease row
// instead of holding a transaction open for the whole run: a run lasts
// minutes, mostly waiting on the model, and an open transaction per run
// pinned a pooled connection each until auth and usage writes of every
// tenant stalled. The holder renews the lease while it runs; if the replica
// dies, the lease simply expires. All times are the database's, so replica
// clocks do not matter.
func (s *pgSessions) LockSession(ctx context.Context, agentName, session string) (func(), error) {
	key := sessionKey(agentName, session)
	holder := newHolderID()
	for wait := 100 * time.Millisecond; ; wait = min(wait*2, leasePoll) {
		var got string
		err := s.db.QueryRowContext(ctx, `INSERT INTO ar_session_leases (key, holder, expires_at)
			VALUES ($1, $2, now() + $3 * interval '1 millisecond')
			ON CONFLICT (key) DO UPDATE SET holder=EXCLUDED.holder, expires_at=EXCLUDED.expires_at
			WHERE ar_session_leases.expires_at < now()
			RETURNING holder`, key, holder, leaseTTL.Milliseconds()).Scan(&got)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		select { // held by another run: ask again
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(leaseRenew)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_, _ = s.db.Exec(`UPDATE ar_session_leases SET expires_at = now() + $3 * interval '1 millisecond'
					WHERE key=$1 AND holder=$2`, key, holder, leaseTTL.Milliseconds())
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		_, _ = s.db.Exec(`DELETE FROM ar_session_leases WHERE key=$1 AND holder=$2`, key, holder)
	}, nil
}

func newHolderID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *pgSessions) Delete(ctx context.Context, agentName, session string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ar_sessions WHERE key=$1`, sessionKey(agentName, session))
	return err
}

// ---------- files ----------

// PutFile writes a file in one transaction that locks the tenant row, so
// concurrent writes of one tenant (from any replica) cannot both pass the
// quota check.
func (p *Postgres) PutFile(ctx context.Context, tenant, path string, data []byte, contentType string, q FileQuota) (File, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return File{}, err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM ar_tenants WHERE id=$1 FOR UPDATE`, tenant).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	var others FileUsage
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size),0) FROM ar_files WHERE tenant_id=$1 AND path<>$2`,
		tenant, path).Scan(&others.Files, &others.Bytes); err != nil {
		return File{}, err
	}
	if err := checkQuota(q, others, int64(len(data))); err != nil {
		return File{}, err
	}
	f := File{Path: path, Size: int64(len(data)), ContentType: contentType}
	if err := tx.QueryRowContext(ctx, `INSERT INTO ar_files (tenant_id, path, content_type, size, data) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, path) DO UPDATE SET content_type=EXCLUDED.content_type, size=EXCLUDED.size, data=EXCLUDED.data, updated_at=now()
		RETURNING updated_at`, tenant, path, contentType, f.Size, data).Scan(&f.UpdatedAt); err != nil {
		return File{}, err
	}
	return f, tx.Commit()
}

func (p *Postgres) GetFile(ctx context.Context, tenant, path string) (File, []byte, error) {
	f := File{Path: path}
	var data []byte
	err := p.db.QueryRowContext(ctx, `SELECT content_type, size, updated_at, data FROM ar_files WHERE tenant_id=$1 AND path=$2`, tenant, path).
		Scan(&f.ContentType, &f.Size, &f.UpdatedAt, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, nil, ErrNotFound
	}
	return f, data, err
}

func (p *Postgres) ListFiles(ctx context.Context, tenant, prefix string, limit int) ([]File, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := p.db.QueryContext(ctx, `SELECT path, content_type, size, updated_at FROM ar_files
		WHERE tenant_id=$1 AND path LIKE $2 ORDER BY path LIMIT $3`, tenant, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.ContentType, &f.Size, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteFile(ctx context.Context, tenant, path string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM ar_files WHERE tenant_id=$1 AND path=$2`, tenant, path)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) FileUsage(ctx context.Context, tenant string) (FileUsage, error) {
	var u FileUsage
	err := p.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size),0) FROM ar_files WHERE tenant_id=$1`, tenant).Scan(&u.Files, &u.Bytes)
	return u, err
}

// ---------- runs ----------

func (p *Postgres) RecordRun(ctx context.Context, r Run) error {
	r = r.Bound()
	trace, err := json.Marshal(r.Trace)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `INSERT INTO ar_runs (id, tenant_id, agent, source, session_id, status, error, input, images, output, trace,
		steps, tool_calls, tool_errors, input_tokens, output_tokens, duration_ms, started_at, error_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		r.ID, r.TenantID, r.Agent, r.Source, r.SessionID, r.Status, r.Error, r.Input, r.Images, r.Output, trace,
		r.Steps, r.ToolCalls, r.ToolErrors, r.InputTokens, r.OutputTokens, r.DurationMS, r.StartedAt, r.ErrorKind)
	return err
}

const runColumns = `id, tenant_id, agent, source, session_id, status, error, %s, images, %s,
	steps, tool_calls, tool_errors, input_tokens, output_tokens, duration_ms, started_at, error_kind`

func scanRun(sc interface{ Scan(...any) error }, r *Run, extra ...any) error {
	return sc.Scan(append([]any{&r.ID, &r.TenantID, &r.Agent, &r.Source, &r.SessionID, &r.Status, &r.Error, &r.Input, &r.Images, &r.Output,
		&r.Steps, &r.ToolCalls, &r.ToolErrors, &r.InputTokens, &r.OutputTokens, &r.DurationMS, &r.StartedAt, &r.ErrorKind}, extra...)...)
}

func (p *Postgres) ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error) {
	cols := fmt.Sprintf(runColumns, fmt.Sprintf("left(input, %d)", previewLen+1), fmt.Sprintf("left(output, %d)", previewLen+1))
	if f.SessionID != "" {
		cols = fmt.Sprintf(runColumns, "input", "output")
	}
	q := `SELECT ` + cols + ` FROM ar_runs WHERE true`
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if !f.AllTenants {
		q += ` AND tenant_id=` + arg(tenant)
	}
	if f.Agent != "" {
		q += ` AND agent=` + arg(f.Agent)
	}
	if f.SessionID != "" {
		q += ` AND session_id=` + arg(f.SessionID)
	}
	switch f.Status {
	case "":
	case "failed":
		q += ` AND status<>'ok'`
	default:
		q += ` AND status=` + arg(f.Status)
	}
	if !f.Before.IsZero() {
		q += ` AND started_at<` + arg(f.Before)
	}
	q += ` ORDER BY started_at DESC LIMIT ` + arg(f.limit())
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		if err := scanRun(rows, &r); err != nil {
			return nil, err
		}
		if f.SessionID == "" {
			r = r.Preview()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) GetRun(ctx context.Context, tenant, id string) (*Run, error) {
	var r Run
	var trace []byte
	row := p.db.QueryRowContext(ctx, `SELECT `+fmt.Sprintf(runColumns, "input", "output")+`, trace FROM ar_runs WHERE tenant_id=$1 AND id=$2`, tenant, id)
	if err := scanRun(row, &r, &trace); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if len(trace) > 0 {
		if err := json.Unmarshal(trace, &r.Trace); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

func (p *Postgres) PruneRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := p.db.ExecContext(ctx, `DELETE FROM ar_runs WHERE started_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// maxStatRuns bounds how many runs one stats request reads. Aggregating in
// Go keeps both backends identical; a tenant with more runs than this in
// the window gets the newest ones.
const maxStatRuns = 200_000

// statRuns reads the numeric columns of runs since t, newest first.
func (p *Postgres) statRuns(ctx context.Context, where string, args ...any) ([]Run, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id, tenant_id, agent, status, left(error, 500), error_kind, tool_calls, tool_errors,
		input_tokens, output_tokens, duration_ms, started_at FROM ar_runs WHERE `+where+
		fmt.Sprintf(` ORDER BY started_at DESC LIMIT %d`, maxStatRuns), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Agent, &r.Status, &r.Error, &r.ErrorKind, &r.ToolCalls, &r.ToolErrors,
			&r.InputTokens, &r.OutputTokens, &r.DurationMS, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) RunStats(ctx context.Context, tenant, agent string, since time.Time) (*RunStats, error) {
	where, args := `tenant_id=$1 AND started_at >= $2`, []any{tenant, since}
	if agent != "" {
		where, args = where+` AND agent=$3`, append(args, agent)
	}
	runs, err := p.statRuns(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	return statsOf(runs, since), nil
}

func (p *Postgres) RunHealth(ctx context.Context, since time.Time) ([]AgentHealth, error) {
	runs, err := p.statRuns(ctx, `started_at >= $1`, since)
	if err != nil {
		return nil, err
	}
	by := map[string]*AgentHealth{}
	var out []AgentHealth
	for i := range runs { // newest first, for LastFailedID
		r := &runs[i]
		k := r.TenantID + "/" + r.Agent
		if by[k] == nil {
			by[k] = &AgentHealth{TenantID: r.TenantID, Agent: r.Agent}
		}
		r.health(by[k])
	}
	for _, h := range by {
		out = append(out, *h)
	}
	return out, nil
}

func (p *Postgres) ListAlerts(ctx context.Context, tenant string, all bool) ([]Alert, error) {
	q := `SELECT tenant_id, agent, kind, severity, summary, run_id, since, last_seen FROM ar_alerts`
	var args []any
	if !all {
		q, args = q+` WHERE tenant_id=$1`, []any{tenant}
	}
	rows, err := p.db.QueryContext(ctx, q+` ORDER BY since DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.TenantID, &a.Agent, &a.Kind, &a.Severity, &a.Summary, &a.RunID, &a.Since, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *Postgres) PutAlert(ctx context.Context, a Alert) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO ar_alerts (tenant_id, agent, kind, severity, summary, run_id, since, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id, agent, kind) DO UPDATE SET severity=$4, summary=$5, run_id=$6, last_seen=$8`,
		a.TenantID, a.Agent, a.Kind, a.Severity, clip(a.Summary, maxTraceText), a.RunID, a.Since, a.LastSeen)
	return err
}

func (p *Postgres) DeleteAlert(ctx context.Context, tenant, agent, kind string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM ar_alerts WHERE tenant_id=$1 AND agent=$2 AND kind=$3`, tenant, agent, kind)
	return err
}

func (p *Postgres) ListSessions(ctx context.Context, tenant string, f SessionFilter) ([]SessionSummary, error) {
	q := `SELECT session_id, agent, count(*), count(*) FILTER (WHERE status <> 'ok'),
		(array_agg(left(input, ` + fmt.Sprint(previewLen+1) + `) ORDER BY started_at))[1], min(started_at), max(started_at)
		FROM ar_runs WHERE tenant_id=$1 AND session_id <> ''`
	args := []any{tenant}
	if f.Agent != "" {
		args = append(args, f.Agent)
		q += fmt.Sprintf(` AND agent=$%d`, len(args))
	}
	q += ` GROUP BY session_id, agent`
	var having []string
	if f.Failed {
		having = append(having, `count(*) FILTER (WHERE status <> 'ok') > 0`)
	}
	if !f.Before.IsZero() {
		args = append(args, f.Before)
		having = append(having, fmt.Sprintf(`max(started_at) < $%d`, len(args)))
	}
	if len(having) > 0 {
		q += ` HAVING ` + strings.Join(having, " AND ")
	}
	args = append(args, f.limit())
	q += fmt.Sprintf(` ORDER BY max(started_at) DESC LIMIT $%d`, len(args))
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionSummary{}
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(&s.SessionID, &s.Agent, &s.Runs, &s.Failed, &s.FirstInput, &s.FirstAt, &s.LastAt); err != nil {
			return nil, err
		}
		s.FirstInput = clip(s.FirstInput, previewLen)
		out = append(out, s)
	}
	return out, rows.Err()
}
