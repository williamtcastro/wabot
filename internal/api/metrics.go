package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/williamtcastro/wame/internal/bot"
)

// promWriter writes the Prometheus text exposition format 0.0.4.
type promWriter struct{ b strings.Builder }

type sample struct {
	labels []string // name, value pairs
	value  float64
}

func one(v float64, labels ...string) sample { return sample{labels, v} }

func (p *promWriter) family(name, typ, help string, samples ...sample) {
	fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	for _, s := range samples {
		p.b.WriteString(name)
		if len(s.labels) > 0 {
			p.b.WriteByte('{')
			for i := 0; i+1 < len(s.labels); i += 2 {
				if i > 0 {
					p.b.WriteByte(',')
				}
				fmt.Fprintf(&p.b, "%s=\"%s\"", s.labels[i], promEscape(s.labels[i+1]))
			}
			p.b.WriteByte('}')
		}
		p.b.WriteByte(' ')
		p.b.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
		p.b.WriteByte('\n')
	}
}

func (p *promWriter) gauge(name, help string, v float64) { p.family(name, "gauge", help, one(v)) }

var promEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func promEscape(s string) string { return promEscaper.Replace(s) }

func boolValue(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

// metrics serves GET /metrics. Per-token counts are left out on purpose: token names would make the
// series unbounded and reveal who uses the API.
func (a *server) metrics(w http.ResponseWriter, r *http.Request) {
	var p promWriter
	st := a.b.Status()
	p.gauge("wame_up", "Always 1 while wame answers.", 1)
	p.family("wame_build_info", "gauge", "Build information.", one(1, "version", a.version, "goversion", runtime.Version()))
	p.gauge("wame_start_time_seconds", "When wame started, in Unix seconds.", float64(a.b.Started.Unix()))
	p.gauge("wame_uptime_seconds", "Seconds since wame started.", time.Since(a.b.Started).Seconds())
	p.gauge("wame_connected", "1 while connected to WhatsApp.", boolValue(st.Connected))
	p.gauge("wame_logged_in", "1 while the linked device is logged in.", boolValue(st.LoggedIn))

	ok, failed := a.b.SendTotals()
	p.family("wame_sends_total", "counter", "Messages and files sent since startup, by result.",
		one(float64(ok), "result", "ok"), one(float64(failed), "result", "failed"))

	limits := []struct {
		name string
		s    bot.LimiterStats
	}{{"send", a.b.Limiter.Stats()}, {"lookups", a.b.Lookups.Stats()}, {"auto_reply", a.b.AutoReplyStats()}}
	var lastMinute, perMinute, rejected []sample
	for _, l := range limits {
		lastMinute = append(lastMinute, one(float64(l.s.LastMinute), "limiter", l.name))
		perMinute = append(perMinute, one(float64(l.s.PerMinute), "limiter", l.name))
		rejected = append(rejected, one(float64(l.s.Rejected), "limiter", l.name))
	}
	p.family("wame_limiter_last_minute", "gauge", "Slots taken in the last minute.", lastMinute...)
	p.family("wame_limiter_per_minute", "gauge", "Configured per-minute limit (0 = unlimited).", perMinute...)
	p.family("wame_limiter_rejected_total", "counter", "Requests refused (429) or dropped by the limiter since startup.", rejected...)

	var hook bot.WebhookStats
	if a.b.Webhook != nil {
		hook = a.b.Webhook.Stats()
	}
	p.gauge("wame_webhook_configured", "1 when a webhook is configured.", boolValue(a.b.Webhook != nil))
	p.family("wame_webhook_delivered_total", "counter", "Webhook deliveries accepted by the receiver.", one(float64(hook.Delivered)))
	p.family("wame_webhook_retried_total", "counter", "Webhook deliveries that needed more than one attempt.", one(float64(hook.Retried)))
	p.family("wame_webhook_failed_total", "counter", "Webhook deliveries given up on.", one(float64(hook.Failed)))
	p.family("wame_webhook_dropped_total", "counter", "Webhook deliveries dropped because the queue was full.", one(float64(hook.Dropped)))
	p.gauge("wame_webhook_queue_depth", "Webhook deliveries waiting.", float64(hook.Queued))

	q := a.b.QueueStats()
	p.family("wame_queue_items", "gauge", "Send queue items by state (finished ones are pruned after 7 days).", queueStates(q)...)
	p.gauge("wame_queue_paused", "1 while the send queue is paused.", boolValue(q.Paused))
	p.gauge("wame_queue_cooldown", "1 while the send queue backs off after repeated WhatsApp errors.", boolValue(!q.CooldownUntil.IsZero()))

	al := a.b.AlertStats()
	p.gauge("wame_alerts_configured", "1 when alerts are configured.", boolValue(al.Configured))
	p.family("wame_alerts_sent_total", "counter", "Alerts delivered since startup.", one(float64(al.Sent)))
	p.family("wame_alerts_failed_total", "counter", "Alerts that couldn't be delivered since startup.", one(float64(al.Failed)))

	p.gauge("wame_open_streams", "Open /events and /messages/wait connections.", float64(a.streams.total()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, p.b.String())
}

// queueStates reads every numeric count from the queue's JSON, so states added later show up too.
func queueStates(q bot.QueueStats) []sample {
	raw, _ := json.Marshal(q.OutboxCounts)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	var out []sample
	for k, v := range fields {
		if n, ok := v.(float64); ok {
			out = append(out, one(n, "state", k))
		}
	}
	slices.SortFunc(out, func(x, y sample) int { return strings.Compare(x.labels[1], y.labels[1]) })
	return out
}

func (s *streams) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.byName {
		n += c
	}
	return n
}
