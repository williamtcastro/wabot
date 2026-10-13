package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
}

// enqueue takes {"batch", "text", "send_at", "messages": [{to, text, reply_to, send_at}], "idempotency_key"};
// the top-level text and send_at are defaults for messages that leave them out.
func (a *server) enqueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batch          string         `json:"batch"`
		Text           string         `json:"text"`
		SendAt         time.Time      `json:"send_at"`
		Messages       []queueMessage `json:"messages"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if !readJSONMax(w, r, &req, maxQueueBody) {
		return
	}
	p := principal(r)
	reqs := make([]bot.QueueRequest, len(req.Messages))
	for i, m := range req.Messages {
		to, ok := a.allowedChat(r, m.To)
		if !ok {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("messages[%d]: %s", i, errChatForbidden))
			return
		}
		reqs[i] = bot.QueueRequest{To: to, Text: cmp.Or(m.Text, req.Text), ReplyTo: m.ReplyTo, SendAt: cmp.Or(m.SendAt, req.SendAt)}
	}
	key := cmp.Or(r.Header.Get("Idempotency-Key"), req.IdempotencyKey)
	items, err := a.b.Enqueue(p.Name, p.TokenID, req.Batch, key, reqs)
	if err != nil {
		writeError(w, err)
		return
	}
	for i := range items {
		items[i].Text = ""
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"batch": items[0].Batch, "queued": len(items), "items": items})
}

// queueFilter limits callers without the admin scope to their own items.
func queueFilter(r *http.Request) store.OutboxFilter {
	q := r.URL.Query()
	f := store.OutboxFilter{Batch: q.Get("batch"), Status: q.Get("status"), Actor: q.Get("actor")}
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	if p := principal(r); !p.Has(auth.Admin) {
		f.Actor = p.Name
	}
	return f
}

func (a *server) listQueue(w http.ResponseWriter, r *http.Request) {
	f := queueFilter(r)
	if f.Status != "" && !slices.Contains(store.OutboxStates, f.Status) {
		writeError(w, bot.BadInput("status must be one of %s", strings.Join(store.OutboxStates, ", ")))
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

func (a *server) queueItem(w http.ResponseWriter, r *http.Request) (store.OutboxItem, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := a.b.Store.OutboxItem(id)
	if err == nil && !principal(r).Has(auth.Admin) && it.Actor != principal(r).Name {
		err = store.ErrNotFound
	}
	if err != nil {
		writeError(w, err)
		return it, false
	}
	return it, true
}

func (a *server) getQueued(w http.ResponseWriter, r *http.Request) {
	if it, ok := a.queueItem(w, r); ok {
		writeJSON(w, 200, it)
	}
}

func (a *server) cancelQueued(w http.ResponseWriter, r *http.Request) {
	it, ok := a.queueItem(w, r)
	if !ok {
		return
	}
	_, err := a.b.CancelQueued(principal(r).Name, it.ID, store.OutboxFilter{Actor: queueFilter(r).Actor})
	if errors.Is(err, store.ErrNotFound) {
		if it, err = a.b.Store.OutboxItem(it.ID); err == nil {
			writeErr(w, http.StatusConflict, "only queued messages can be canceled; this one is "+it.Status)
			return
		}
	}
	respond(w, map[string]int{"canceled": 1}, err)
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
