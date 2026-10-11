package bot

import (
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

	mu     sync.Mutex
	slots  []time.Time
	byChat map[string]time.Time
}

func NewLimiter(perMinute int, chatGap, maxWait time.Duration) *Limiter {
	return &Limiter{PerMinute: perMinute, ChatGap: chatGap, MaxWait: maxWait, byChat: map[string]time.Time{}}
}

// Reserve returns when the next message to chat may be sent, or a RateLimitError.
func (l *Limiter) Reserve(chat string) (time.Time, error) { return l.ReserveN(chat, 1) }

// ReserveN reserves n consecutive slots, all or none, and returns when the last one is due.
func (l *Limiter) ReserveN(chat string, n int) (time.Time, error) {
	if l.PerMinute > 0 && n > l.PerMinute {
		return time.Time{}, BadInput("at most %d per minute", l.PerMinute)
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

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
	for k := 0; k < n; k++ {
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
	if wait := at.Sub(now); l.MaxWait > 0 && wait > l.MaxWait {
		return time.Time{}, RateLimitError{RetryAfter: wait}
	}

	l.slots = slots
	l.byChat[chat] = at
	if len(l.byChat) > 10000 {
		for k, t := range l.byChat {
			if t.Before(cutoff) {
				delete(l.byChat, k)
			}
		}
	}
	return at, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
