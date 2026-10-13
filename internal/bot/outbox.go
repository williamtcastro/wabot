package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

const (
	maxQueueBatch    = 1000
	maxQueueOpen     = 10000
	maxQueueText     = 65536
	maxQueueAhead    = 30 * 24 * time.Hour
	MaxKeyLength     = 200
	outboxAttempts   = 5
	outboxKeep       = 7 * 24 * time.Hour
	outboxIdle       = time.Minute
	outboxOffline    = 5 * time.Second
	outboxMinStep    = 10 * time.Millisecond
	outboxSlotWithin = 250 * time.Millisecond
	maxOutboxBackoff = 15 * time.Minute
	maxCooldown      = 10 * time.Minute
)

var (
	batchName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

	// ErrKeyReused rejects a repeated idempotency key whose request differs: silently returning the
	// first result would hide a bug in the caller.
	ErrKeyReused = errors.New("idempotency key was already used for a different request")
)

// QueueRequest is one message to queue. A zero SendAt means as soon as the limits allow.
type QueueRequest struct {
	To      string
	Text    string
	ReplyTo string
	SendAt  time.Time
}

// QueueEvent is published (and sent to the webhook as event "queue") when an item changes state.
// It never carries the message text.
type QueueEvent struct{ Item store.OutboxItem }

// QueueStats is the outbox's state for GET /stats and the admin panel.
type QueueStats struct {
	store.OutboxCounts
	Paused        bool      `json:"paused"`
	Running       bool      `json:"running"`
	JitterMS      int64     `json:"jitter_ms"`
	QuietHours    string    `json:"quiet_hours,omitempty"`
	Timezone      string    `json:"timezone"`
	QuietUntil    time.Time `json:"quiet_until,omitzero"`
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
}

// QuietHours is a daily window, in minutes since midnight, when the queue sends nothing. It may
// wrap midnight (21:00-08:00). The zero value is off.
type QuietHours struct{ start, end int }

// ParseQuietHours reads "HH:MM-HH:MM"; "" turns quiet hours off.
func ParseQuietHours(s string) (QuietHours, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return QuietHours{}, nil
	}
	from, to, ok := strings.Cut(s, "-")
	start, err1 := time.Parse("15:04", strings.TrimSpace(from))
	end, err2 := time.Parse("15:04", strings.TrimSpace(to))
	switch {
	case !ok || err1 != nil || err2 != nil:
		return QuietHours{}, fmt.Errorf("%q isn't a window like 21:00-08:00 (empty turns it off)", s)
	case start.Equal(end):
		return QuietHours{}, errors.New("start and end must differ")
	}
	return QuietHours{start.Hour()*60 + start.Minute(), end.Hour()*60 + end.Minute()}, nil
}

func (q QuietHours) On() bool { return q.start != q.end }

func (q QuietHours) String() string {
	if !q.On() {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", q.start/60, q.start%60, q.end/60, q.end%60)
}

// Until is when the window that now falls in ends, read on loc's wall clock, or zero outside it.
func (q QuietHours) Until(now time.Time, loc *time.Location) time.Time {
	if !q.On() {
		return time.Time{}
	}
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	endOn := func(days int) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day()+days, q.end/60, q.end%60, 0, 0, loc)
	}
	switch {
	case q.start < q.end && m >= q.start && m < q.end:
		return endOn(0)
	case q.start > q.end && m >= q.start:
		return endOn(1)
	case q.start > q.end && m < q.end:
		return endOn(0)
	}
	return time.Time{}
}

// ParseTimezone loads an IANA zone name; "", "Local" or zoneName's "Local (…)" is the server's own zone.
func ParseTimezone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" || strings.HasPrefix(name, "Local (") {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q (use an IANA name like America/Sao_Paulo)", name)
	}
	return loc, nil
}

// zoneName shows which zone "Local" really is (in a container without TZ that's UTC), so it's
// clear what quiet hours are measured in.
func zoneName(loc *time.Location) string {
	if loc != time.Local {
		return loc.String()
	}
	abbr, _ := time.Now().In(loc).Zone()
	return "Local (" + abbr + ")"
}

