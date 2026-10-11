package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"
)

const (
	maxUpload   = 64 << 20
	maxPollWait = 120 * time.Second
	idemTTL     = 24 * time.Hour
	apiActor    = "api"
	maxPageSize = 1000
)

//go:embed docs/openapi.yaml
var openapiSpec []byte

const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>wabot API</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"
  integrity="sha384-wxLW6kwyHktdDGr6Pv1zgm/VGJh99lfUbzSn6HNHBENZlCN7W602k9VkGdxuFvPn" crossorigin="anonymous">
</head><body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"
  integrity="sha384-wmyclcVGX/WhUkdkATwhaK1X1JtiNrr2EoYJ+diV3vj4v6OC5yCeSu+yW13SYJep" crossorigin="anonymous"></script>
<script>SwaggerUIBundle({url: "openapi.yaml", dom_id: "#ui"});</script>
</body></html>`

type api struct {
	b    *Bot
	idem idempotency
}

func NewAPI(b *Bot, token string) http.Handler {
	a := &api{b: b, idem: idempotency{entries: map[string]*idemEntry{}}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, b.Status()) })
	mux.HandleFunc("GET /qr", a.qr)
	mux.HandleFunc("POST /pair", a.pair)
	mux.HandleFunc("POST /logout", a.logout)

	mux.HandleFunc("POST /send", a.sendText)
	mux.HandleFunc("POST /send/file", a.sendFile)
	mux.HandleFunc("POST /react", a.react)
	mux.HandleFunc("POST /edit", a.edit)
	mux.HandleFunc("POST /delete", a.delete)
	mux.HandleFunc("POST /read", a.read)
	mux.HandleFunc("POST /typing", a.typing)

	mux.HandleFunc("GET /messages", a.messages)
	mux.HandleFunc("GET /messages/wait", a.wait)
	mux.HandleFunc("GET /events", a.events)
	mux.HandleFunc("GET /media/{seq}", a.media)
	mux.HandleFunc("GET /chats", a.chats)

	mux.HandleFunc("GET /contacts", a.contacts)
	mux.HandleFunc("GET /groups", a.groups)
	mux.HandleFunc("POST /check", a.check)
	mux.HandleFunc("GET /audit", a.audit)

	root := http.NewServeMux()
	root.Handle("/", withAuth(token, mux))
	root.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, docsPage)
	})
	root.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	return withRequestLog(b.Log, root)
}

func (a *api) qr(w http.ResponseWriter, r *http.Request) {
	s := a.b.Status()
	if s.LoggedIn || s.QR == "" {
		writeError(w, fmt.Errorf("%w: no QR available (state: %s)", errNotFound, s.State))
		return
	}
	code, err := qr.Encode(s.QR, qr.M)
	if err != nil {
		writeError(w, err)
		return
	}
	code.Scale = 8
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(code.PNG())
}

func (a *api) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	code, err := a.b.PairPhone(r.Context(), apiActor, req.Phone)
	respond(w, map[string]string{"code": code}, err)
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	err := a.b.Client.Logout(r.Context())
	a.b.Audit(apiActor, "logout", "", "", err)
	respond(w, map[string]bool{"ok": true}, err)
}

func (a *api) sendText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To             string `json:"to"`
		Text           string `json:"text"`
		ReplyTo        string `json:"reply_to"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.To == "" || req.Text == "" {
		writeError(w, badInput("'to' and 'text' are required"))
		return
	}
	key := cmp.Or(r.Header.Get("Idempotency-Key"), req.IdempotencyKey)
	m, err := a.idem.do(r.Context(), "send", key, fingerprint(req.To, req.Text, req.ReplyTo), func() (Message, error) {
		return a.b.SendText(r.Context(), apiActor, req.To, req.Text, SendOptions{ReplyTo: req.ReplyTo})
	})
	respond(w, m, err)
}

func (a *api) sendFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, badInput("invalid multipart form: %v", err))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, badInput("multipart field 'file' is required"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		writeError(w, badInput("read upload: %v", err))
		return
	}
	if len(data) > maxUpload {
		writeError(w, badInput("file is larger than %d MB", maxUpload>>20))
		return
	}
	to := r.FormValue("to")
	if to == "" {
		writeError(w, badInput("'to' is required"))
		return
	}
	key := cmp.Or(r.Header.Get("Idempotency-Key"), r.FormValue("idempotency_key"))
	sum := sha256.Sum256(data)
	m, err := a.idem.do(r.Context(), "file", key, fingerprint(to, hdr.Filename, r.FormValue("caption"), r.FormValue("reply_to"), string(sum[:])), func() (Message, error) {
		return a.b.SendFile(r.Context(), apiActor, to, data, hdr.Filename, r.FormValue("caption"), SendOptions{ReplyTo: r.FormValue("reply_to")})
	})
	respond(w, m, err)
}

