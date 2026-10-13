package bot

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// RateLimitError is returned when a send would have to wait longer than the limiter's MaxWait.
type RateLimitError struct{ RetryAfter time.Duration }

func (e RateLimitError) Error() string {
	return fmt.Sprintf("rate limited, retry in %s", e.RetryAfter.Round(time.Second))
}

// Limiter spaces out outgoing messages: at most PerMinute in any rolling minute, and at least ChatGap
// between two messages to the same chat. Callers reserve a slot and wait for it.
type Limiter struct {
	PerMinute int
	ChatGap   time.Duration
	MaxWait   time.Duration

	mu           sync.Mutex
	slots        []time.Time
	byChat       map[string]time.Time
	rejected     int64
	lastRejected time.Time
}

// Set changes the limits while the limiter is in use; slots already reserved keep their time.
func (l *Limiter) Set(perMinute int, chatGap, maxWait time.Duration) {
	l.mu.Lock()
	l.PerMinute, l.ChatGap, l.MaxWait = perMinute, chatGap, maxWait
	l.mu.Unlock()
}

// LookupsPerMinute is the number-lookup budget that goes with a send rate: three lookups per send.
func LookupsPerMinute(rate int) int { return rate * 3 }

// LimiterStats is a snapshot of a Limiter for the admin panel.
type LimiterStats struct {
	PerMinute    int       `json:"per_minute"`
	ChatGapMS    int64     `json:"chat_gap_ms"`
	MaxWaitMS    int64     `json:"max_wait_ms"`
	LastMinute   int       `json:"last_minute"`
	NextFree     time.Time `json:"next_free,omitzero"`
	Rejected     int64     `json:"rejected"`
	LastRejected time.Time `json:"last_rejected,omitzero"`
}

// Stats counts slots reserved in the last minute (including ones still waiting to go out) and
// rejections since startup. NextFree is set when the per-minute budget is used up.
func (l *Limiter) Stats() LimiterStats {
	if l == nil {
		return LimiterStats{}
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := LimiterStats{PerMinute: l.PerMinute, ChatGapMS: l.ChatGap.Milliseconds(), MaxWaitMS: l.MaxWait.Milliseconds(),
		Rejected: l.rejected, LastRejected: l.lastRejected}
	cutoff := now.Add(-time.Minute)
	for _, t := range l.slots {
		if !t.Before(cutoff) {
			s.LastMinute++
		}
	}
	if l.PerMinute > 0 && len(l.slots) >= l.PerMinute {
		if next := l.slots[len(l.slots)-l.PerMinute].Add(time.Minute); next.After(now) {
			s.NextFree = next
		}
	}
	return s
}

func NewLimiter(perMinute int, chatGap, maxWait time.Duration) *Limiter {
	return &Limiter{PerMinute: perMinute, ChatGap: chatGap, MaxWait: maxWait, byChat: map[string]time.Time{}}
}

// Reserve returns when the next message to chat may be sent, or a RateLimitError.
func (l *Limiter) Reserve(chat string) (time.Time, error) { return l.ReserveN(chat, 1) }

// ReserveN reserves n consecutive slots, all or none, and returns when the last one is due.
func (l *Limiter) ReserveN(chat string, n int) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.PerMinute > 0 && n > l.PerMinute {
		return time.Time{}, BadInput("at most %d per minute", l.PerMinute)
	}
	now := time.Now()
	at, slots := l.plan(chat, n, now)
	if wait := at.Sub(now); l.MaxWait > 0 && wait > l.MaxWait {
		l.rejected++
		l.lastRejected = now
		return time.Time{}, RateLimitError{RetryAfter: wait}
	}
	l.commit(chat, at, slots, now)
	return at, nil
}

// Wait reserves n slots for chat and waits until the last one is due.
func (l *Limiter) Wait(ctx context.Context, chat string, n int) error {
	at, err := l.ReserveN(chat, n)
	if err != nil {
		return err
	}
	return SleepUntil(ctx, at)
}

// Peek says how long until a slot for chat would be due, without taking it.
func (l *Limiter) Peek(chat string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	at, _ := l.plan(chat, 1, now)
	return at.Sub(now)
}

// TryReserve takes a slot for chat only if it's due within within, and otherwise says how long until
// one is. It never counts as a rejection: the outbox uses it to take only free slots, so it doesn't
// push interactive sends back.
func (l *Limiter) TryReserve(chat string, within time.Duration) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	at, slots := l.plan(chat, 1, now)
	if wait := at.Sub(now); wait > within {
		return wait, false
	}
	l.commit(chat, at, slots, now)
	return at.Sub(now), true
}

// plan works out when n slots for chat would be due and the slot list that would result.
func (l *Limiter) plan(chat string, n int, now time.Time) (time.Time, []time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(l.slots) && l.slots[i].Before(cutoff) {
		i++
	}
	l.slots = l.slots[i:]

	slots := slices.Clone(l.slots)
	at := now
	if last, ok := l.byChat[chat]; ok && l.ChatGap > 0 {
		at = later(at, last.Add(l.ChatGap))
	}
	for k := range n {
		if k > 0 && l.ChatGap > 0 {
			at = at.Add(l.ChatGap)
		}
		if l.PerMinute > 0 && len(slots) >= l.PerMinute {
			at = later(at, slots[len(slots)-l.PerMinute].Add(time.Minute))
		}
		if l.PerMinute > 0 {
			slots = append(slots, at)
		}
	}
	return at, slots
}

func (l *Limiter) commit(chat string, at time.Time, slots []time.Time, now time.Time) {
	l.slots = slots
	l.byChat[chat] = at
	if len(l.byChat) > 10000 {
		cutoff := now.Add(-time.Minute)
		for k, t := range l.byChat {
			if t.Before(cutoff) {
				delete(l.byChat, k)
			}
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// AutoReplyStats is the auto-reply limiter's snapshot; rejected replies were dropped.
func (b *Bot) AutoReplyStats() LimiterStats { return b.autoLimit.Stats() }

// ThrottleToken applies a token's own send rate (per minute) on top of the global limiter. It is shared
// by the HTTP API and the MCP server, so a token gets one budget however it connects.
func (b *Bot) ThrottleToken(ctx context.Context, id string, rate int) error {
	if rate <= 0 || id == "" {
		return nil
	}
	return b.tokenLimiter(id, rate).Wait(ctx, id, 1)
}

func (b *Bot) tokenLimiter(id string, rate int) *Limiter {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	if b.tokenRate == nil {
		b.tokenRate = map[string]*Limiter{}
	}
	l, ok := b.tokenRate[id]
	if !ok || l.PerMinute != rate {
		l = NewLimiter(rate, 0, 30*time.Second)
		b.tokenRate[id] = l
	}
	return l
}