type outbox struct {
	wake      chan struct{}
	failed    atomic.Int64 // items that failed for good, for the alerter
	mu        sync.Mutex
	running   bool
	fails     int
	cooldown  time.Time
	lastErr   string
	lastErrAt time.Time
}

func (b *Bot) wakeOutbox() {
	select {
	case b.outbox.wake <- struct{}{}:
	default:
	}
}

// Enqueue validates and stores messages for the outbox worker. key makes the request safe to retry:
// the same key from the same actor returns the items already queued.
func (b *Bot) Enqueue(actor, tokenID, batch, key string, reqs []QueueRequest) (items []store.OutboxItem, err error) {
	named := batch != ""
	if !named {
		batch = "b-" + randomHex(6)
	}
	defer func() { b.Audit(actor, "queue", batch, fmt.Sprintf("%d messages", len(reqs)), err) }()
	switch {
	case len(reqs) == 0:
		return nil, BadInput("no messages given")
	case len(reqs) > maxQueueBatch:
		return nil, BadInput("at most %d messages per request", maxQueueBatch)
	case !batchName.MatchString(batch):
		return nil, BadInput("batch must be 1-64 letters, digits, '.', '_' or '-'")
	case len(key) > MaxKeyLength:
		return nil, BadInput("idempotency key is longer than %d characters", MaxKeyLength)
	}
	rows := make([]store.OutboxItem, len(reqs))
	for i, r := range reqs {
		if err := validQueueRequest(r); err != nil {
			return nil, BadInput("messages[%d]: %v", i, err)
		}
		rows[i] = store.OutboxItem{Batch: batch, Actor: actor, TokenID: tokenID, To: strings.TrimSpace(r.To), Text: r.Text, ReplyTo: r.ReplyTo, Due: r.SendAt}
		if key != "" {
			rows[i].Key = fmt.Sprintf("%s#%d", key, i)
		}
	}
	c, err := b.Store.CountOutbox(store.OutboxFilter{Actor: actor})
	if err != nil {
		return nil, err
	}
	if open := c.Queued + c.Sending; open+len(reqs) > maxQueueOpen {
		return nil, BadInput("%s already has %d messages waiting; at most %d may be", actor, open, maxQueueOpen)
	}
	if items, err = b.Store.Enqueue(rows); err != nil {
		return nil, err
	}
	for i, it := range items {
		r := rows[i]
		if (named && it.Batch != r.Batch) || it.To != r.To || it.Text != r.Text || it.ReplyTo != r.ReplyTo {
			return nil, ErrKeyReused
		}
	}
	b.wakeOutbox()
	return items, nil
}

func validQueueRequest(r QueueRequest) error {
	switch {
	case strings.TrimSpace(r.Text) == "":
		return errors.New("text is empty")
	case utf8.RuneCountInString(r.Text) > maxQueueText:
		return fmt.Errorf("text is longer than %d characters", maxQueueText)
	case r.SendAt.After(time.Now().Add(maxQueueAhead)):
		return fmt.Errorf("send_at is more than %d days ahead", int(maxQueueAhead.Hours()/24))
	}
	_, err := ParseRecipient(r.To)
	return err
}

// NotQueuedError is CancelQueued's answer for an item (within its filter) that is no longer queued.
// It unwraps to store.ErrNotFound for callers that only check that.
type NotQueuedError struct {
	ID     int64
	Status string
}

func (e NotQueuedError) Error() string {
	return fmt.Sprintf("only queued messages can be canceled; #%d is %s", e.ID, e.Status)
}

func (e NotQueuedError) Unwrap() error { return store.ErrNotFound }

