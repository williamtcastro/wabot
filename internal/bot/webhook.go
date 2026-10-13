package bot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

const (
	webhookQueueSize = 1000
	webhookWorkers   = 4
	webhookAttempts  = 6
)

type webhookJob struct {
	event string
	body  []byte
	id    string
}

// Webhook POSTs messages to a URL with retries and an optional HMAC signature.
// Delivery is at-least-once and unordered; receivers should dedupe on X-Wame-Delivery.
type Webhook struct {
	URL    string
	Secret string
	Filter store.Filter

	log    *slog.Logger
	client *http.Client
	queue  chan webhookJob
	mu     sync.RWMutex

	statsMu sync.Mutex
	stats   WebhookStats
}

// WebhookStats counts deliveries since startup and keeps the most recent failures, for the admin
// panel. Nothing here includes the URL, the secret or message content.
type WebhookStats struct {
	Queued      int              `json:"queued"`
	Capacity    int              `json:"capacity"`
	Delivered   int64            `json:"delivered"`
	Retried     int64            `json:"retried"`
	Failed      int64            `json:"failed"`
	Dropped     int64            `json:"dropped"`
	Replayed    int64            `json:"replayed"`
	LastSuccess time.Time        `json:"last_success,omitzero"`
	LastFailure time.Time        `json:"last_failure,omitzero"`
	Recent      []WebhookFailure `json:"recent_failures"`
}

// WebhookFailure is one failed attempt.
type WebhookFailure struct {
	At       time.Time `json:"at"`
	Delivery string    `json:"delivery"`
	Event    string    `json:"event"`
	Attempt  int       `json:"attempt"`
	Status   int       `json:"status,omitempty"`
	Error    string    `json:"error,omitempty"`
	GaveUp   bool      `json:"gave_up"`
	Replayed bool      `json:"replayed,omitempty"`
}

const webhookRecentFailures = 20

// Stats returns a copy of the counters, with the queue length and the newest failures first.
func (w *Webhook) Stats() WebhookStats {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	s := w.stats
	s.Queued, s.Capacity = len(w.queue), cap(w.queue)
	s.Recent = make([]WebhookFailure, len(w.stats.Recent))
	for i, f := range w.stats.Recent {
		s.Recent[len(s.Recent)-1-i] = f
	}
	return s
}

func (w *Webhook) record(f func(s *WebhookStats)) {
	w.statsMu.Lock()
	f(&w.stats)
	w.statsMu.Unlock()
}

func (w *Webhook) recordFailure(f WebhookFailure) {
	w.record(func(s *WebhookStats) {
		s.LastFailure = f.At
		s.Recent = append(s.Recent, f)
		if len(s.Recent) > webhookRecentFailures {
			s.Recent = s.Recent[len(s.Recent)-webhookRecentFailures:]
		}
	})
}

// SetChatIf replaces the filter's chat once a phone number has been resolved to WhatsApp's JID,
// unless the filter was changed in the meantime.
func (w *Webhook) SetChatIf(old, chat string) {
	w.mu.Lock()
	if w.Filter.Chat == old {
		w.Filter.Chat = chat
	}
	w.mu.Unlock()
}

// SetFilter replaces the whole filter (a runtime settings change).
func (w *Webhook) SetFilter(f store.Filter) {
	w.mu.Lock()
	w.Filter = f
	w.mu.Unlock()
}

func NewWebhook(ctx context.Context, url, secret string, filter store.Filter, log *slog.Logger) *Webhook {
	w := &Webhook{
		URL: url, Secret: secret, Filter: filter,
		log: log.With("component", "webhook"),
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue: make(chan webhookJob, webhookQueueSize),
	}
	for range webhookWorkers {
		go w.worker(ctx)
	}
	return w
}

// Deliver queues m if it matches the filter. event is "message" or "update".
func (w *Webhook) Deliver(event string, m store.Message) {
	w.mu.RLock()
	f := w.Filter
	w.mu.RUnlock()
	f.Since = 0
	if !f.Match(m) {
		return
	}
	body, err := json.Marshal(m)
	if err != nil {
		return
	}
	job := webhookJob{event: event, body: body, id: fmt.Sprintf("%d-%s-%s", m.Seq, event, randomHex(4))}
	select {
	case w.queue <- job:
	default:
		w.log.Error("queue full, dropping delivery", "delivery", job.id)
		w.record(func(s *WebhookStats) { s.Dropped++ })
	}
}

func (w *Webhook) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.queue:
			w.send(ctx, job)
		}
	}
}

