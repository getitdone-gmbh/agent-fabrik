package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/testutil"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/llm"
)

// Run the Postgres suite with:
//
//	AR_TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/ar?sslmode=disable go test ./internal/store/
func backends(t *testing.T) map[string]func(t *testing.T) Store {
	b := map[string]func(t *testing.T) Store{
		"mem": func(t *testing.T) Store {
			m, err := NewMem("", time.Hour, 100)
			if err != nil {
				t.Fatal(err)
			}
			return m
		},
	}
	if url := testutil.PostgresURL(t, "store"); url != "" {
		b["postgres"] = func(t *testing.T) Store {
			p, err := NewPostgres(context.Background(), url, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			p.db.Exec(`TRUNCATE ar_tenants, ar_sessions CASCADE`)
			return p
		}
	}
	return b
}

func TestConformance(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			defer s.Close()
			conformance(t, s)
		})
	}
}

func conformance(t *testing.T, s Store) {
	ctx := context.Background()
	lim := config.Limits{MaxAgents: 3}
	if err := s.CreateTenant(ctx, Tenant{ID: "t1", Name: "One", Plan: "free", Limits: &lim}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTenant(ctx, Tenant{ID: "t1"}); !errors.Is(err, ErrExists) {
		t.Fatalf("dup tenant: %v", err)
	}
	s.CreateTenant(ctx, Tenant{ID: "t2", Name: "Two"})
	tn, err := s.GetTenant(ctx, "t1")
	if err != nil || tn.Name != "One" || tn.Limits == nil || tn.Limits.MaxAgents != 3 || tn.CreatedAt.IsZero() {
		t.Fatalf("get: %+v %v", tn, err)
	}
	if _, err := s.GetTenant(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// keys
	if err := s.CreateKey(ctx, APIKey{ID: "k1", TenantID: "t1", Name: "ci", Hash: "h1", Prefix: "ark_ab"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, APIKey{ID: "k2", TenantID: "missing", Hash: "h2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for missing tenant: %v", err)
	}
	k, err := s.KeyByHash(ctx, "h1")
	if err != nil || k.TenantID != "t1" || k.Name != "ci" {
		t.Fatalf("key: %+v %v", k, err)
	}
	if ks, _ := s.ListKeys(ctx, "t1"); len(ks) != 1 {
		t.Fatalf("keys %v", ks)
	}

	// resources + secrets + check abort
	spec := json.RawMessage(`{"type":"mistral"}`)
	err = s.Apply(ctx, "t1", Change{
		Put:        []Resource{{Kind: KindProvider, Name: "m", Spec: spec}, {Kind: KindAgent, Name: "a", Spec: json.RawMessage(`{"provider":"m"}`)}},
		PutSecrets: map[string][]byte{"KEY": []byte("enc")},
	}, func(st State) error {
		if len(st.Resources) != 2 || len(st.SecretNames) != 1 {
			t.Errorf("state: %+v", st)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort")
	if err := s.Apply(ctx, "t1", Change{Delete: []Resource{{Kind: KindProvider, Name: "m"}}}, func(State) error { return abort }); !errors.Is(err, abort) {
		t.Fatalf("abort: %v", err)
	}
	rs, _ := s.ListResources(ctx, "t1")
	if len(rs) != 2 {
		t.Fatalf("aborted change applied: %+v", rs)
	}
	var got map[string]string
	for _, r := range rs {
		if r.Kind == KindProvider {
			json.Unmarshal(r.Spec, &got)
		}
	}
	if got["type"] != "mistral" {
		t.Errorf("spec: %v", got)
	}
	sec, _ := s.Secrets(ctx, "t1")
	if string(sec["KEY"]) != "enc" {
		t.Errorf("secrets: %v", sec)
	}
	if other, _ := s.ListResources(ctx, "t2"); len(other) != 0 {
		t.Error("tenant isolation broken")
	}
	vers, _ := s.TenantVersions(ctx)
	if vers["t1"] != 1 || vers["t2"] != 0 {
		t.Errorf("versions %v", vers)
	}
	// declarative replace
	s.Apply(ctx, "t1", Change{ReplaceAll: true, Put: []Resource{{Kind: KindProvider, Name: "x", Spec: spec}}, DeleteSecrets: []string{"KEY"}}, nil)
	rs, _ = s.ListResources(ctx, "t1")
	if len(rs) != 1 || rs[0].Name != "x" {
		t.Errorf("replace: %+v", rs)
	}
	if sec, _ := s.Secrets(ctx, "t1"); len(sec) != 0 {
		t.Errorf("secret not deleted")
	}

	// usage
	s.AddUsage(ctx, "t1", "2026-09", 10, 5, 1)
	s.AddUsage(ctx, "t1", "2026-09", 1, 1, 1)
	u, _ := s.GetUsage(ctx, "t1", "2026-09")
	if u.InputTokens != 11 || u.OutputTokens != 6 || u.Runs != 2 {
		t.Errorf("usage %+v", u)
	}

	// update + list
	tn.Disabled, tn.Plan = true, "pro"
	if err := s.UpdateTenant(ctx, *tn); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.ListTenants(ctx)
	ids := []string{}
	for _, x := range ts {
		ids = append(ids, x.ID)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ts[0].Plan != "pro" || !ts[0].Disabled {
		t.Errorf("list %+v", ts)
	}

	// sessions
	ss := s.Sessions()
	if err := ss.Save(ctx, "t1/a", "s1", []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := ss.Load(ctx, "t1/a", "s1"); len(m) != 1 || m[0].Content != "hi" {
		t.Errorf("session %v", m)
	}
	if m, _ := ss.Load(ctx, "t2/a", "s1"); m != nil {
		t.Error("session isolation broken")
	}

	// delete cascades
	if err := s.DeleteTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeyByHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Error("keys not cascaded")
	}
	if rs, _ := s.ListResources(ctx, "t1"); len(rs) != 0 {
		t.Error("resources not cascaded")
	}
	if !s.Leader(ctx) {
		t.Error("single instance should be leader")
	}
}

func TestMemFilePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	m, _ := NewMem(path, time.Hour, 10)
	ctx := context.Background()
	m.CreateTenant(ctx, Tenant{ID: "a"})
	m.Apply(ctx, "a", Change{Put: []Resource{{Kind: KindAgent, Name: "x", Spec: json.RawMessage(`{}`)}}}, nil)
	m.Close()
	m2, err := NewMem(path, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	rs, _ := m2.ListResources(ctx, "a")
	if len(rs) != 1 {
		t.Fatalf("not persisted: %+v", rs)
	}
}

func TestPostgresLeaderExclusive(t *testing.T) {
	url := testutil.PostgresURL(t, "store")
	if url == "" {
		t.Skip("AR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	a, _ := NewPostgres(ctx, url, time.Hour)
	b, _ := NewPostgres(ctx, url, time.Hour)
	defer b.Close()
	if !a.Leader(ctx) {
		t.Fatal("a should lead")
	}
	if b.Leader(ctx) {
		t.Fatal("b must not lead while a holds the lock")
	}
	a.Close()
	if !b.Leader(ctx) {
		t.Fatal("b should take over")
	}
}

// Two replicas (two pools on one database) must not run one session at once.
func TestSessionLockAcrossReplicas(t *testing.T) {
	url := testutil.PostgresURL(t, "store")
	if url == "" {
		t.Skip("AR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r1, err := NewPostgres(ctx, url, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := NewPostgres(ctx, url, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	l1 := r1.Sessions().(agent.Locker)
	l2 := r2.Sessions().(agent.Locker)

	unlock, err := l1.LockSession(ctx, "t/a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	// Another session is independent.
	u, err := l2.LockSession(ctx, "t/a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	u()
	got := make(chan struct{})
	go func() {
		u, err := l2.LockSession(ctx, "t/a", "s1")
		if err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second replica got the lock while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("lock not released")
	}
	// A cancelled wait returns an error instead of hanging.
	unlock, _ = l1.LockSession(ctx, "t/a", "s1")
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := l2.LockSession(cctx, "t/a", "s1"); err == nil {
		t.Fatal("expected timeout")
	}
	unlock()
}

// A held session lease must not pin a pooled connection for the run, and a
// replica that dies while holding one must not block the session forever.
func TestSessionLeaseHoldsNoConnectionAndExpires(t *testing.T) {
	url := testutil.PostgresURL(t, "store")
	if url == "" {
		t.Skip("AR_TEST_DATABASE_URL not set")
	}
	oldTTL, oldRenew := leaseTTL, leaseRenew
	leaseTTL, leaseRenew = 400*time.Millisecond, time.Hour // the "dead" holder never renews
	defer func() { leaseTTL, leaseRenew = oldTTL, oldRenew }()
	ctx := context.Background()
	r1, err := NewPostgres(ctx, url, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := NewPostgres(ctx, url, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r1.Sessions().(agent.Locker).LockSession(ctx, "t/a", "lease"); err != nil {
		t.Fatal(err)
	}
	if n := r1.db.Stats().InUse; n != 0 {
		t.Errorf("held lease pins %d connection(s)", n)
	}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	unlock, err := r2.Sessions().(agent.Locker).LockSession(wctx, "t/a", "lease")
	if err != nil {
		t.Fatalf("lease of a dead holder never expired: %v", err)
	}
	unlock()
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("second replica got the session after %v, before the lease ran out", d)
	}
}