// CancelQueued cancels queued items: one by id (within f), or every queued one matching f.
func (b *Bot) CancelQueued(actor string, id int64, f store.OutboxFilter) (items []store.OutboxItem, err error) {
	target := f.Batch
	if id > 0 {
		target = fmt.Sprintf("#%d", id)
	}
	items, err = b.Store.CancelOutbox(id, f)
	if err == nil && len(items) == 0 && id > 0 {
		err = store.ErrNotFound
		if it, gerr := b.Store.OutboxItem(id); gerr == nil && (f.Actor == "" || it.Actor == f.Actor) && (f.Batch == "" || it.Batch == f.Batch) {
			err = NotQueuedError{ID: id, Status: it.Status}
		}
	}
	b.Audit(actor, "queue_cancel", target, fmt.Sprintf("%d canceled", len(items)), err)
	for _, it := range items {
		b.queueEvent(it)
	}
	return items, err
}

func (b *Bot) QueueStats() QueueStats {
	c, err := b.Store.CountOutbox(store.OutboxFilter{})
	if err != nil {
		b.Log.Error("outbox: count", "err", err)
	}
	s := b.currentSettings()
	cooldown := b.queueCooldown()
	b.outbox.mu.Lock()
	defer b.outbox.mu.Unlock()
	return QueueStats{OutboxCounts: c, Paused: s.QueuePaused, Running: b.outbox.running, JitterMS: s.QueueJitter.Milliseconds(),
		QuietHours: s.QuietHours.String(), Timezone: zoneName(s.location()), QuietUntil: s.QuietHours.Until(time.Now(), s.location()),
		CooldownUntil: cooldown, LastError: b.outbox.lastErr, LastErrorAt: b.outbox.lastErrAt}
}

// queueCooldown is when the queue's back-off after repeated errors ends, or zero.
func (b *Bot) queueCooldown() time.Time {
	b.outbox.mu.Lock()
	defer b.outbox.mu.Unlock()
	if time.Now().Before(b.outbox.cooldown) {
		return b.outbox.cooldown
	}
	return time.Time{}
}

func (b *Bot) currentSettings() Settings {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	return b.settings
}

func (b *Bot) queueEvent(it store.OutboxItem) {
	it.Text = ""
	b.publish(QueueEvent{Item: it})
	if b.Webhook != nil && (it.Status == store.OutboxSent || it.Status == store.OutboxFailed) {
		if body, err := json.Marshal(it); err == nil {
			b.Webhook.enqueue(webhookJob{event: "queue", body: body, id: fmt.Sprintf("q%d-%s-%s", it.ID, it.Progress(), randomHex(4))})
		}
	}
}

// outboxReceipt records a delivery or read receipt for queued messages. It runs inline in the
// event handler: one indexed UPDATE is quick, and doing it in order keeps a read from landing
// before its delivery.
func (b *Bot) outboxReceipt(ids []types.MessageID, read bool, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = string(id)
	}
	items, err := b.Store.MarkOutboxReceipt(strs, read, at)
	if err != nil {
		b.Log.Error("outbox: receipt", "err", err)
		return
	}
	for _, it := range items {
		// Only sent items report receipts: a failed or retried one already had its final event.
		if it.Status == store.OutboxSent {
			b.queueEvent(it)
		}
	}
}

// RunOutbox sends queued messages until ctx is done. Only one should run per database.
func (b *Bot) RunOutbox(ctx context.Context) {
	if n, err := b.Store.RequeueSending(); err != nil {
		b.Log.Error("outbox: requeue", "err", err)
	} else if n > 0 {
		b.Log.Info("outbox: requeued items left sending", "count", n)
	}
	b.setOutboxRunning(true)
	defer b.setOutboxRunning(false)
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	b.pruneOutbox()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			b.pruneOutbox()
			continue
		case <-b.outbox.wake:
		case <-timer.C:
		}
		timer.Reset(b.outboxStep(ctx))
	}
}

func (b *Bot) setOutboxRunning(on bool) {
	b.outbox.mu.Lock()
	b.outbox.running = on
	b.outbox.mu.Unlock()
}

