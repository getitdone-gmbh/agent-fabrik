package tenant

import (
	"context"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/llm"
	"github.com/getitdone-gmbh/agentd/internal/store"
)

// bucket is a token-bucket rate limiter (runs per minute).
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *bucket) take(perMinute int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	capacity := float64(perMinute)
	if b.last.IsZero() {
		b.tokens = capacity
	} else {
		b.tokens += now.Sub(b.last).Minutes() * capacity
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (b *bucket) idle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last) > 2*time.Minute
}

func (m *Manager) allow(tenant string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	m.mu.Lock()
	b := m.limiters[tenant]
	if b == nil {
		b = &bucket{}
		m.limiters[tenant] = b
	}
	m.mu.Unlock()
	return b.take(perMinute)
}

// usageCounter caches this month's token usage per tenant. It is loaded
// from the store once and then updated locally; other replicas' usage is
// picked up when the cache is refreshed (every minute).
type usageCounter struct {
	month   string
	tokens  int64
	fetched time.Time
}

func (m *Manager) usedTokens(ctx context.Context, tenant string) (int64, error) {
	month := store.Month(time.Now())
	m.mu.Lock()
	u := m.usage[tenant]
	if u != nil && u.month == month && time.Since(u.fetched) < time.Minute {
		n := u.tokens
		m.mu.Unlock()
		return n, nil
	}
	m.mu.Unlock()
	us, err := m.st.GetUsage(ctx, tenant, month)
	if err != nil {
		return 0, err
	}
	n := us.InputTokens + us.OutputTokens
	m.mu.Lock()
	m.usage[tenant] = &usageCounter{month: month, tokens: n, fetched: time.Now()}
	m.mu.Unlock()
	return n, nil
}

func (m *Manager) recordUsage(tenant string, u llm.Usage) {
	// Token counts come from the provider's response, and a tenant may run
	// its own provider endpoint: a negative count would lower the monthly
	// total and unlock the shared (operator-paid) provider.
	u.InputTokens, u.OutputTokens = max(u.InputTokens, 0), max(u.OutputTokens, 0)
	month := store.Month(time.Now())
	m.mu.Lock()
	if c := m.usage[tenant]; c != nil && c.month == month {
		c.tokens += int64(u.InputTokens + u.OutputTokens)
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.st.AddUsage(ctx, tenant, month, int64(u.InputTokens), int64(u.OutputTokens), 1); err != nil {
		m.log.Warn("recording usage failed", "tenant", tenant, "err", err)
	}
}

// Usage returns this month's usage.
func (m *Manager) Usage(ctx context.Context, tenant string) (store.Usage, error) {
	return m.st.GetUsage(ctx, tenant, store.Month(time.Now()))
}
