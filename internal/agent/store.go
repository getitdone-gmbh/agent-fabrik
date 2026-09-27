package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/llm"
)

// Store persists conversation history per agent and session.
type Store interface {
	Load(ctx context.Context, agent, session string) ([]llm.Message, error)
	Save(ctx context.Context, agent, session string, msgs []llm.Message) error
	Delete(ctx context.Context, agent, session string) error
}

// Locker is implemented by stores shared between replicas. LockSession
// serializes runs of one session across processes; the returned func
// releases the lock.
type Locker interface {
	LockSession(ctx context.Context, agent, session string) (func(), error)
}

// MemoryStore keeps sessions in RAM with a TTL and a size cap.
type MemoryStore struct {
	mu          sync.Mutex
	ttl         time.Duration
	maxSessions int
	items       map[string]memItem
}

type memItem struct {
	msgs    []llm.Message
	touched time.Time
}

func NewMemoryStore(ttl time.Duration, maxSessions int) *MemoryStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxSessions <= 0 {
		maxSessions = 1000
	}
	return &MemoryStore{ttl: ttl, maxSessions: maxSessions, items: map[string]memItem{}}
}

func key(agent, session string) string { return agent + "\x00" + session }

func (m *MemoryStore) Load(_ context.Context, agent, session string) ([]llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key(agent, session)]
	if !ok || time.Since(it.touched) > m.ttl {
		return nil, nil
	}
	return append([]llm.Message(nil), it.msgs...), nil
}

func (m *MemoryStore) Save(_ context.Context, agent, session string, msgs []llm.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.items[key(agent, session)] = memItem{msgs: msgs, touched: now}
	if len(m.items) > m.maxSessions {
		// Evict expired entries, then the oldest one.
		var oldestK string
		var oldestT time.Time
		for k, it := range m.items {
			if now.Sub(it.touched) > m.ttl {
				delete(m.items, k)
				continue
			}
			if oldestK == "" || it.touched.Before(oldestT) {
				oldestK, oldestT = k, it.touched
			}
		}
		if len(m.items) > m.maxSessions {
			delete(m.items, oldestK)
		}
	}
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, agent, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key(agent, session))
	return nil
}

// FileStore writes one JSON file per session (survives restarts when the
// directory is on a persistent volume).
type FileStore struct {
	dir string
	ttl time.Duration
}

func NewFileStore(dir string, ttl time.Duration) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &FileStore{dir: dir, ttl: ttl}, nil
}

func (f *FileStore) path(agent, session string) string {
	sum := sha256.Sum256([]byte(key(agent, session)))
	return filepath.Join(f.dir, hex.EncodeToString(sum[:16])+".json")
}

func (f *FileStore) Load(_ context.Context, agent, session string) ([]llm.Message, error) {
	p := f.path(agent, session)
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Since(st.ModTime()) > f.ttl {
		_ = os.Remove(p)
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	return msgs, json.Unmarshal(b, &msgs)
}

func (f *FileStore) Save(_ context.Context, agent, session string, msgs []llm.Message) error {
	b, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	p := f.path(agent, session)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (f *FileStore) Delete(_ context.Context, agent, session string) error {
	err := os.Remove(f.path(agent, session))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Sweep removes expired session files.
func (f *FileStore) Sweep() {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > f.ttl {
			_ = os.Remove(filepath.Join(f.dir, e.Name()))
		}
	}
}

// Prefixed namespaces another store (e.g. per tenant).
func Prefixed(inner Store, prefix string) Store { return prefixed{inner, prefix} }

type prefixed struct {
	inner  Store
	prefix string
}

func (p prefixed) Load(ctx context.Context, agent, session string) ([]llm.Message, error) {
	return p.inner.Load(ctx, p.prefix+agent, session)
}
func (p prefixed) Save(ctx context.Context, agent, session string, msgs []llm.Message) error {
	return p.inner.Save(ctx, p.prefix+agent, session, msgs)
}
func (p prefixed) Delete(ctx context.Context, agent, session string) error {
	return p.inner.Delete(ctx, p.prefix+agent, session)
}
func (p prefixed) LockSession(ctx context.Context, agent, session string) (func(), error) {
	if l, ok := p.inner.(Locker); ok {
		return l.LockSession(ctx, p.prefix+agent, session)
	}
	return func() {}, nil
}