func (b *Bot) pruneOutbox() {
	if n, err := b.Store.PruneOutbox(time.Now().Add(-outboxKeep)); err != nil {
		b.Log.Error("outbox: prune", "err", err)
	} else if n > 0 {
		b.Log.Info("outbox: pruned finished items", "count", n)
	}
}

// outboxStep sends at most one message and returns how long to wait before the next step.
func (b *Bot) outboxStep(ctx context.Context) time.Duration {
	s := b.currentSettings()
	quietUntil := s.QuietHours.Until(time.Now(), s.location())
	switch {
	case s.QueuePaused:
		return outboxIdle
	case !quietUntil.IsZero():
		return min(max(time.Until(quietUntil), outboxMinStep), outboxIdle)
	case !b.Client.IsConnected() || !b.Client.IsLoggedIn():
		return outboxOffline
	}
	b.outbox.mu.Lock()
	cooldown := time.Until(b.outbox.cooldown)
	b.outbox.mu.Unlock()
	if cooldown > 0 {
		return cooldown
	}
	items, err := b.Store.DueOutbox(time.Now(), 50)
	if err != nil {
		b.Log.Error("outbox: read", "err", err)
		return outboxOffline
	}
	if len(items) == 0 {
		next, err := b.Store.NextOutboxDue()
		if err != nil || next.IsZero() {
			return outboxIdle
		}
		return min(max(time.Until(next), outboxMinStep), outboxIdle)
	}
	wait := outboxIdle
	tokens := map[string]*store.Token{}
	for _, it := range items {
		tok, err := b.queueToken(it.TokenID, tokens)
		if err != nil {
			b.finishQueued(it, store.OutboxResult{Status: store.OutboxFailed, Error: "the token that queued it: " + err.Error()})
			continue
		}
		jid, err := b.resolveOnline(ctx, it.To)
		limited, isLimited := errors.AsType[RateLimitError](err)
		switch {
		case isLimited:
			wait = min(wait, limited.RetryAfter)
			continue
		case err != nil && !b.Client.IsLoggedIn():
			return outboxOffline
		case err != nil:
			// Let the attempt record the error: a bad number fails, anything else is retried later.
			b.sendQueued(ctx, it)
			return jitter(s.QueueJitter)
		}
		chat := b.PhoneJID(jid).String()
		if err := b.optedOut(jid, it.To); err != nil {
			if _, isInput := errors.AsType[InputError](err); !isInput {
				b.Log.Error("outbox: opt-out check", "err", err)
				return outboxOffline
			}
			b.finishQueued(it, store.OutboxResult{Status: store.OutboxFailed, Chat: chat, Error: err.Error()})
			continue
		}
		d, ok := b.outboxSlot(tok, chat)
		if !ok {
			wait = min(wait, d)
			continue
		}
		if SleepUntil(ctx, time.Now().Add(d)) != nil {
			return outboxIdle
		}
		b.sendQueued(ctx, it)
		return jitter(s.QueueJitter)
	}
	return max(wait, outboxMinStep)
}

// queueToken looks up the token an item was queued with (nil for the owner or the CLI), once per
// step, and says why it can't be used any more.
func (b *Bot) queueToken(id string, seen map[string]*store.Token) (*store.Token, error) {
	if id == "" {
		return nil, nil
	}
	t, ok := seen[id]
	if !ok {
		found, err := b.Store.TokenByID(id)
		if err != nil {
			return nil, errors.New("no longer exists")
		}
		t, seen[id] = &found, &found
	}
	return t, t.Usable(time.Now())
}

// outboxSlot takes a send slot for chat if one is free now under both the token's own rate and the
// global limiter; checking both first keeps one from being taken while the other is busy.
func (b *Bot) outboxSlot(tok *store.Token, chat string) (time.Duration, bool) {
	var own *Limiter
	if tok != nil && tok.Rate > 0 {
		own = b.tokenLimiter(tok.ID, tok.Rate)
	}
	wait := b.Limiter.Peek(chat)
	if own != nil {
		wait = max(wait, own.Peek(tok.ID))
	}
	if wait > outboxSlotWithin {
		return wait, false
	}
	if own != nil {
		if d, ok := own.TryReserve(tok.ID, outboxSlotWithin); !ok {
			return d, false
		}
	}
	return b.Limiter.TryReserve(chat, outboxSlotWithin)
}

