package mirror

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// rateMeterMax bounds the meter. It is the size backstop behind lazy expiry,
// not the primary eviction rule.
const rateMeterMax = 512

// staleResetGrace is how long a reading whose reset moment is unreadable is
// kept. It is the longest window a provider is likely to publish, so an entry
// older than this is describing a budget that cannot still be current.
const staleResetGrace = time.Hour

// RateBudget is the latest reading of one identity's budget for one resource.
type RateBudget struct {
	Principal  string    `json:"principal"`
	Resource   string    `json:"resource"`
	Limit      int       `json:"limit"`
	Remaining  int       `json:"remaining"`
	Used       int       `json:"used"`
	Reset      time.Time `json:"reset"`
	ObservedAt time.Time `json:"observed_at"`
	// Stale says the reset moment has already passed, so this reading describes
	// a window that is over. The dashboard renders it as such rather than as a
	// budget about to refresh.
	Stale bool `json:"stale"`
}

// RateMeter is a passive, in-memory view of what the upstream last said about
// each identity's budget.
//
// Passive is the whole design: nothing here asks the upstream for a budget. It
// reads the headers of answers the mirror was already getting, so the meter
// costs nothing and cannot itself spend the budget it reports.
type RateMeter struct {
	mu      sync.Mutex
	entries map[string]*RateBudget
	headers RateHeaders
	now     func() time.Time
}

// RateHeaders names the response headers that carry a budget. The names are the
// upstream's vocabulary, so they come from the spec rather than from here.
type RateHeaders struct {
	Limit     string
	Remaining string
	Used      string
	Reset     string
	Resource  string
}

// declared reports whether the spec named enough to read a budget at all.
func (h RateHeaders) declared() bool {
	return h.Limit != "" || h.Remaining != "" || h.Reset != ""
}

// NewRateMeter returns a meter reading the headers the spec declared. A meter
// with no declared headers observes nothing rather than guessing at names.
func NewRateMeter(headers RateHeaders) *RateMeter {
	return &RateMeter{entries: map[string]*RateBudget{}, headers: headers, now: time.Now}
}

// Observe reads one answer's budget headers.
func (m *RateMeter) Observe(principal string, h http.Header) {
	if m == nil || h == nil || !m.headers.declared() {
		return
	}
	limit, hasLimit := headerInt(h, m.headers.Limit)
	remaining, hasRemaining := headerInt(h, m.headers.Remaining)
	if !hasLimit && !hasRemaining {
		return
	}
	used, _ := headerInt(h, m.headers.Used)
	resource := h.Get(m.headers.Resource)
	if resource == "" {
		resource = "default"
	}
	if principal == "" {
		principal = "anonymous"
	}
	b := &RateBudget{
		Principal:  principal,
		Resource:   resource,
		Limit:      limit,
		Remaining:  remaining,
		Used:       used,
		ObservedAt: m.now(),
	}
	if secs, ok := headerInt(h, m.headers.Reset); ok && secs > 0 {
		b.Reset = time.Unix(int64(secs), 0).UTC()
	}
	if !hasLimit && used > 0 && hasRemaining {
		b.Limit = used + remaining
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[principal+"\x00"+resource] = b
	m.sweepLocked()
	m.capLocked()
}

// Snapshot returns the live readings, most recently observed first.
func (m *RateMeter) Snapshot() []RateBudget {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	now := m.now()
	out := make([]RateBudget, 0, len(m.entries))
	for _, b := range m.entries {
		copied := *b
		copied.Stale = !copied.Reset.IsZero() && copied.Reset.Before(now)
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObservedAt.After(out[j].ObservedAt) })
	return out
}

// sweepLocked drops readings that cannot still describe anything.
//
// An identity still calling is re-observed with a fresh reset on every answer,
// so a reset well in the past means that identity stopped calling. Expiry is
// lazy on both paths rather than on a timer: a goroutine whose only job is to
// delete is a moving part with nothing to gain from moving.
func (m *RateMeter) sweepLocked() {
	now := m.now()
	for k, b := range m.entries {
		if !b.Reset.IsZero() {
			if b.Reset.Add(staleResetGrace).Before(now) {
				delete(m.entries, k)
			}
			continue
		}
		if b.ObservedAt.Add(staleResetGrace).Before(now) {
			delete(m.entries, k)
		}
	}
}

// capLocked enforces the size ceiling by dropping the least recently observed.
func (m *RateMeter) capLocked() {
	if len(m.entries) <= rateMeterMax {
		return
	}
	type aged struct {
		key string
		at  time.Time
	}
	all := make([]aged, 0, len(m.entries))
	for k, b := range m.entries {
		all = append(all, aged{key: k, at: b.ObservedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)-rateMeterMax; i++ {
		delete(m.entries, all[i].key)
	}
}

func headerInt(h http.Header, name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