func (w *Webhook) send(ctx context.Context, job webhookJob) {
	backoff := time.Second
	for attempt := 1; attempt <= webhookAttempts; attempt++ {
		status, err := w.post(ctx, job)
		if err == nil && status < 300 {
			if attempt > 1 {
				w.log.Info("delivered after retry", "delivery", job.id, "attempt", attempt)
			}
			w.record(func(s *WebhookStats) {
				s.Delivered++
				s.LastSuccess = time.Now()
				if attempt > 1 {
					s.Retried++
				}
			})
			return
		}
		retryable := err != nil || status == http.StatusTooManyRequests || status >= 500
		w.log.Warn("delivery failed", "delivery", job.id, "attempt", attempt, "status", status, "err", err, "will_retry", retryable && attempt < webhookAttempts)
		fail := WebhookFailure{At: time.Now(), Delivery: job.id, Event: job.event, Attempt: attempt, Status: status, GaveUp: !retryable || attempt == webhookAttempts}
		if err != nil {
			fail.Error = err.Error()
		}
		w.recordFailure(fail)
		if fail.GaveUp {
			w.record(func(s *WebhookStats) { s.Failed++ })
		}
		if !retryable {
			return
		}
		if SleepUntil(ctx, time.Now().Add(backoff+mrand.N(backoff/2))) != nil {
			return
		}
		backoff *= 2
	}
	w.log.Error("giving up on delivery", "delivery", job.id, "attempts", webhookAttempts)
}

func (w *Webhook) post(ctx context.Context, job webhookJob) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(job.body))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wame-webhook/1")
	req.Header.Set("X-Wame-Event", job.event)
	req.Header.Set("X-Wame-Delivery", job.id)
	req.Header.Set("X-Wame-Timestamp", ts)
	if w.Secret != "" {
		req.Header.Set("X-Wame-Signature", "sha256="+Sign(w.Secret, ts, job.body))
	}
	resp, err := w.client.Do(req)
	if uerr, ok := err.(*url.Error); ok {
		return 0, uerr.Err
	}
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign returns hex(HMAC-SHA256(secret, timestamp + "." + body)).
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WebhookResult is the outcome of a single delivery attempt made on request (test or replay).
type WebhookResult struct {
	Delivery   string `json:"delivery"`
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

func (w *Webhook) once(ctx context.Context, job webhookJob) (WebhookResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	status, err := w.post(ctx, job)
	if err == nil && status >= 300 {
		err = fmt.Errorf("the webhook answered HTTP %d", status)
	}
	r := WebhookResult{Delivery: job.id, Status: status, DurationMS: time.Since(start).Milliseconds(), OK: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	return r, err
}

var errNoWebhook = BadInput("no webhook is configured (-webhook / WAME_WEBHOOK)")

// TestWebhook posts a signed "test" event, so an admin can check the receiver and its signature
// check. It's audited but not counted in the delivery stats.
func (b *Bot) TestWebhook(ctx context.Context, actor string) (WebhookResult, error) {
	if b.Webhook == nil {
		b.Audit(actor, "webhook_test", "", "", errNoWebhook)
		return WebhookResult{}, errNoWebhook
	}
	body, _ := json.Marshal(map[string]any{"event": "test", "at": time.Now(), "note": "a test delivery from wame; there is no message"})
	r, err := b.Webhook.once(ctx, webhookJob{event: "test", body: body, id: "test-" + randomHex(4)})
	b.Audit(actor, "webhook_test", "", fmt.Sprintf("HTTP %d in %d ms", r.Status, r.DurationMS), err)
	return r, err
}

// ReplayWebhook sends a recently failed delivery again, once, with the same delivery id. The body
// is rebuilt from the stored message as it is now; only deliveries in the recent failures list can
// be replayed, so this can't push messages the filter left out.
func (b *Bot) ReplayWebhook(ctx context.Context, actor, delivery string) (WebhookResult, error) {
	if b.Webhook == nil {
		b.Audit(actor, "webhook_replay", "", delivery, errNoWebhook)
		return WebhookResult{}, errNoWebhook
	}
	w := b.Webhook
	var event string
	w.statsMu.Lock()
	for _, f := range w.stats.Recent {
		if f.Delivery == delivery {
			event = f.Event
		}
	}
	w.statsMu.Unlock()
	seqText, _, _ := strings.Cut(delivery, "-")
	seq, perr := strconv.ParseInt(seqText, 10, 64)
	if event == "" || perr != nil {
		err := fmt.Errorf("%w: delivery %q isn't among the recent failures", store.ErrNotFound, delivery)
		b.Audit(actor, "webhook_replay", "", delivery, err)
		return WebhookResult{}, err
	}
	m, err := b.Store.GetBySeq(seq)
	if err != nil {
		b.Audit(actor, "webhook_replay", "", delivery, err)
		return WebhookResult{}, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return WebhookResult{}, err
	}
	r, err := w.once(ctx, webhookJob{event: event, body: body, id: delivery})
	if err == nil {
		w.record(func(s *WebhookStats) {
			s.Replayed++
			s.LastSuccess = time.Now()
			for i := range s.Recent {
				if s.Recent[i].Delivery == delivery {
					s.Recent[i].Replayed = true
				}
			}
		})
	}
	b.Audit(actor, "webhook_replay", m.Chat, delivery, err)
	return r, err
}