type targetReq struct {
	Chat  string   `json:"chat"`
	ID    string   `json:"id"`
	IDs   []string `json:"ids"`
	Emoji string   `json:"emoji"`
	Text  string   `json:"text"`
	State string   `json:"state"`
}

func (a *api) target(w http.ResponseWriter, r *http.Request) (targetReq, bool) {
	var req targetReq
	if !readJSON(w, r, &req) {
		return req, false
	}
	if req.Chat == "" {
		writeError(w, badInput("'chat' is required"))
		return req, false
	}
	return req, true
}

func (a *api) react(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.React(r.Context(), apiActor, req.Chat, req.ID, req.Emoji)
		respond(w, m, err)
	}
}

func (a *api) edit(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.Edit(r.Context(), apiActor, req.Chat, req.ID, req.Text)
		respond(w, m, err)
	}
}

func (a *api) delete(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.Delete(r.Context(), apiActor, req.Chat, req.ID)
		respond(w, m, err)
	}
}

func (a *api) read(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		if req.ID != "" {
			req.IDs = append(req.IDs, req.ID)
		}
		n, err := a.b.MarkRead(r.Context(), apiActor, req.Chat, req.IDs)
		respond(w, map[string]int{"marked": n}, err)
	}
}

func (a *api) typing(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		respond(w, map[string]bool{"ok": true}, a.b.SetTyping(r.Context(), req.Chat, req.State))
	}
}

func (a *api) filter(w http.ResponseWriter, r *http.Request) (Filter, bool) {
	f, err := ParseFilter(r.URL.Query(), func(s string) (string, error) {
		jid, err := a.b.Resolve(r.Context(), s)
		if err != nil {
			if jid, err = ParseRecipient(s); err != nil {
				return "", err
			}
		}
		return a.b.phoneJID(jid).String(), nil
	})
	if err != nil {
		writeError(w, badInput("%v", err))
		return f, false
	}
	return f, true
}

func intParam(r *http.Request, name string, def, maxVal int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, maxVal)
}

func (a *api) messages(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filter(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Has("since")
	writeJSON(w, 200, a.b.Query(f, intParam(r, "limit", 50, maxPageSize), cursor))
}

func (a *api) wait(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filter(w, r)
	if !ok {
		return
	}
	if !r.URL.Query().Has("since") {
		f.Since = a.b.LastSeq()
	}
	limit := intParam(r, "limit", 100, maxPageSize)
	timeout := time.Duration(intParam(r, "timeout", 30, int(maxPollWait/time.Second))) * time.Second

	ch, unsub := a.b.Subscribe()
	defer unsub()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if msgs := a.b.Query(f, limit, true); len(msgs) > 0 {
			writeJSON(w, 200, map[string]any{"messages": msgs, "next_since": msgs[len(msgs)-1].Seq})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			writeJSON(w, 200, map[string]any{"messages": []Message{}, "next_since": f.Since})
			return
		case <-ch:
		}
	}
}

func (a *api) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	f, ok := a.filter(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	// An EventSource reconnects to the same URL with Last-Event-ID, which must win over ?since=.
	if last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && last >= 0 {
		f.Since = last
	} else if !q.Has("since") {
		f.Since = a.b.LastSeq()
	}
	want := map[string]bool{"message": true, "update": true, "status": true}
	if v := q.Get("events"); v != "" {
		want = map[string]bool{}
		for _, e := range strings.Split(v, ",") {
			want[strings.TrimSpace(e)] = true
		}
	}

	ch, unsub := a.b.Subscribe()
	defer unsub()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")

	send := func(event, id string, v any) {
		data, _ := json.Marshal(v)
		if id != "" {
			fmt.Fprintf(w, "id: %s\n", id)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	}
	catchUp := func() {
		if !want["message"] {
			return
		}
		for {
			msgs := a.b.Query(f, 500, true)
			for _, m := range msgs {
				send("message", strconv.FormatInt(m.Seq, 10), m)
				f.Since = m.Seq
			}
			if len(msgs) < 500 {
				return
			}
		}
	}
	catchUp()
	_ = rc.Flush()

	unfiltered := f
	unfiltered.Since = 0
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case v := <-ch:
			switch v := v.(type) {
			case Message, ChatsChanged:
				catchUp()
			case MessageUpdate:
				if want["update"] && unfiltered.Match(v.Message) {
					send("update", "", v.Message)
				}
			case Status:
				if want["status"] {
					send("status", "", v)
				}
			}
		}
		_ = rc.Flush()
	}
}

