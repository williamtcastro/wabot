package bot

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// LogEntry is one app log record kept in memory for GET /logs.
type LogEntry struct {
	ID    int64             `json:"id"`
	Time  time.Time         `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`

	level slog.Level
}

// LogQuery selects entries: at least Level, newer than Since, older than Before (when set), at most Limit.
type LogQuery struct {
	Level         slog.Level
	Since, Before int64
	Limit         int
}

const (
	redacted    = "[redacted]"
	maxLogValue = 1024
)

// secretKeys are substrings of attribute keys whose values never reach the buffer.
var secretKeys = []string{"token", "secret", "password", "passwd", "authorization", "cookie", "key", "url", "webhook"}

// LogBuffer keeps the newest records an slog.Handler sees (see Wrap), with secrets taken out, so
// admins can read them over the API. Only app logs pass through it; whatsmeow logs separately.
type LogBuffer struct {
	mu      sync.Mutex
	ring    []LogEntry
	next    int
	lastID  int64
	secrets []string
}

func NewLogBuffer(size int) *LogBuffer { return &LogBuffer{ring: make([]LogEntry, 0, max(size, 1))} }

// Scrub replaces each of these strings (the webhook URL, its secret, the owner secret) wherever it
// appears in later records. Empty strings are ignored.
func (l *LogBuffer) Scrub(secrets ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range secrets {
		if s != "" && !slices.Contains(l.secrets, s) {
			l.secrets = append(l.secrets, s)
		}
	}
}

// Wrap returns a handler that passes every record to h unchanged and keeps a copy.
func (l *LogBuffer) Wrap(h slog.Handler) slog.Handler { return &logHandler{inner: h, buf: l} }

// Entries returns the matching entries, newest first.
func (l *LogBuffer) Entries(q LogQuery) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []LogEntry{}
	n := len(l.ring)
	for i := range n {
		e := l.ring[(l.next-1-i+n)%n]
		if e.ID <= q.Since || (q.Limit > 0 && len(out) >= q.Limit) {
			break
		}
		if (q.Before == 0 || e.ID < q.Before) && e.level >= q.Level {
			out = append(out, e)
		}
	}
	return out
}

func (l *LogBuffer) add(r slog.Record, attrs map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range attrs {
		attrs[k] = clip(l.scrub(v))
	}
	l.lastID++
	e := LogEntry{ID: l.lastID, Time: r.Time, Level: strings.ToLower(r.Level.String()), Msg: clip(l.scrub(r.Message)), level: r.Level}
	if len(attrs) > 0 {
		e.Attrs = attrs
	}
	if len(l.ring) < cap(l.ring) {
		l.ring = append(l.ring, e)
	} else {
		l.ring[l.next] = e
	}
	l.next = (l.next + 1) % cap(l.ring)
}

func (l *LogBuffer) scrub(s string) string {
	for _, secret := range l.secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

type logHandler struct {
	inner slog.Handler
	buf   *LogBuffer
	attrs map[string]string
	group string
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for k, v := range h.attrs {
		attrs[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		flatten(attrs, h.group, a)
		return true
	})
	h.buf.add(r, attrs)
	return h.inner.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	attrs := make(map[string]string, len(h.attrs)+len(as))
	for k, v := range h.attrs {
		attrs[k] = v
	}
	for _, a := range as {
		flatten(attrs, h.group, a)
	}
	return &logHandler{inner: h.inner.WithAttrs(as), buf: h.buf, attrs: attrs, group: h.group}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &logHandler{inner: h.inner.WithGroup(name), buf: h.buf, attrs: h.attrs, group: h.group + name + "."}
}

// flatten adds a, with groups as dotted keys, redacting values under keys that look secret.
func flatten(dst map[string]string, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, g := range v.Group() {
			flatten(dst, prefix, g)
		}
		return
	}
	if a.Key == "" {
		return
	}
	key := prefix + a.Key
	if secretKey(key) {
		dst[key] = redacted
		return
	}
	dst[key] = v.String()
}

func secretKey(key string) bool {
	key = strings.ToLower(key)
	return slices.ContainsFunc(secretKeys, func(s string) bool { return strings.Contains(key, s) })
}

func clip(s string) string {
	if len(s) <= maxLogValue {
		return s
	}
	return strings.ToValidUTF8(s[:maxLogValue], "") + "…"
}
