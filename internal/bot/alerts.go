package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

const (
	alertTick    = 5 * time.Second
	alertEvery   = 15 * time.Minute
	alertBackoff = time.Minute
	alertTimeout = 10 * time.Second

	priorityLow    = 2
	priorityNormal = 5
	priorityWarn   = 7
	priorityHigh   = 8
)

// Alert is one notification. It never carries message text, tokens, URLs or contacts' numbers.
type Alert struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

// Notifier delivers alerts to a push service. Its errors must not contain the service's URL or token.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, a Alert) error
}

// Gotify posts alerts to a Gotify server's /message endpoint, with the app token in X-Gotify-Key.
type Gotify struct {
	endpoint string
	host     string
	token    string
	client   *http.Client
}

// NewGotify checks base (http(s), no query or credentials) and that there is a token.
func NewGotify(base, token string) (*Gotify, error) {
	u, err := url.Parse(base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, errors.New("the Gotify URL must be an http(s) URL")
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return nil, errors.New("the Gotify URL must not have a query or credentials; pass the app token with -alert-gotify-token")
	case token == "":
		return nil, errors.New("a Gotify app token is required (-alert-gotify-token / WAME_ALERT_GOTIFY_TOKEN)")
	}
	return &Gotify{
		endpoint: strings.TrimRight(u.String(), "/") + "/message",
		host:     u.Host,
		token:    token,
		client: &http.Client{
			Timeout:       alertTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (g *Gotify) Name() string { return "gotify" }

func (g *Gotify) Notify(ctx context.Context, a Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return g.clean(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wame-alerts/1")
	req.Header.Set("X-Gotify-Key", g.token)
	resp, err := g.client.Do(req)
	if err != nil {
		return g.clean(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gotify answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// clean drops the URL from transport errors, so the address never reaches /stats or the log.
func (g *Gotify) clean(err error) error {
	if dns, ok := errors.AsType[*net.DNSError](err); ok {
		return fmt.Errorf("gotify: lookup failed: %s", dns.Err)
	}
	if op, ok := errors.AsType[*net.OpError](err); ok {
		return fmt.Errorf("gotify: %s: %v", op.Op, op.Err)
	}
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		err = uerr.Err
	}
	msg := strings.ReplaceAll(err.Error(), g.host, "gotify")
	return errors.New(strings.ReplaceAll(msg, g.token, "***"))
}

// AlertStats is the alerter's state for GET /stats. It never holds the URL or the token.
type AlertStats struct {
	Configured  bool      `json:"configured"`
	Backend     string    `json:"backend,omitempty"`
	DelayMS     int64     `json:"delay_ms,omitempty"`
	Sent        int64     `json:"sent"`
	Failed      int64     `json:"failed"`
	LastSent    time.Time `json:"last_sent,omitzero"`
	LastTitle   string    `json:"last_title,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

type connLevel int

const (
	connOK connLevel = iota
	connUnpaired
	connDown
	connLoggedOut
)

// alerter turns the bot's state into alerts. Only RunAlerts' goroutine calls observe; mu guards
// stats, which /stats and TestAlert also touch.
type alerter struct {
	n     Notifier
	delay time.Duration

	mu    sync.Mutex
	stats AlertStats

	primed      bool
	account     string
	downSince   time.Time
	alerted     connLevel
	alertedAt   time.Time
	retryAt     time.Time
	lastEvent   map[string]time.Time
	cooldown    time.Time
	queueFailed int
	hookNoted   int64
}

// alertSnapshot is what one check looks at. queueFailed counts the items that failed since the
// previous check, taken from QueueEvents.
type alertSnapshot struct {
	status      Status
	queue       QueueStats
	queueFailed int
	webhook     *WebhookStats
}

// SetAlerts makes RunAlerts send alerts through n; a disconnection must last delay first.
func (b *Bot) SetAlerts(n Notifier, delay time.Duration) {
	b.alerts = &alerter{n: n, delay: delay, lastEvent: map[string]time.Time{},
		stats: AlertStats{Configured: true, Backend: n.Name(), DelayMS: delay.Milliseconds()}}
}

func (b *Bot) AlertStats() AlertStats {
	if b.alerts == nil {
		return AlertStats{}
	}
	b.alerts.mu.Lock()
	defer b.alerts.mu.Unlock()
	return b.alerts.stats
}

var errNoAlerts = BadInput("no alerts are configured (-alert-gotify / WAME_ALERT_GOTIFY)")

// TestAlert sends a low-priority test alert.
func (b *Bot) TestAlert(ctx context.Context, actor string) error {
	if b.alerts == nil {
		b.Audit(actor, "alert_test", "", "", errNoAlerts)
		return errNoAlerts
	}
	err := b.alerts.send(ctx, Alert{Title: "wame: test alert", Message: "A test alert from wame, sent by " + actor + ".", Priority: priorityLow})
	b.Audit(actor, "alert_test", "", b.alerts.n.Name(), err)
	return err
}

// RunAlerts watches the connection, the send queue and the webhook until ctx is done. It returns at
// once when no alerts are configured.
func (b *Bot) RunAlerts(ctx context.Context) {
	a := b.alerts
	if a == nil {
		return
	}
	ch, unsub := b.Subscribe()
	defer unsub()
	tick := time.NewTicker(alertTick)
	defer tick.Stop()
	failed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-ch:
			switch v := v.(type) {
			case QueueEvent:
				if v.Item.Status == store.OutboxFailed {
					failed++
				}
				continue
			case Status:
			default:
				continue
			}
		case <-tick.C:
		}
		snap := alertSnapshot{status: b.Status(), queue: b.QueueStats(), queueFailed: failed}
		failed = 0
		if b.Webhook != nil {
			s := b.Webhook.Stats()
			snap.webhook = &s
		}
		a.observe(ctx, time.Now(), snap)
	}
}

func (a *alerter) send(ctx context.Context, al Alert) error {
	ctx, cancel := context.WithTimeout(ctx, alertTimeout)
	defer cancel()
	err := a.n.Notify(ctx, al)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.stats.Failed++
		a.stats.LastError, a.stats.LastErrorAt = err.Error(), time.Now()
		return err
	}
	a.stats.Sent++
	a.stats.LastSent, a.stats.LastTitle = time.Now(), al.Title
	return nil
}

// notify sends al unless a recent failure is still backing off; false means try again later.
func (a *alerter) notify(ctx context.Context, now time.Time, al Alert) bool {
	if now.Before(a.retryAt) {
		return false
	}
	if a.account != "" {
		al.Message += "\nAccount: " + a.account
	}
	if a.send(ctx, al) != nil {
		a.retryAt = now.Add(alertBackoff)
		return false
	}
	return true
}

func connectionLevel(s Status) connLevel {
	switch {
	case s.Connected && s.LoggedIn:
		return connOK
	case s.State == "logged_out":
		return connLoggedOut
	case s.JID == "":
		return connUnpaired
	}
	return connDown
}

func (a *alerter) observe(ctx context.Context, now time.Time, s alertSnapshot) {
	if user, _, _ := strings.Cut(s.status.JID, "@"); user != "" {
		a.account = user
	}
	a.observeConnection(ctx, now, s.status)
	a.observeQueue(ctx, now, s.queue, s.queueFailed)
	if s.webhook != nil {
		a.observeWebhook(ctx, now, *s.webhook)
	}
	a.primed = true
}

// observeConnection alerts on a logout at once, on a disconnection once it has lasted the delay, and
// once more when WhatsApp is back after either.
func (a *alerter) observeConnection(ctx context.Context, now time.Time, s Status) {
	level := connectionLevel(s)
	if level != connDown {
		a.downSince = time.Time{}
	}
	switch level {
	case connOK:
		if a.alerted != connOK && a.notify(ctx, now, Alert{Title: "wame: WhatsApp is back",
			Message: fmt.Sprintf("WhatsApp is connected again after %s.", now.Sub(a.alertedAt).Round(time.Second)), Priority: priorityLow}) {
			a.alerted = connOK
		}
	case connLoggedOut:
		if a.alerted != connLoggedOut && a.notify(ctx, now, Alert{Title: "wame: WhatsApp logged out",
			Message: "The linked device was logged out. Link it again with 'wame login' or the admin panel.", Priority: priorityHigh}) {
			if a.alerted == connOK {
				a.alertedAt = now
			}
			a.alerted = connLoggedOut
		}
	case connDown:
		if a.downSince.IsZero() {
			a.downSince = now
		}
		if a.alerted == connOK && now.Sub(a.downSince) >= a.delay && a.notify(ctx, now, Alert{Title: "wame: WhatsApp disconnected",
			Message: fmt.Sprintf("WhatsApp has been disconnected for %s (state: %s).", now.Sub(a.downSince).Round(time.Second), s.State), Priority: priorityWarn}) {
			a.alerted, a.alertedAt = connDown, a.downSince
		}
	}
}

// event sends an alert of kind at most once per alertEvery.
func (a *alerter) event(ctx context.Context, now time.Time, kind string, al Alert) bool {
	if last, ok := a.lastEvent[kind]; ok && now.Sub(last) < alertEvery {
		return false
	}
	if !a.notify(ctx, now, al) {
		return false
	}
	a.lastEvent[kind] = now
	return true
}

func (a *alerter) observeQueue(ctx context.Context, now time.Time, q QueueStats, failed int) {
	if !q.CooldownUntil.IsZero() && !q.CooldownUntil.Equal(a.cooldown) && a.event(ctx, now, "queue_cooldown", Alert{Title: "wame: send queue backing off",
		Message: fmt.Sprintf("The send queue paused until %s after repeated WhatsApp errors.", q.CooldownUntil.Format("15:04:05")), Priority: priorityNormal}) {
		a.cooldown = q.CooldownUntil
	}
	a.queueFailed += failed
	if a.queueFailed > 0 && a.event(ctx, now, "queue_failed", Alert{Title: "wame: queued messages failed",
		Message: fmt.Sprintf("%d queued message(s) failed for good. See GET /queue?status=failed.", a.queueFailed), Priority: priorityNormal}) {
		a.queueFailed = 0
	}
}

func (a *alerter) observeWebhook(ctx context.Context, now time.Time, w WebhookStats) {
	lost := w.Failed + w.Dropped
	if !a.primed {
		a.hookNoted = lost
	}
	if n := lost - a.hookNoted; n > 0 && a.event(ctx, now, "webhook_failed", Alert{Title: "wame: webhook deliveries lost",
		Message: fmt.Sprintf("%d webhook deliveries were given up or dropped. See GET /stats.", n), Priority: priorityNormal}) {
		a.hookNoted = lost
	}
}
