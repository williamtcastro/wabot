package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const idemTTL = 24 * time.Hour

var errConflict = errors.New("idempotency key was already used for a different request")

func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// idempotency makes repeated requests with the same key return the first result instead of sending twice.
type idempotency struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
}

type idemEntry struct {
	done chan struct{}
	msg  store.Message
	err  error
	fp   string
	at   time.Time
}

// do runs fn once per (scope, key). A repeat with the same request gets the first result; a repeat
// with a different request is rejected, since silently returning the old message would hide a bug.
func (s *idempotency) do(ctx context.Context, scope, key, fp string, fn func() (store.Message, error)) (msg store.Message, err error) {
	if key == "" {
		return fn()
	}
	if len(key) > 200 {
		return store.Message{}, bot.BadInput("idempotency key is longer than 200 characters")
	}
	key = scope + ":" + key
	s.mu.Lock()
	if e, ok := s.entries[key]; ok && time.Since(e.at) < idemTTL {
		s.mu.Unlock()
		if e.fp != fp {
			return store.Message{}, errConflict
		}
		select {
		case <-e.done:
			return e.msg, e.err
		case <-ctx.Done():
			return store.Message{}, ctx.Err()
		}
	}
	e := &idemEntry{done: make(chan struct{}), at: time.Now(), fp: fp}
	s.entries[key] = e
	if len(s.entries) > 10000 {
		for k, old := range s.entries {
			if time.Since(old.at) >= idemTTL {
				delete(s.entries, k)
			}
		}
	}
	s.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			e.err = fmt.Errorf("internal error: %v", r)
		}
		if e.err != nil {
			s.mu.Lock()
			delete(s.entries, key)
			s.mu.Unlock()
		}
		close(e.done)
		msg, err = e.msg, e.err
	}()
	e.msg, e.err = fn()
	return e.msg, e.err
}
