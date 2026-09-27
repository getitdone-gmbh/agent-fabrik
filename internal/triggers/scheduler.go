package triggers

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/config"
)

// Scheduler fires cron, interval and IMAP triggers for any number of
// tenants from one goroutine and a bounded worker pool. Idle triggers cost a
// heap entry, not a goroutine.
type Scheduler struct {
	envFor func(tenant string) Env
	log    *slog.Logger
	sem    chan struct{}

	mu       sync.Mutex
	h        entryHeap
	byTenant map[string]map[string]*entry
	wake     chan struct{}
	wg       sync.WaitGroup
	fired    atomic.Int64
	skipped  atomic.Int64
}

type entry struct {
	tenant, agent string
	trigger       config.Trigger
	key           string
	next          time.Time
	cron          *config.Cron
	loc           *time.Location
	every         time.Duration
	poller        *poller
	running       atomic.Bool
	removed       atomic.Bool
	index         int
}

// NewScheduler creates a scheduler; envFor resolves a tenant's environment
// at fire time.
func NewScheduler(envFor func(tenant string) Env, workers int, log *slog.Logger) *Scheduler {
	if workers <= 0 {
		workers = 32
	}
	return &Scheduler{
		envFor:   envFor,
		log:      log,
		sem:      make(chan struct{}, workers),
		byTenant: map[string]map[string]*entry{},
		wake:     make(chan struct{}, 1),
	}
}

func triggerKey(agentName string, i int, t config.Trigger) string {
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	return agentName + "#" + strconv.Itoa(i) + "#" + hex.EncodeToString(sum[:8])
}

// Sync replaces the scheduled triggers of a tenant. Unchanged triggers keep
// their state (next fire time, retry counters).
func (s *Scheduler) Sync(tenant string, agents map[string]config.AgentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.byTenant[tenant]
	cur := map[string]*entry{}
	now := time.Now()
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, t := range agents[name].Triggers {
			if t.Type == "webhook" {
				continue
			}
			k := triggerKey(name, i, t)
			if e, ok := old[k]; ok {
				cur[k] = e
				continue
			}
			e := &entry{tenant: tenant, agent: name, trigger: t, key: k, loc: time.UTC}
			log := s.log.With("tenant", tenant, "agent", name, "trigger", t.Type)
			switch t.Type {
			case "cron":
				c, err := config.ParseCron(t.Schedule)
				if err != nil {
					log.Error("bad cron", "err", err)
					continue
				}
				e.cron = c
				if t.Timezone != "" {
					if l, err := time.LoadLocation(t.Timezone); err == nil {
						e.loc = l
					}
				}
				e.next = c.Next(now.In(e.loc))
			case "interval", "imap":
				def := time.Minute
				e.every = def
				if d, err := time.ParseDuration(t.Every); err == nil && d > 0 {
					e.every = d
				}
				// Random first offset spreads many agents over the interval.
				e.next = now.Add(time.Duration(rand.Int64N(int64(e.every)) + 1))
				if t.Type == "imap" {
					e.poller = &poller{agent: name, t: t, log: log, attempts: map[uint32]int{}}
				}
			default:
				continue
			}
			if e.next.IsZero() {
				continue
			}
			cur[k] = e
			heap.Push(&s.h, e)
		}
	}
	for k, e := range old {
		if _, keep := cur[k]; !keep {
			e.removed.Store(true)
			if e.index >= 0 && e.index < len(s.h) && s.h[e.index] == e {
				heap.Remove(&s.h, e.index)
			}
		}
	}
	if len(cur) == 0 {
		delete(s.byTenant, tenant)
	} else {
		s.byTenant[tenant] = cur
	}
	s.poke()
}

// Remove drops all triggers of a tenant.
func (s *Scheduler) Remove(tenant string) { s.Sync(tenant, nil) }

// Clear drops everything (e.g. when losing leadership).
func (s *Scheduler) Clear() {
	s.mu.Lock()
	tenants := make([]string, 0, len(s.byTenant))
	for t := range s.byTenant {
		tenants = append(tenants, t)
	}
	s.mu.Unlock()
	for _, t := range tenants {
		s.Remove(t)
	}
}

// Stats reports scheduler counters.
func (s *Scheduler) Stats() map[string]int64 {
	s.mu.Lock()
	n := int64(len(s.h))
	s.mu.Unlock()
	return map[string]int64{"entries": n, "fired": s.fired.Load(), "skipped_busy": s.skipped.Load()}
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run drives the scheduler until ctx ends, then waits for running jobs.
func (s *Scheduler) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		now := time.Now()
		for len(s.h) > 0 && !s.h[0].next.After(now) {
			e := heap.Pop(&s.h).(*entry)
			s.fire(ctx, e, e.next)
			if e.cron != nil {
				e.next = e.cron.Next(now.In(e.loc))
			} else {
				e.next = now.Add(e.every)
			}
			if !e.next.IsZero() {
				heap.Push(&s.h, e)
			}
		}
		wait := time.Hour
		if len(s.h) > 0 {
			wait = time.Until(s.h[0].next)
		}
		s.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-timer.C:
		case <-s.wake:
		}
	}
}

func (s *Scheduler) fire(ctx context.Context, e *entry, at time.Time) {
	if !e.running.CompareAndSwap(false, true) {
		s.skipped.Add(1) // previous run still busy
		return
	}
	s.fired.Add(1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer e.running.Store(false)
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-s.sem }()
		if e.removed.Load() {
			return
		}
		env := s.envFor(e.tenant)
		if env == nil {
			return
		}
		log := s.log.With("tenant", e.tenant, "agent", e.agent, "trigger", e.trigger.Type)
		if e.poller != nil {
			if err := e.poller.poll(ctx, env); err != nil && ctx.Err() == nil {
				log.Warn("imap poll failed", "err", err)
			}
			return
		}
		runScheduled(ctx, env, e.agent, e.trigger, at, log)
	}()
}

type entryHeap []*entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h entryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *entryHeap) Push(x any) {
	e := x.(*entry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}
