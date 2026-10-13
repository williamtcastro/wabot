package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

var promLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*")*\})? (\S+)$`)

// parseProm checks the exposition format and returns each family's type and its sample lines.
func parseProm(t *testing.T, body string) (map[string]string, map[string][]string) {
	t.Helper()
	types, samples, helped := map[string]string{}, map[string][]string{}, map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			helped[name] = true
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if _, dup := types[name]; dup || !helped[name] {
				t.Fatalf("TYPE for %s repeated or without HELP", name)
			}
			types[name] = typ
		default:
			m := promLine.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("bad line %q", line)
			}
			if _, ok := types[m[1]]; !ok {
				t.Fatalf("sample %s before its TYPE", m[1])
			}
			samples[m[1]] = append(samples[m[1]], line)
		}
	}
	for name, typ := range types {
		if (typ == "counter") != strings.HasSuffix(name, "_total") {
			t.Errorf("%s is a %s", name, typ)
		}
		if len(samples[name]) == 0 {
			t.Errorf("%s has no samples", name)
		}
	}
	return types, samples
}

func TestMetrics(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	sender := issue(t, b, "sender", []auth.Scope{auth.Read, auth.Send}, nil, 0)
	for _, tok := range []string{reader, sender} {
		if w := do(h, "GET", "/metrics", bearer(tok)); w.Code != 403 {
			t.Fatalf("non-admin token: %d", w.Code)
		}
	}
	if w := do(h, "GET", "/metrics", nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if _, err := b.Enqueue("ops", "", "", "", []bot.QueueRequest{{To: "5511999999999", Text: "secret text"}}); err != nil {
		t.Fatal(err)
	}

	w := do(h, "GET", "/metrics", bearer("s3cret-owner-secret"))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("%d %s", w.Code, w.Header())
	}
	body := w.Body.String()
	types, samples := parseProm(t, body)
	for name, typ := range map[string]string{
		"wame_up": "gauge", "wame_connected": "gauge", "wame_logged_in": "gauge", "wame_build_info": "gauge",
		"wame_uptime_seconds": "gauge", "wame_sends_total": "counter", "wame_limiter_last_minute": "gauge",
		"wame_limiter_rejected_total": "counter", "wame_webhook_delivered_total": "counter",
		"wame_webhook_failed_total": "counter", "wame_webhook_dropped_total": "counter",
		"wame_webhook_retried_total": "counter", "wame_webhook_queue_depth": "gauge", "wame_queue_items": "gauge",
		"wame_queue_sent_receipts": "gauge", "wame_open_streams": "gauge", "wame_alerts_sent_total": "counter",
	} {
		if types[name] != typ {
			t.Errorf("%s: type %q, want %q", name, types[name], typ)
		}
	}
	for _, want := range []string{
		"wame_up 1", "wame_connected 0", `wame_build_info{version="test",`, `wame_sends_total{result="ok"} 0`,
		`wame_limiter_rejected_total{limiter="auto_reply"} 0`, `wame_queue_items{state="queued"} 1`,
		`wame_queue_items{state="failed"} 0`, `wame_queue_sent_receipts{receipt="read"} 0`,
		"wame_open_streams 0", "wame_alerts_configured 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if got := samples["wame_queue_items"]; len(got) != len(store.OutboxStates) || strings.Contains(strings.Join(got, "\n"), `"delivered"`) {
		t.Errorf("queue states must be exactly store.OutboxStates, so they add up: %v", got)
	}
	if len(samples["wame_limiter_last_minute"]) != 3 {
		t.Errorf("limiters: %v", samples["wame_limiter_last_minute"])
	}
	for _, leak := range []string{"secret text", "5511999999999", "reader", "sender", "s3cret"} {
		if strings.Contains(body, leak) {
			t.Errorf("metrics leak %q", leak)
		}
	}
}

func TestPromEscape(t *testing.T) {
	var p promWriter
	p.family("wame_x", "gauge", "x.", one(1, "version", "a\"b\\c\nd"))
	if want := `wame_x{version="a\"b\\c\nd"} 1`; !strings.Contains(p.b.String(), want) {
		t.Fatalf("%q", p.b.String())
	}
	parseProm(t, p.b.String())
}

func TestAlertRoutes(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Gotify-Key"))
	}))
	defer srv.Close()
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	owner := bearer("s3cret-owner-secret")
	sender := issue(t, b, "sender", []auth.Scope{auth.Read, auth.Send}, nil, 0)

	if w := doBody(h, "POST", "/alerts/test", "", owner); w.Code != 400 {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body)
	}
	g, err := bot.NewGotify(srv.URL, "A1b2C3d4")
	if err != nil {
		t.Fatal(err)
	}
	b.SetAlerts(g, 0)
	if w := doBody(h, "POST", "/alerts/test", "", bearer(sender)); w.Code != 403 {
		t.Fatalf("send token: %d", w.Code)
	}
	if w := doBody(h, "POST", "/alerts/test", "", owner); w.Code != 200 || len(keys) != 1 || keys[0] != "A1b2C3d4" {
		t.Fatalf("test: %d %s %v", w.Code, w.Body, keys)
	}
	entries, _ := b.Store.AuditLog(1, 0)
	if len(entries) != 1 || entries[0].Action != "alert_test" || !entries[0].OK || entries[0].Actor != "api" {
		t.Fatalf("audit = %+v", entries)
	}

	w := do(h, "GET", "/stats", owner)
	var s Stats
	if json.Unmarshal(w.Body.Bytes(), &s) != nil || !s.Alerts.Configured || s.Alerts.Sent != 1 || s.Alerts.LastSent.IsZero() {
		t.Fatalf("stats: %s", w.Body)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	if w := doBody(h, "POST", "/alerts/test", "", owner); w.Code != 502 || strings.Contains(w.Body.String(), host) {
		t.Fatalf("failed test: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/stats", owner)
	if body := w.Body.String(); strings.Contains(body, host) || strings.Contains(body, "A1b2C3d4") || !strings.Contains(body, `"last_error"`) {
		t.Fatalf("stats leak the Gotify address or token: %s", body)
	}
}
