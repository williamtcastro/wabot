package api

import (
	"cmp"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const maxQueueBody = 16 << 20

type queueMessage struct {
	To      string    `json:"to"`
	Text    string    `json:"text"`
	ReplyTo string    `json:"reply_to"`
	SendAt  time.Time `json:"send_at"`
	Name    string    `json:"name"`
	File    string    `json:"file"`
}

// queueBody is POST /queue as JSON; a CSV upload is read into the same shape.
type queueBody struct {
	Batch          string         `json:"batch"`
	Text           string         `json:"text"`
	SendAt         time.Time      `json:"send_at"`
	Messages       []queueMessage `json:"messages"`
	IdempotencyKey string         `json:"idempotency_key"`
	Template       bool           `json:"template"`
	Dedupe         bool           `json:"dedupe"`
	SkipSent       string         `json:"skip_sent"`
	File           string         `json:"file"`
	isCSV          bool
	csv            []bot.QueueRequest
}

// enqueue takes {"batch", "text", "send_at", "messages": [{to, text, reply_to, send_at, name}],
// "idempotency_key", "template", "dedupe", "skip_sent"}, or a text/csv (or application/csv) body
// with the other fields in the query (see readQueueCSV); the top-level text and send_at are
// defaults for messages that leave them out.
func (a *server) enqueue(w http.ResponseWriter, r *http.Request) {
	var req queueBody
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "text/csv" || ct == "application/csv" {
		if !readQueueCSV(w, r, &req) {
			return
		}
	} else if !readJSONMax(w, r, &req, maxQueueBody) {
		return
	}
	reqs, ok := a.queueRequests(w, r, req)
	if !ok {
		return
	}
	p := principal(r)
	res, err := a.b.Queue(r.Context(), bot.QueueOptions{
		Actor: p.Name, TokenID: p.TokenID, Batch: req.Batch, Key: cmp.Or(r.Header.Get("Idempotency-Key"), req.IdempotencyKey),
		Template: req.Template, Dedupe: req.Dedupe, SkipSent: req.SkipSent,
		Admin: p.Has(auth.Admin), Read: p.Has(auth.Read), Allow: p.Chats,
	}, reqs)
	if err != nil {
		writeError(w, err)
		return
	}
	for i := range res.Items {
		res.Items[i].Text = ""
	}
	if res.SkippedRows == nil {
		res.SkippedRows = []bot.SkippedRow{}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"batch": res.Batch, "queued": len(res.Items), "replayed": res.Replayed, "items": res.Items,
		"skipped": res.Skipped, "skipped_rows": res.SkippedRows})
}

// readQueueCSV reads a CSV body (bot.ReadQueueCSV) of at most maxQueueBody bytes and
// bot.MaxQueueBatch rows, with ?batch, ?text and ?send_at as the body's top-level fields.
func readQueueCSV(w http.ResponseWriter, r *http.Request, req *queueBody) bool {
	q := r.URL.Query()
	req.Batch, req.Text, req.isCSV = q.Get("batch"), q.Get("text"), true
	var ok bool
	if req.Template, ok = boolParam(w, r, "template"); !ok {
		return false
	}
	if req.Dedupe, ok = boolParam(w, r, "dedupe"); !ok {
		return false
	}
	req.SkipSent = q.Get("skip_sent")
	req.File = q.Get("file")
	if s := q.Get("send_at"); s != "" {
		t, err := bot.ParseSendAt(s)
		if err != nil {
			writeError(w, bot.BadInput("?send_at %v", err))
			return false
		}
		req.SendAt = t
	}
	rows, err := bot.ReadQueueCSV(http.MaxBytesReader(w, r.Body, maxQueueBody), bot.MaxQueueBatch)
	if tooLarge(w, err) {
		return false
	}
	if err != nil {
		writeError(w, bot.BadInput("%v", err))
		return false
	}
	req.csv = rows
	return true
}

