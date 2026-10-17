package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/store"
)

// KeyPrefix marks runtime API keys.
const KeyPrefix = "ark_"

// ErrUnauthorized means the token is not valid.
var ErrUnauthorized = errors.New("unauthorized")

func hashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

func randID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// CreateKey issues a new API key; the plaintext is returned only once.
func (m *Manager) CreateKey(ctx context.Context, tenant, name string) (string, *store.APIKey, error) {
	if tenant == System {
		return "", nil, ErrReadOnly
	}
	plain := randID(KeyPrefix, 32)
	k := store.APIKey{ID: randID("key_", 9), TenantID: tenant, Name: name, Hash: hashToken(plain), Prefix: plain[:12]}
	if err := m.st.CreateKey(ctx, k); err != nil {
		return "", nil, err
	}
	k.CreatedAt = time.Now().UTC()
	return plain, &k, nil
}

// DeleteKey revokes a key.
func (m *Manager) DeleteKey(ctx context.Context, tenant, id string) error {
	if err := m.st.DeleteKey(ctx, tenant, id); err != nil {
		return err
	}
	m.keys.clear() // revocation takes effect immediately on this replica
	return nil
}

// Authenticate maps a bearer token (runtime key or external token) to a tenant.
func (m *Manager) Authenticate(ctx context.Context, token string) (string, error) {
	if !m.Enabled() || token == "" {
		return "", ErrUnauthorized
	}
	h := hashToken(token)
	if id, ok := m.keys.get(h); ok {
		if id == "" {
			return "", ErrUnauthorized
		}
		return id, nil
	}
	var id string
	if strings.HasPrefix(token, KeyPrefix) {
		k, err := m.st.KeyByHash(ctx, h)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if k != nil {
			id = k.TenantID
		}
	} else if m.external != nil {
		var err error
		id, err = m.externalTenant(ctx, token)
		if err != nil && !errors.Is(err, ErrUnauthorized) {
			return "", err
		}
	}
	m.keys.put(h, id)
	if id == "" {
		return "", ErrUnauthorized
	}
	return id, nil
}

// keyCache caches token hash -> tenant id (negative results as "").
type keyCache struct {
	mu sync.Mutex
	m  map[string]keyEntry
}

type keyEntry struct {
	tenant string
	exp    time.Time
}

func newKeyCache() *keyCache { return &keyCache{m: map[string]keyEntry{}} }

func (c *keyCache) get(h string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[h]
	if !ok || time.Now().After(e.exp) {
		return "", false
	}
	return e.tenant, true
}

func (c *keyCache) put(h, tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := time.Minute
	if tenant == "" {
		ttl = 10 * time.Second
	}
	if len(c.m) > 100000 {
		c.m = map[string]keyEntry{}
	}
	c.m[h] = keyEntry{tenant: tenant, exp: time.Now().Add(ttl)}
}

func (c *keyCache) clear() {
	c.mu.Lock()
	c.m = map[string]keyEntry{}
	c.mu.Unlock()
}

// ---------- external token authentication (OIDC userinfo) ----------

// userinfoAuth validates foreign bearer tokens by calling a userinfo
// endpoint (OIDC /userinfo of Keycloak, Zitadel, Auth0, Google, ... or any
// "who am I" API). Each distinct user becomes a tenant.
type userinfoAuth struct {
	cfg    config.ExternalAuthConfig
	client *http.Client
	mu     sync.Mutex
	cache  map[string]userinfoEntry
}

type userinfoEntry struct {
	user externalUser
	exp  time.Time
}

type externalUser struct {
	ID, Name, Plan string
	Allowed        bool // require_field is true (or not configured)
}

func newUserinfoAuth(cfg config.ExternalAuthConfig) *userinfoAuth {
	return &userinfoAuth{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, cache: map[string]userinfoEntry{}}
}

// field reads a dot-separated path ("user.id") from decoded JSON.
func field(doc map[string]any, path string) string {
	if path == "" {
		return ""
	}
	var cur any = doc
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func (a *userinfoAuth) lookup(ctx context.Context, token string) (*externalUser, error) {
	h := hashToken(token)
	a.mu.Lock()
	if e, ok := a.cache[h]; ok && time.Now().Before(e.exp) {
		a.mu.Unlock()
		u := e.user
		return &u, nil
	}
	a.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.UserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("external auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("external auth: HTTP %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("external auth: invalid userinfo response")
	}
	u := externalUser{ID: field(doc, a.cfg.IDFieldOrDefault()), Plan: field(doc, a.cfg.PlanField)}
	u.Allowed = a.cfg.RequireField == "" || field(doc, a.cfg.RequireField) == "true"
	for _, f := range []string{a.cfg.NameField, "name", "email", "preferred_username"} {
		if u.Name = field(doc, f); u.Name != "" {
			break
		}
	}
	if u.ID == "" {
		return nil, fmt.Errorf("external auth: userinfo response has no %q field", a.cfg.IDFieldOrDefault())
	}
	a.mu.Lock()
	if len(a.cache) > 100000 {
		a.cache = map[string]userinfoEntry{}
	}
	a.cache[h] = userinfoEntry{user: u, exp: time.Now().Add(a.cfg.TTL())}
	a.mu.Unlock()
	return &u, nil
}

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,35}$`)

// externalTenant validates a foreign token and ensures the user's tenant.
func (m *Manager) externalTenant(ctx context.Context, token string) (string, error) {
	u, err := m.external.lookup(ctx, token)
	if err != nil {
		return "", err
	}
	if !u.Allowed {
		return "", ErrUnauthorized
	}
	cfg := m.cfg.Tenancy.ExternalAuth
	id := cfg.PrefixOrDefault() + u.ID
	if !safeID.MatchString(u.ID) {
		// Anything else (e-mails, mixed case, OIDC sub URLs) is hashed:
		// stripping characters would map different users to one tenant.
		// Hashes are 40 chars, safe ids at most 36 (a UUID), so the two never meet.
		id = cfg.PrefixOrDefault() + hashToken(u.ID)[:40]
	}
	plan := m.cfg.Tenancy.DefaultPlan
	if p, ok := cfg.PlanMap[u.Plan]; ok {
		plan = p
	}
	t, err := m.st.GetTenant(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := m.st.CreateTenant(ctx, store.Tenant{ID: id, Name: u.Name, Plan: plan}); err != nil && !errors.Is(err, store.ErrExists) {
			return "", err
		}
		m.log.Info("tenant created for external user", "tenant", id, "plan", plan)
	case err != nil:
		return "", err
	case cfg.PlanField != "" && t.Plan != plan && t.Limits == nil:
		t.Plan = plan // follow plan changes (e.g. upgrade)
		if err := m.UpdateTenant(ctx, *t); err != nil {
			return "", err
		}
	}
	return id, nil
}
