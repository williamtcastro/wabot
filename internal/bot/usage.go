package bot

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// TokenUsage is what one caller (a token name, or api / mcp) did in the last 24 hours. It's kept
// in memory and starts over when wame restarts.
type TokenUsage struct {
	Name     string    `json:"name"`
	Requests int64     `json:"requests_24h"`
	Sends    int64     `json:"sends_24h"`
	Failed   int64     `json:"failed_sends_24h"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	LastIP   string    `json:"last_ip,omitempty"`
}

type usageEntry struct {
	hours    [24]int64
	requests [24]int64
	sends    [24]int64
	failed   [24]int64
	lastSeen time.Time
	lastIP   string
}

// bucket returns the slot for the current hour, clearing it if it last held an older hour.
func (e *usageEntry) bucket(now time.Time) int {
	h := now.Unix() / 3600
	i := int(h % 24)
	if e.hours[i] != h {
		e.hours[i], e.requests[i], e.sends[i], e.failed[i] = h, 0, 0, 0
	}
	return i
}

type usage struct {
	mu                 sync.Mutex
	by                 map[string]*usageEntry
	sentOK, sentFailed int64
}

func (u *usage) entry(name string) *usageEntry {
	if u.by == nil {
		u.by = map[string]*usageEntry{}
	}
	e, ok := u.by[name]
	if !ok {
		e = &usageEntry{}
		u.by[name] = e
	}
	return e
}

// RecordRequest counts an authenticated API or MCP request by caller name.
func (b *Bot) RecordRequest(name, ip string) {
	if name == "" {
		return
	}
	now := time.Now()
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	e := b.usage.entry(name)
	e.requests[e.bucket(now)]++
	e.lastSeen, e.lastIP = now, ip
}

func (b *Bot) recordSend(name string, ok bool) {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	e := b.usage.entry(name)
	i := e.bucket(time.Now())
	if ok {
		e.sends[i]++
		b.usage.sentOK++
	} else {
		e.failed[i]++
		b.usage.sentFailed++
	}
}

// SendTotals counts sends (text and files, every caller) since startup.
func (b *Bot) SendTotals() (ok, failed int64) {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	return b.usage.sentOK, b.usage.sentFailed
}

// Usage returns every caller seen since startup, by name.
func (b *Bot) Usage() []TokenUsage {
	now := time.Now()
	h := now.Unix() / 3600
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	out := make([]TokenUsage, 0, len(b.usage.by))
	for name, e := range b.usage.by {
		t := TokenUsage{Name: name, LastSeen: e.lastSeen, LastIP: e.lastIP}
		for i := range 24 {
			if h-e.hours[i] < 24 {
				t.Requests += e.requests[i]
				t.Sends += e.sends[i]
				t.Failed += e.failed[i]
			}
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b TokenUsage) int { return strings.Compare(a.Name, b.Name) })
	return out
}