// queueRequests applies the body's defaults and checks every recipient against the caller's
// allowlist; Enqueue checks the rest.
func (a *server) queueRequests(w http.ResponseWriter, r *http.Request, req queueBody) ([]bot.QueueRequest, bool) {
	reqs := req.csv
	if !req.isCSV {
		reqs = make([]bot.QueueRequest, len(req.Messages))
		for i, m := range req.Messages {
			reqs[i] = bot.QueueRequest{To: m.To, Text: m.Text, ReplyTo: m.ReplyTo, SendAt: m.SendAt, Name: m.Name, File: m.File}
		}
	}
	for i := range reqs {
		to, ok := reqs[i].To, true
		// A tag's members are checked against the allowlist when Bot expands it.
		if _, isTag := bot.QueueTag(to); !isTag {
			to, ok = a.allowedChat(r, reqs[i].To)
		}
		if !ok {
			writeErr(w, http.StatusForbidden, reqs[i].Where(i)+": "+errChatForbidden)
			return nil, false
		}
		reqs[i].To, reqs[i].Text, reqs[i].SendAt = to, cmp.Or(reqs[i].Text, req.Text), cmp.Or(reqs[i].SendAt, req.SendAt)
		reqs[i].File = cmp.Or(reqs[i].File, req.File)
	}
	return reqs, true
}

// own limits f to the caller's own items unless it has the admin scope.
func own(r *http.Request, f store.OutboxFilter) store.OutboxFilter {
	p := principal(r)
	return f.For(p.Name, p.Has(auth.Admin))
}

func queueFilter(r *http.Request) store.OutboxFilter {
	q := r.URL.Query()
	f := store.OutboxFilter{Batch: q.Get("batch"), Status: q.Get("status"), Actor: q.Get("actor")}
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	return own(r, f)
}

func (a *server) listQueue(w http.ResponseWriter, r *http.Request) {
	f := queueFilter(r)
	if err := store.ValidOutboxStatus(f.Status); err != nil {
		writeError(w, bot.BadInput("%v", err))
		return
	}
	items, err := a.b.Store.ListOutbox(f, intParam(r, "limit", 100, 500))
	if err != nil {
		writeError(w, err)
		return
	}
	counts, err := a.b.Store.CountOutbox(f)
	respond(w, map[string]any{"items": items, "counts": counts}, err)
}

// queueID is the {id} of a /queue/{id} route; anything but a positive number is a 404.
func queueID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, store.ErrNotFound)
		return 0, false
	}
	return id, true
}

func (a *server) getQueued(w http.ResponseWriter, r *http.Request) {
	id, ok := queueID(w, r)
	if !ok {
		return
	}
	it, err := a.b.Store.OutboxItem(id)
	if f := own(r, store.OutboxFilter{}); err == nil && f.Actor != "" && it.Actor != f.Actor {
		err = store.ErrNotFound
	}
	respond(w, it, err)
}

func (a *server) cancelQueued(w http.ResponseWriter, r *http.Request) {
	id, ok := queueID(w, r)
	if !ok {
		return
	}
	_, err := a.b.CancelQueued(principal(r).Name, id, own(r, store.OutboxFilter{}))
	respond(w, map[string]int{"canceled": 1}, err)
}

func (a *server) listOptOuts(w http.ResponseWriter, r *http.Request) {
	list, err := a.b.OptOuts()
	respond(w, list, err)
}

// addOptOut takes {"chat": number or JID}; 201 when added, 200 when it was already listed.
func (a *server) addOptOut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	o, added, err := a.b.AddOptOut(r.Context(), principal(r).Name, req.Chat)
	if err != nil {
		writeError(w, err)
		return
	}
	code := http.StatusOK
	if added {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"chat": o.Chat, "added": added})
}

func (a *server) removeOptOut(w http.ResponseWriter, r *http.Request) {
	err := a.b.RemoveOptOut(r.Context(), principal(r).Name, r.PathValue("chat"))
	respond(w, map[string]bool{"removed": true}, err)
}

// cancelBatch cancels every queued item of ?batch= (the caller's own unless admin).
func (a *server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	f := queueFilter(r)
	if f.Batch == "" {
		writeError(w, bot.BadInput("?batch= is required"))
		return
	}
	items, err := a.b.CancelQueued(principal(r).Name, 0, f)
	respond(w, map[string]int{"canceled": len(items)}, err)
}
