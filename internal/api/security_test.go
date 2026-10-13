package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

// Regression tests for docs/security/SCOPED-TOKENS.md findings.

func fromLAN(hdr map[string]string) map[string]string {
	out := map[string]string{"X-Remote": "192.168.1.66:4000"}
	maps.Copy(out, hdr)
	return out
}

func doFrom(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "localhost:8080"
	r.RemoteAddr = "127.0.0.1:50000"
	for k, v := range hdr {
		switch k {
		case "X-Remote":
			r.RemoteAddr = v
		case "Host":
			r.Host = v
		default:
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestF1TokenlessModeNeverReturns(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	if rec := doFrom(h, "GET", "/status", fromLAN(nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("token-less mode from a LAN address with Host: localhost = %d, want 403", rec.Code)
	}
	issue(t, b, "only", []auth.Scope{auth.Read}, nil, 0)
	if _, err := b.Store.RevokeToken("only"); err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []map[string]string{nil, fromLAN(nil)} {
		if rec := doFrom(h, "GET", "/status", hdr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("after the last token is revoked, no-token requests must still be 401, got %d", rec.Code)
		}
	}
}

func TestF3RevocationEndsOpenStreams(t *testing.T) {
	old := streamRecheck
	streamRecheck = 20 * time.Millisecond
	defer func() { streamRecheck = old }()
	b := newTestBot(t)
	tok := issue(t, b, "watcher", []auth.Scope{auth.Read}, nil, 0)
	srv := httptest.NewServer(newHandler(b, ""))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := b.Store.RevokeToken("watcher"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream stayed open after its token was revoked")
	}
}

func TestF4RestrictedTokensDontSpendLookups(t *testing.T) {
	b := newTestBot(t)
	b.Lookups = bot.NewLimiter(2, 0, time.Millisecond)
	tok := issue(t, b, "maria-only", []auth.Scope{auth.Read, auth.Send}, []string{maria}, 0)
	h := newHandler(b, "")
	for _, probe := range []string{"5521911112222", "5521933334444", "5521955556666"} {
		if rec := do(h, "GET", "/messages?chat="+probe, bearer(tok)); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "@") {
			t.Fatalf("probe %s = %d %s; want a fixed 403", probe, rec.Code, rec.Body)
		}
		if rec := doJSON(h, "POST", "/send", `{"to":"`+probe+`","text":"x"}`, bearer(tok)); rec.Code != http.StatusForbidden {
			t.Fatalf("send probe %s = %d, want 403", probe, rec.Code)
		}
	}
	if _, err := b.Lookups.Reserve("lookup"); err != nil {
		t.Fatalf("restricted probes used up the shared lookup budget: %v", err)
	}
}

// GET /contacts was 403 for tokens limited to chats; it's now filtered to the allowlist, and
// nothing a restricted token sends about contacts is looked up on WhatsApp.
func TestRestrictedContacts(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	admin := bearer(issue(t, b, "boss", []auth.Scope{auth.Admin}, nil, 0))
	mariaBot := bearer(issue(t, b, "maria-bot", []auth.Scope{auth.Read, auth.Send}, []string{maria}, 0))
	for _, to := range []string{maria, john, ana} {
		if w := doJSON(h, "PUT", "/contacts/"+to, `{"name":"x","tags":["clients"],"notes":"private"}`, admin); w.Code != 200 {
			t.Fatalf("PUT = %d %s", w.Code, w.Body)
		}
	}
	lookups := func() bot.LimiterStats { s := b.Lookups.Stats(); s.NextFree = time.Time{}; return s }
	before := lookups()

	for _, target := range []string{"/contacts", "/contacts?tag=clients", "/contacts?q=55", "/contacts?known=1"} {
		w := do(h, "GET", target, mariaBot)
		if got := contactList(t, w); len(got) != 1 || got[0] != maria || strings.Contains(w.Body.String(), "private") {
			t.Errorf("restricted %s = %s", target, w.Body)
		}
	}
	if w := do(h, "GET", "/contacts/tags", mariaBot); w.Code != http.StatusForbidden {
		t.Errorf("restricted /contacts/tags = %d, want 403 (counts span every contact)", w.Code)
	}
	for _, probe := range []string{"5511988887777", john, "5521911112222", "+55 21 93333-4444", "120363012345678901@g.us"} {
		w := do(h, "GET", "/contacts/"+url.PathEscape(probe), mariaBot)
		if w.Code != http.StatusForbidden || errorOf(w) != errChatForbidden {
			t.Errorf("restricted /contacts/%s = %d %s; want the fixed 403", probe, w.Code, w.Body)
		}
	}
	if v := contactView(t, do(h, "GET", "/contacts/5511999999999", mariaBot)); v.JID != maria || v.Own != nil {
		t.Errorf("restricted own chat = %+v", v)
	}
	if after := lookups(); after != before {
		t.Fatalf("restricted contact reads spent lookups: %+v -> %+v", before, after)
	}
}

func TestF5ChildTokensAreBoundToTheirParent(t *testing.T) {
	b := newTestBot(t)
	parent, _, err := auth.Issue(b.Store, auth.Request{Name: "boss", Scopes: []auth.Scope{auth.Admin}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(b, "")
	if rec := doJSON(h, "POST", "/tokens", `{"name":"forever","scopes":["admin"]}`, bearer(parent)); rec.Code != http.StatusForbidden {
		t.Fatalf("a token minting an admin token = %d, want 403", rec.Code)
	}
	rec := doJSON(h, "POST", "/tokens", `{"name":"helper","scopes":["read","send"]}`, bearer(parent))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token   string
		Details struct {
			Expires   time.Time `json:"expires"`
			Rate      int       `json:"rate"`
			CreatedBy string    `json:"created_by"`
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Details.Expires.IsZero() || out.Details.Expires.After(time.Now().Add(time.Hour)) || out.Details.CreatedBy != "boss" {
		t.Fatalf("child must expire with its parent: %+v", out.Details)
	}
	if out.Details.Rate != auth.DefaultRate {
		t.Fatalf("F11: tokens get a default per-token rate, got %d", out.Details.Rate)
	}
	if _, err := b.Store.RevokeToken("boss"); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, "GET", "/status", bearer(out.Token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoking the parent must revoke the child, got %d", rec.Code)
	}
}

func TestF6StatusHidesQRAndAccount(t *testing.T) {
	s := bot.Status{QR: "2@secret", JID: maria, PushName: "Me", State: "waiting_for_qr_scan"}
	if v := visibleStatus(auth.Principal{Scopes: []auth.Scope{auth.Read}}, s); v.QR != "" {
		t.Fatal("non-admins must not see the login QR")
	}
	if v := visibleStatus(auth.Principal{Scopes: []auth.Scope{auth.Read}, Chats: []string{john}}, s); v.JID != "" || v.PushName != "" {
		t.Fatal("restricted tokens must not see the account")
	}
	if v := visibleStatus(auth.Owner(), s); v.QR == "" || v.JID == "" {
		t.Fatal("the owner sees everything")
	}
}

func TestF7QueryTokenOnlyOnBrowserRoutes(t *testing.T) {
	b := newTestBot(t)
	admin, _, _ := auth.Issue(b.Store, auth.Request{Name: "boss", Scopes: []auth.Scope{auth.Admin}})
	h := newHandler(b, "")
	if rec := doJSON(h, "POST", "/tokens?token="+admin, `{"name":"x","scopes":["read"]}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST with ?token= = %d, want 401", rec.Code)
	}
}

func TestF9FailedAttemptsAreThrottled(t *testing.T) {
	h := newHandler(newTestBot(t), "a-long-enough-owner-secret")
	for range failuresPerMinute {
		do(h, "GET", "/status", bearer("guess"))
	}
	if rec := do(h, "GET", "/status", bearer("a-long-enough-owner-secret")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the IP must be throttled, got %d", failuresPerMinute, rec.Code)
	}
	if auth.CheckOwnerSecret("short") == nil || auth.CheckOwnerSecret("") != nil {
		t.Fatal("owner secrets under 16 characters must be rejected")
	}
}

func TestF10StreamsPerTokenAreCapped(t *testing.T) {
	var s streams
	p := auth.Principal{TokenID: "abcdefgh"}
	var releases []func()
	for range maxStreamsPerToken {
		release, ok := s.acquire(p)
		if !ok {
			t.Fatal("under the cap must succeed")
		}
		releases = append(releases, release)
	}
	if _, ok := s.acquire(p); ok {
		t.Fatal("over the cap must fail")
	}
	releases[0]()
	if _, ok := s.acquire(p); !ok {
		t.Fatal("releasing frees a slot")
	}
}

func TestMCPFailedAttemptsShareTheThrottle(t *testing.T) {
	h := newHandler(newTestBot(t), "a-long-enough-owner-secret")
	for range failuresPerMinute {
		do(h, "POST", "/mcp", bearer("guess"))
	}
	if rec := do(h, "GET", "/status", bearer("a-long-enough-owner-secret")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failures on /mcp must count toward the IP block, got %d", rec.Code)
	}
	if rec := do(h, "POST", "/mcp", bearer("a-long-enough-owner-secret")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a blocked IP must be refused on /mcp too, got %d", rec.Code)
	}
}

func TestFailureLimiterSurvivesIPSpray(t *testing.T) {
	var f failureLimiter
	for range failuresPerMinute {
		f.record("192.168.1.66")
	}
	for i := range 3 * failureLimiterPrune {
		f.record(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if !f.blocked("192.168.1.66") {
		t.Fatal("a spray of other addresses must not clear an IP's block")
	}
	if f.blocked("192.168.1.77") || len(f.hits) != 3*failureLimiterPrune+1 {
		t.Fatalf("a lookup must not add an entry: %d tracked", len(f.hits))
	}

	stale := time.Now().Add(-2 * time.Minute)
	for k := range f.hits {
		f.hits[k] = []time.Time{stale}
	}
	if f.blocked("192.168.1.66") || len(f.hits) != 3*failureLimiterPrune {
		t.Fatalf("a lookup of a stale IP must drop it: %d tracked", len(f.hits))
	}
	f.pruneAt = 0
	f.record("192.168.1.66")
	if len(f.hits) != 1 {
		t.Fatalf("stale entries must be pruned, %d left", len(f.hits))
	}
}

func TestStopStreamsEndsOpenStreams(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	done := make(chan int, 2)
	for _, target := range []string{"/events", "/messages/wait?timeout=60"} {
		go func() { done <- doJSON(h, "GET", target, "", nil).Code }()
	}
	time.Sleep(100 * time.Millisecond)
	h.StopStreams()
	for range 2 {
		select {
		case code := <-done:
			if code != 200 {
				t.Fatalf("stream ended with %d", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("StopStreams didn't end an open stream")
		}
	}
}
