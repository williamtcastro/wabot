// Package api serves the HTTP API on top of a bot.Bot.
package api

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"

	"github.com/williamtcastro/wame/docs"
	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const (
	maxUpload   = 64 << 20
	maxPollWait = 120 * time.Second
	maxPageSize = 1000
)

const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>wame API</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"
  integrity="sha384-wxLW6kwyHktdDGr6Pv1zgm/VGJh99lfUbzSn6HNHBENZlCN7W602k9VkGdxuFvPn" crossorigin="anonymous">
</head><body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"
  integrity="sha384-wmyclcVGX/WhUkdkATwhaK1X1JtiNrr2EoYJ+diV3vj4v6OC5yCeSu+yW13SYJep" crossorigin="anonymous"></script>
<script>SwaggerUIBundle({url: "openapi.yaml", dom_id: "#ui"});</script>
</body></html>`

type server struct {
	b       *bot.Bot
	version string
	idem    idempotency
	streams streams
}

// NewHandler serves the API. Every route requires a scope; see docs/API.md for the table.
func NewHandler(b *bot.Bot, authn *auth.Authenticator, version string) http.Handler {
	a := &server{b: b, version: version, idem: idempotency{entries: map[string]*idemEntry{}}}
	mux := http.NewServeMux()
	read, send, admin := auth.Read, auth.Send, auth.Admin

	mux.HandleFunc("GET /status", need(read, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, visibleStatus(principal(r), b.Status()))
	}))
	mux.HandleFunc("GET /qr", need(admin, a.qr))
	mux.HandleFunc("POST /pair", need(admin, a.pair))
	mux.HandleFunc("POST /logout", need(admin, a.logout))

	mux.HandleFunc("POST /send", need(send, a.sendText))
	mux.HandleFunc("POST /send/file", need(send, a.sendFile))
	mux.HandleFunc("POST /react", need(send, a.react))
	mux.HandleFunc("POST /edit", need(send, a.edit))
	mux.HandleFunc("POST /delete", need(send, a.delete))
	mux.HandleFunc("POST /read", need(send, a.read))
	mux.HandleFunc("POST /typing", need(send, a.typing))

	mux.HandleFunc("GET /messages", need(read, a.messages))
	mux.HandleFunc("GET /messages/wait", need(read, a.wait))
	mux.HandleFunc("GET /events", need(read, a.events))
	mux.HandleFunc("GET /media/{seq}", need(read, a.media))
	mux.HandleFunc("GET /chats", need(read, a.chats))

	mux.HandleFunc("GET /contacts", need(read, unrestricted(a.contacts)))
	mux.HandleFunc("GET /groups", need(read, unrestricted(a.groups)))
	mux.HandleFunc("POST /check", need(send, unrestricted(a.check)))

	mux.HandleFunc("GET /audit", need(admin, a.audit))
	mux.HandleFunc("GET /stats", need(admin, a.stats))
	mux.HandleFunc("GET /tokens", need(admin, a.listTokens))
	mux.HandleFunc("POST /tokens", need(admin, a.createToken))
	mux.HandleFunc("DELETE /tokens/{name}", need(admin, a.revokeToken))

	root := http.NewServeMux()
	fails := &failureLimiter{}
	root.Handle("/", withAuth(authn, fails, mux))
	root.Handle("/mcp", mcpHandler(b, authn, fails, version, b.Log))
	root.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, docsPage)
	})
	root.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(docs.OpenAPI)
	})
	return withRequestLog(b.Log, root)
}

func (a *server) qr(w http.ResponseWriter, r *http.Request) {
	s := a.b.Status()
	if s.LoggedIn || s.QR == "" {
		writeError(w, fmt.Errorf("%w: no QR available (state: %s)", store.ErrNotFound, s.State))
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

func (a *server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	code, err := a.b.PairPhone(r.Context(), principal(r).Name, req.Phone)
	respond(w, map[string]string{"code": code}, err)
}

func (a *server) logout(w http.ResponseWriter, r *http.Request) {
	err := a.b.Client.Logout(r.Context())
	a.b.Audit(principal(r).Name, "logout", "", "", err)
	respond(w, map[string]bool{"ok": true}, err)
}

func (a *server) sendText(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, bot.BadInput("'to' and 'text' are required"))
		return
	}
	to, ok := a.sendGuard(w, r, req.To)
	if !ok {
		return
	}
	key := cmp.Or(r.Header.Get("Idempotency-Key"), req.IdempotencyKey)
	m, err := a.idem.do(r.Context(), principal(r).Name+":send", key, fingerprint(req.To, req.Text, req.ReplyTo), func() (store.Message, error) {
		return a.b.SendText(r.Context(), principal(r).Name, to, req.Text, bot.SendOptions{ReplyTo: req.ReplyTo})
	})
	respond(w, m, err)
}

func (a *server) sendFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, bot.BadInput("invalid multipart form: %v", err))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, bot.BadInput("multipart field 'file' is required"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		writeError(w, bot.BadInput("read upload: %v", err))
		return
	}
	if len(data) > maxUpload {
		writeError(w, bot.BadInput("file is larger than %d MB", maxUpload>>20))
		return
	}
	to := r.FormValue("to")
	if to == "" {
		writeError(w, bot.BadInput("'to' is required"))
		return
	}
	to, ok := a.sendGuard(w, r, to)
	if !ok {
		return
	}
	key := cmp.Or(r.Header.Get("Idempotency-Key"), r.FormValue("idempotency_key"))
	sum := sha256.Sum256(data)
	m, err := a.idem.do(r.Context(), principal(r).Name+":file", key, fingerprint(to, hdr.Filename, r.FormValue("caption"), r.FormValue("reply_to"), string(sum[:])), func() (store.Message, error) {
		return a.b.SendFile(r.Context(), principal(r).Name, to, data, hdr.Filename, r.FormValue("caption"), bot.SendOptions{ReplyTo: r.FormValue("reply_to")})
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

func (a *server) target(w http.ResponseWriter, r *http.Request) (targetReq, bool) {
	var req targetReq
	if !readJSON(w, r, &req) {
		return req, false
	}
	if req.Chat == "" {
		writeError(w, bot.BadInput("'chat' is required"))
		return req, false
	}
	chat, ok := a.sendGuard(w, r, req.Chat)
	req.Chat = chat
	return req, ok
}

func (a *server) react(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.React(r.Context(), principal(r).Name, req.Chat, req.ID, req.Emoji)
		respond(w, m, err)
	}
}

func (a *server) edit(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.Edit(r.Context(), principal(r).Name, req.Chat, req.ID, req.Text)
		respond(w, m, err)
	}
}

func (a *server) delete(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		m, err := a.b.Delete(r.Context(), principal(r).Name, req.Chat, req.ID)
		respond(w, m, err)
	}
}

func (a *server) read(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		if req.ID != "" {
			req.IDs = append(req.IDs, req.ID)
		}
		n, err := a.b.MarkRead(r.Context(), principal(r).Name, req.Chat, req.IDs)
		respond(w, map[string]int{"marked": n}, err)
	}
}

func (a *server) typing(w http.ResponseWriter, r *http.Request) {
	if req, ok := a.target(w, r); ok {
		respond(w, map[string]bool{"ok": true}, a.b.SetTyping(r.Context(), req.Chat, req.State))
	}
}

func (a *server) filter(w http.ResponseWriter, r *http.Request) (store.Filter, bool) {
	f, err := store.ParseFilter(r.URL.Query(), func(s string) (string, error) {
		if principal(r).Restricted() {
			return a.b.LocalChat(s)
		}
		jid, err := a.b.Resolve(r.Context(), s)
		if err != nil {
			if jid, err = bot.ParseRecipient(s); err != nil {
				return "", err
			}
		}
		return a.b.PhoneJID(jid).String(), nil
	})
	if err != nil {
		writeError(w, bot.BadInput("%v", err))
		return f, false
	}
	return f, a.restrictFilter(w, r, &f)
}

func intParam(r *http.Request, name string, def, maxVal int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, maxVal)
}

func (a *server) messages(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filter(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Has("since")
	writeJSON(w, 200, a.b.Store.Query(f, intParam(r, "limit", 50, maxPageSize), cursor))
}

func (a *server) wait(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filter(w, r)
	if !ok {
		return
	}
	if !r.URL.Query().Has("since") {
		f.Since = a.b.Store.LastSeq()
	}
	limit := intParam(r, "limit", 100, maxPageSize)
	timeout := time.Duration(intParam(r, "timeout", 30, int(maxPollWait/time.Second))) * time.Second
	p := principal(r)
	release, ok := a.streams.acquire(p)
	if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": errTooManyStreams.Error()})
		return
	}
	defer release()

	ch, unsub := a.b.Subscribe()
	defer unsub()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if !a.stillValid(p) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token revoked or expired"})
			return
		}
		if msgs := a.b.Store.Query(f, limit, true); len(msgs) > 0 {
			writeJSON(w, 200, map[string]any{"messages": msgs, "next_since": msgs[len(msgs)-1].Seq})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			writeJSON(w, 200, map[string]any{"messages": []store.Message{}, "next_since": f.Since})
			return
		case <-ch:
		}
	}
}

func (a *server) events(w http.ResponseWriter, r *http.Request) {
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
		f.Since = a.b.Store.LastSeq()
	}
	want := map[string]bool{"message": true, "update": true, "status": true}
	if v := q.Get("events"); v != "" {
		want = map[string]bool{}
		for _, e := range strings.Split(v, ",") {
			want[strings.TrimSpace(e)] = true
		}
	}

	p := principal(r)
	release, ok := a.streams.acquire(p)
	if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": errTooManyStreams.Error()})
		return
	}
	defer release()
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
			msgs := a.b.Store.Query(f, 500, true)
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
	recheck := time.NewTicker(streamRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-recheck.C:
			if !a.stillValid(p) {
				return
			}
			continue
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case v := <-ch:
			if !a.stillValid(p) {
				return
			}
			switch v := v.(type) {
			case store.Message, store.ChatsChanged:
				catchUp()
			case store.MessageUpdate:
				if want["update"] && unfiltered.Match(v.Message) {
					send("update", "", v.Message)
				}
			case bot.Status:
				if want["status"] {
					send("status", "", visibleStatus(p, v))
				}
			}
		}
		_ = rc.Flush()
	}
}

func (a *server) media(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeError(w, bot.BadInput("seq must be a number"))
		return
	}
	if p := principal(r); p.Restricted() {
		if m, err := a.b.Store.GetBySeq(seq); err != nil || !a.b.ChatAllowed(r.Context(), p.Chats, m.Chat) {
			writeError(w, store.ErrNotFound)
			return
		}
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
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": m.MediaFileName()}))
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

func (a *server) chats(w http.ResponseWriter, r *http.Request) {
	chats, err := a.b.Chats(r.Context(), intParam(r, "limit", 50, maxPageSize))
	if p := principal(r); err == nil && p.Restricted() {
		allowed := a.b.ExpandAllowlist(r.Context(), p.Chats)
		chats = slices.DeleteFunc(chats, func(c store.Chat) bool { return !slices.Contains(allowed, c.JID) })
	}
	respond(w, chats, err)
}

func (a *server) contacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := a.b.Contacts(r.Context(), r.URL.Query().Get("q"))
	respond(w, contacts, err)
}

func (a *server) groups(w http.ResponseWriter, r *http.Request) {
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

func (a *server) check(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phones []string `json:"phones"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Phones) == 0 || len(req.Phones) > 50 {
		writeError(w, bot.BadInput("send between 1 and 50 phones"))
		return
	}
	out, err := a.b.Check(r.Context(), req.Phones)
	respond(w, out, err)
}

func (a *server) audit(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	entries, err := a.b.Store.AuditLog(intParam(r, "limit", 100, maxPageSize), before)
	respond(w, entries, err)
}

// Stats is the body of GET /stats: what the admin panel's Health tab shows.
type Stats struct {
	Version   string            `json:"version"`
	Started   time.Time         `json:"started"`
	AutoReply bool              `json:"auto_reply"`
	Limits    StatsLimits       `json:"limits"`
	Webhook   *bot.WebhookStats `json:"webhook"`
}

type StatsLimits struct {
	Send      bot.LimiterStats `json:"send"`
	Lookups   bot.LimiterStats `json:"lookups"`
	AutoReply bot.LimiterStats `json:"auto_reply"`
}

func (a *server) stats(w http.ResponseWriter, r *http.Request) {
	out := Stats{
		Version: a.version, Started: a.b.Started, AutoReply: a.b.AutoReply,
		Limits: StatsLimits{Send: a.b.Limiter.Stats(), Lookups: a.b.Lookups.Stats(), AutoReply: a.b.AutoReplyStats()},
	}
	if a.b.Webhook != nil {
		s := a.b.Webhook.Stats()
		out.Webhook = &s
	}
	writeJSON(w, 200, out)
}