func (a *api) media(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeError(w, badInput("seq must be a number"))
		return
	}
	data, m, err := a.b.DownloadMedia(r.Context(), seq)
	var dl whatsmeow.DownloadHTTPError
	if errors.As(err, &dl) && dl.Response != nil && slices.Contains([]int{403, 404, 410}, dl.Response.StatusCode) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "media expired on WhatsApp's servers"})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	ctype, disposition := m.Media.Mimetype, "inline"
	if !inlineSafe(ctype) {
		ctype, disposition = "application/octet-stream", "attachment"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": mediaFileName(m)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(data)
}

func inlineSafe(mimetype string) bool {
	base, _, _ := strings.Cut(strings.ToLower(mimetype), ";")
	base = strings.TrimSpace(base)
	switch {
	case base == "image/svg+xml":
		return false
	case strings.HasPrefix(base, "image/"), strings.HasPrefix(base, "audio/"), strings.HasPrefix(base, "video/"), base == "application/pdf":
		return true
	}
	return false
}

func (a *api) chats(w http.ResponseWriter, r *http.Request) {
	chats, err := a.b.Chats(r.Context(), intParam(r, "limit", 50, maxPageSize))
	respond(w, chats, err)
}

func (a *api) contacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := a.b.Contacts(r.Context(), r.URL.Query().Get("q"))
	respond(w, contacts, err)
}

func (a *api) groups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.b.Client.GetJoinedGroups(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	type item struct {
		JID     string `json:"jid"`
		Name    string `json:"name"`
		Members int    `json:"members"`
	}
	out := make([]item, len(groups))
	for i, g := range groups {
		out[i] = item{g.JID.String(), g.Name, len(g.Participants)}
	}
	writeJSON(w, 200, out)
}

func (a *api) check(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phones []string `json:"phones"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Phones) == 0 || len(req.Phones) > 50 {
		writeError(w, badInput("send between 1 and 50 phones"))
		return
	}
	phones := make([]string, len(req.Phones))
	for i, p := range req.Phones {
		phones[i] = "+" + digits(p)
	}
	if err := a.b.lookup(r.Context(), len(phones)); err != nil {
		writeError(w, err)
		return
	}
	res, err := a.b.Client.IsOnWhatsApp(r.Context(), phones)
	if err != nil {
		writeError(w, err)
		return
	}
	type item struct {
		Query string `json:"query"`
		IsIn  bool   `json:"on_whatsapp"`
		JID   string `json:"jid"`
	}
	out := make([]item, len(res))
	for i, x := range res {
		jid := ""
		if x.IsIn {
			jid = x.JID.String()
		}
		out[i] = item{x.Query, x.IsIn, jid}
	}
	writeJSON(w, 200, out)
}

func (a *api) audit(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	entries, err := a.b.AuditLog(intParam(r, "limit", 100, maxPageSize), before)
	respond(w, entries, err)
}

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
	msg  Message
	err  error
	fp   string
	at   time.Time
}

// do runs fn once per (scope, key). A repeat with the same request gets the first result; a repeat
// with a different request is rejected, since silently returning the old message would hide a bug.
func (s *idempotency) do(ctx context.Context, scope, key, fp string, fn func() (Message, error)) (msg Message, err error) {
	if key == "" {
		return fn()
	}
	if len(key) > 200 {
		return Message{}, badInput("idempotency key is longer than 200 characters")
	}
	key = scope + ":" + key
	s.mu.Lock()
	if e, ok := s.entries[key]; ok && time.Since(e.at) < idemTTL {
		s.mu.Unlock()
		if e.fp != fp {
			return Message{}, errConflict
		}
		select {
		case <-e.done:
			return e.msg, e.err
		case <-ctx.Done():
			return Message{}, ctx.Err()
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

func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return localOnly(next)
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if q := r.URL.Query().Get("token"); q != "" {
			got = []byte("Bearer " + q)
		}
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localOnly protects a token-less API from web pages the owner visits: it rejects cross-site
// requests (CSRF) and any Host that isn't loopback (DNS rebinding).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		crossSite := r.Header.Get("Sec-Fetch-Site") == "cross-site"
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			crossSite = crossSite || err != nil || u.Host != r.Host
		}
		if !loopback || crossSite {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "without -token only same-origin requests to localhost are allowed"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withRequestLog logs one line per request. The query string is left out because it may hold the token.
func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.URL.Path == "/status" || r.URL.Path == "/messages/wait" || r.URL.Path == "/events":
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, badInput("invalid JSON body: %v", err))
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps errors to status codes: bad input 400, missing 404, key reuse 409, rate limit 429, anything else 502.
func writeError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var in inputError
	var rl RateLimitError
	switch {
	case errors.As(err, &in):
		code = http.StatusBadRequest
	case errors.Is(err, errNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errConflict):
		code = http.StatusConflict
	case errors.As(err, &rl):
		code = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())+1))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
