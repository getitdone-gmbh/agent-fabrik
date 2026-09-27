// Package store persists tenants, API keys, resource definitions
// (providers, connectors, agents), encrypted secrets, usage counters and
// agent sessions. Backends: in-memory (optionally snapshotted to a file) and
// PostgreSQL.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/config"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Resource kinds.
const (
	KindProvider  = "provider"
	KindConnector = "connector"
	KindAgent     = "agent"
)

// Kinds lists all resource kinds.
var Kinds = []string{KindProvider, KindConnector, KindAgent}

// Tenant is an isolated customer account.
type Tenant struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Plan      string         `json:"plan"`
	Limits    *config.Limits `json:"limits,omitempty"` // overrides the plan
	Disabled  bool           `json:"disabled"`
	Version   int64          `json:"version"` // bumped on every resource/secret change
	CreatedAt time.Time      `json:"created_at"`
}

// APIKey authenticates a tenant. Only the SHA-256 hash is stored.
type APIKey struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	Hash      string    `json:"-"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
}

// Resource is a stored definition (the same schema as the YAML sections).
type Resource struct {
	TenantID  string          `json:"-"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Usage is a tenant's consumption for one month (YYYY-MM).
type Usage struct {
	Month        string `json:"month"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Runs         int64  `json:"runs"`
}

// Store is the persistence interface.
type Store interface {
	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (*Tenant, error)
	UpdateTenant(ctx context.Context, t Tenant) error // name, plan, limits, disabled
	DeleteTenant(ctx context.Context, id string) error
	ListTenants(ctx context.Context) ([]Tenant, error)
	// TenantVersions returns id -> version for change detection.
	TenantVersions(ctx context.Context) (map[string]int64, error)

	CreateKey(ctx context.Context, k APIKey) error
	KeyByHash(ctx context.Context, hash string) (*APIKey, error)
	ListKeys(ctx context.Context, tenant string) ([]APIKey, error)
	DeleteKey(ctx context.Context, tenant, id string) error

	// PutResources upserts and deletes resources and secrets of a tenant
	// atomically and bumps its version. check runs inside the transaction
	// with the resulting state; returning an error aborts the change.
	Apply(ctx context.Context, tenant string, ch Change, check func(State) error) error
	ListResources(ctx context.Context, tenant string) ([]Resource, error)
	// Secrets returns name -> encrypted value.
	Secrets(ctx context.Context, tenant string) (map[string][]byte, error)

	AddUsage(ctx context.Context, tenant, month string, in, out, runs int64) error
	GetUsage(ctx context.Context, tenant, month string) (Usage, error)

	// Sessions returns a session store shared by all tenants (keys are
	// namespaced by the caller).
	Sessions() agent.Store

	// Leader reports whether this replica should run schedulers. Backends
	// without coordination always return true.
	Leader(ctx context.Context) bool

	Close() error
}

// Change is a batch of modifications.
type Change struct {
	Put           []Resource
	Delete        []Resource // Kind+Name
	PutSecrets    map[string][]byte
	DeleteSecrets []string
	// ReplaceAll deletes every resource not in Put (declarative apply).
	ReplaceAll bool
}

// State is a tenant's resources and secret names after a change.
type State struct {
	Resources   []Resource
	SecretNames []string
}

func resKey(kind, name string) string { return kind + "/" + name }

// merge applies ch to the current resources/secrets.
func merge(cur []Resource, secretNames []string, ch Change) State {
	m := map[string]Resource{}
	if !ch.ReplaceAll {
		for _, r := range cur {
			m[resKey(r.Kind, r.Name)] = r
		}
	}
	for _, r := range ch.Delete {
		delete(m, resKey(r.Kind, r.Name))
	}
	for _, r := range ch.Put {
		m[resKey(r.Kind, r.Name)] = r
	}
	st := State{}
	for _, r := range m {
		st.Resources = append(st.Resources, r)
	}
	sec := map[string]bool{}
	for _, n := range secretNames {
		sec[n] = true
	}
	for n := range ch.PutSecrets {
		sec[n] = true
	}
	for _, n := range ch.DeleteSecrets {
		delete(sec, n)
	}
	for n := range sec {
		st.SecretNames = append(st.SecretNames, n)
	}
	return st
}

// Month returns the usage bucket for t.
func Month(t time.Time) string { return t.UTC().Format("2006-01") }