func (b *Bot) sendQueued(ctx context.Context, it store.OutboxItem) {
	it, err := b.Store.ClaimOutbox(it.ID, string(b.Client.GenerateMessageID()))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			b.Log.Error("outbox: claim", "id", it.ID, "err", err)
		}
		return
	}
	b.queueEvent(it)
	m, err := b.SendText(ctx, it.Actor, it.To, it.Text, SendOptions{ReplyTo: it.ReplyTo, queued: it.ID, messageID: types.MessageID(it.MessageID)})
	if err == nil {
		b.outboxResult(true, "")
		b.finishQueued(it, store.OutboxResult{Status: store.OutboxSent, Chat: m.Chat})
		return
	}
	if ctx.Err() != nil {
		return // shutting down: it stays in sending and is requeued on the next start
	}
	b.finishQueued(it, b.attemptResult(it, err))
}

// attemptResult turns a failed attempt's error into what happens to the item next.
func (b *Bot) attemptResult(it store.OutboxItem, err error) store.OutboxResult {
	r := store.OutboxResult{Status: store.OutboxQueued, Error: err.Error()}
	_, isInput := errors.AsType[InputError](err)
	limited, isLimited := errors.AsType[RateLimitError](err)
	switch {
	case isInput:
		r.Status = store.OutboxFailed
	case isLimited:
		r.Due, r.Refund = time.Now().Add(limited.RetryAfter), true
	case errors.Is(err, errOptOutCheck):
		b.Log.Error("outbox: opt-out check", "id", it.ID, "err", err)
		r.Due, r.Refund = time.Now().Add(outboxOffline), true
	case !b.Client.IsLoggedIn() || !b.Client.IsConnected():
		r.Refund = true
	default:
		b.outboxResult(false, err.Error())
		if it.Attempts >= outboxAttempts {
			r.Status = store.OutboxFailed
		} else {
			r.Due = time.Now().Add(outboxBackoff(it.Attempts))
		}
	}
	return r
}

func (b *Bot) finishQueued(it store.OutboxItem, r store.OutboxResult) {
	done, err := b.Store.FinishOutbox(it.ID, r)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return // canceled meanwhile
	case err != nil:
		b.Log.Error("outbox: update", "id", it.ID, "err", err)
		return
	case r.Status == store.OutboxFailed:
		b.Log.Warn("outbox: gave up", "id", it.ID, "batch", it.Batch, "err", r.Error)
		if r.Error != errOptedOut {
			b.outbox.failed.Add(1)
		}
	}
	b.queueEvent(done)
}

// outboxResult tracks consecutive WhatsApp errors. From the second in a row the whole queue backs off
// (30 s, 1 min, 2 min … up to 10 min), since a run of failures usually means WhatsApp is pushing back.
func (b *Bot) outboxResult(ok bool, errText string) {
	b.outbox.mu.Lock()
	defer b.outbox.mu.Unlock()
	if ok {
		b.outbox.fails, b.outbox.cooldown = 0, time.Time{}
		return
	}
	b.outbox.fails++
	b.outbox.lastErr, b.outbox.lastErrAt = errText, time.Now()
	if b.outbox.fails >= 2 {
		d := min(15*time.Second<<min(b.outbox.fails-1, 10), maxCooldown)
		b.outbox.cooldown = time.Now().Add(d)
		b.Log.Warn("outbox: backing off after repeated errors", "errors", b.outbox.fails, "for", d)
	}
}

func outboxBackoff(attempts int) time.Duration {
	return min(30*time.Second<<min(max(attempts-1, 0), 10), maxOutboxBackoff)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return mrand.N(d)
}
