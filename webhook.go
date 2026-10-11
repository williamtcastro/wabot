package main

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
	"time"
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
// Delivery is at-least-once and unordered; receivers should dedupe on X-Wabot-Delivery.
type Webhook struct {
	URL    string
	Secret string
	Filter Filter

	log    *slog.Logger
	client *http.Client
	queue  chan webhookJob
}

func NewWebhook(ctx context.Context, url, secret string, filter Filter, log *slog.Logger) *Webhook {
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
func (w *Webhook) Deliver(event string, m Message) {
	f := w.Filter
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
			return
		}
		retryable := err != nil || status == http.StatusTooManyRequests || status >= 500
		w.log.Warn("delivery failed", "delivery", job.id, "attempt", attempt, "status", status, "err", err, "will_retry", retryable && attempt < webhookAttempts)
		if !retryable {
			return
		}
		if sleepUntil(ctx, time.Now().Add(backoff+mrand.N(backoff/2))) != nil {
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
	req.Header.Set("User-Agent", "wabot-webhook/1")
	req.Header.Set("X-Wabot-Event", job.event)
	req.Header.Set("X-Wabot-Delivery", job.id)
	req.Header.Set("X-Wabot-Timestamp", ts)
	if w.Secret != "" {
		req.Header.Set("X-Wabot-Signature", "sha256="+Sign(w.Secret, ts, job.body))
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
