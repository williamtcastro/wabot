package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const sendAtHelp = `send_at must be an RFC 3339 time with its offset (like 2026-10-13T09:00:00-03:00) or a delay like "90m", "2h" or "3d"`

func (s *server) addQueueTools(srv *mcp.Server) {
	open, closed := true, false
	mcp.AddTool(srv, &mcp.Tool{Name: "schedule_message", Description: "Queue a text message to send later (or as soon as the limits allow). Only when the user asked for it; confirm the time in the user's time zone first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &open, OpenWorldHint: &open}}, s.scheduleMessage)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_scheduled", Description: "Queued messages, newest first, with counts by status: this connection's own (admins see all, but only their own text).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}}, s.listScheduled)
	mcp.AddTool(srv, &mcp.Tool{Name: "cancel_scheduled", Description: "Cancel queued messages that haven't gone out yet: one by id, or a whole batch.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &open, OpenWorldHint: &closed}}, s.cancelScheduled)
}

// queueTarget checks a restricted caller's recipient; the queue worker resolves the others at send time.
func (s *server) queueTarget(ctx context.Context, p auth.Principal, to string) (string, error) {
	switch {
	case strings.TrimSpace(to) == "":
		return "", errors.New("to is required")
	case p.Restricted():
		return s.allowedChat(ctx, p, to)
	}
	return to, nil
}

// parseSendAt reads an absolute RFC 3339 time or a delay from now ("90m", "2h", "3d").
func parseSendAt(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		if t.Before(now.Add(-time.Minute)) {
			return time.Time{}, errors.New("send_at is in the past")
		}
		return t, nil
	}
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(v, "d"); ok {
		var n int
		if n, err = strconv.Atoi(days); err == nil && n > 366 {
			err = errors.New("too far ahead")
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(v)
	}
	if err != nil || d <= 0 {
		return time.Time{}, errors.New(sendAtHelp)
	}
	return now.Add(d), nil
}

// ownFilter limits callers without the admin scope to the items they queued.
func ownFilter(p auth.Principal, f store.OutboxFilter) store.OutboxFilter {
	if !p.Has(auth.Admin) {
		f.Actor = p.Name
	}
	return f
}

type scheduleIn struct {
	To     string `json:"to" jsonschema:"chat JID or phone number with country code"`
	Text   string `json:"text" jsonschema:"message text (WhatsApp formatting like *bold* works)"`
	SendAt string `json:"send_at,omitempty" jsonschema:"when to send: RFC 3339 with offset (2026-10-13T09:00:00-03:00) or a delay like 90m, 2h, 3d; omit to send as soon as the limits allow"`
	Batch  string `json:"batch,omitempty" jsonschema:"group name for list_scheduled and cancel_scheduled (letters, digits, . _ -); generated when omitted"`
}

type scheduledOut struct {
	ID     int64     `json:"id"`
	Batch  string    `json:"batch"`
	To     string    `json:"to"`
	Status string    `json:"status"`
	Due    time.Time `json:"due"`
	Note   string    `json:"note,omitempty"`
}

func (s *server) scheduleMessage(ctx context.Context, req *mcp.CallToolRequest, in scheduleIn) (*mcp.CallToolResult, scheduledOut, error) {
	p, err := s.require(req, auth.Send)
	if err != nil {
		return nil, scheduledOut{}, err
	}
	to, err := s.queueTarget(ctx, p, in.To)
	if err != nil {
		return nil, scheduledOut{}, err
	}
	at, err := parseSendAt(in.SendAt, time.Now())
	if err != nil {
		return nil, scheduledOut{}, err
	}
	items, err := s.b.Enqueue(p.Name, p.TokenID, in.Batch, "", []bot.QueueRequest{{To: to, Text: in.Text, SendAt: at}})
	if err != nil {
		return nil, scheduledOut{}, err
	}
	it := items[0]
	out := scheduledOut{ID: it.ID, Batch: it.Batch, To: cleanID(it.To), Status: it.Status, Due: it.Due}
	switch st := s.b.QueueStats(); {
	case st.Paused:
		out.Note = "the queue is paused; nothing goes out until an admin resumes it"
	case !st.Running:
		out.Note = "no queue worker runs in this process; it goes out once wame serve or wame tui runs on this database"
	}
	return nil, out, nil
}

type listScheduledIn struct {
	Status string `json:"status,omitempty" jsonschema:"queued, sending, sent, failed or canceled"`
	Batch  string `json:"batch,omitempty" jsonschema:"only this batch"`
	Before int64  `json:"before,omitempty" jsonschema:"only ids below this one, to page"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max items (default 50, max 200)"`
}

type queueItemOut struct {
	ID        int64     `json:"id"`
	Batch     string    `json:"batch"`
	Actor     string    `json:"actor"`
	To        string    `json:"to"`
	Chat      string    `json:"chat,omitempty"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	Due       time.Time `json:"due"`
	Created   time.Time `json:"created_at"`
	Updated   time.Time `json:"updated_at"`
	MessageID string    `json:"message_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	Untrusted struct {
		Text string `json:"text,omitempty"`
	} `json:"untrusted"`
}

type listScheduledOut struct {
	Items  []queueItemOut     `json:"items"`
	Counts store.OutboxCounts `json:"counts"`
	Note   string             `json:"note"`
}

func (s *server) listScheduled(ctx context.Context, req *mcp.CallToolRequest, in listScheduledIn) (*mcp.CallToolResult, listScheduledOut, error) {
	p, err := s.require(req, auth.Send)
	if err != nil {
		return nil, listScheduledOut{}, err
	}
	if in.Status != "" && !slices.Contains(store.OutboxStates, in.Status) {
		return nil, listScheduledOut{}, fmt.Errorf("status must be one of %s", strings.Join(store.OutboxStates, ", "))
	}
	f := ownFilter(p, store.OutboxFilter{Status: in.Status, Batch: in.Batch, Before: in.Before})
	items, err := s.b.Store.ListOutbox(f, clamp(in.Limit, 50, 200))
	if err != nil {
		return nil, listScheduledOut{}, err
	}
	counts, err := s.b.Store.CountOutbox(f)
	if err != nil {
		return nil, listScheduledOut{}, err
	}
	out := listScheduledOut{Items: []queueItemOut{}, Counts: counts, Note: untrustedNote}
	for _, it := range items {
		o := queueItemOut{ID: it.ID, Batch: it.Batch, Actor: it.Actor, To: cleanID(it.To), Chat: it.Chat, Status: it.Status,
			Attempts: it.Attempts, Due: it.Due, Created: it.Created, Updated: it.Updated, MessageID: it.MessageID, Error: clean(it.Error)}
		if it.Actor == p.Name {
			o.Untrusted.Text = clean(it.Text)
		}
		out.Items = append(out.Items, o)
	}
	return nil, out, nil
}

type cancelScheduledIn struct {
	ID    int64  `json:"id,omitempty" jsonschema:"one queued item's id"`
	Batch string `json:"batch,omitempty" jsonschema:"cancel every queued item of this batch instead"`
}

type cancelScheduledOut struct {
	Canceled []int64 `json:"canceled"`
}

func (s *server) cancelScheduled(ctx context.Context, req *mcp.CallToolRequest, in cancelScheduledIn) (*mcp.CallToolResult, cancelScheduledOut, error) {
	p, err := s.require(req, auth.Send)
	if err != nil {
		return nil, cancelScheduledOut{}, err
	}
	if (in.ID > 0) == (in.Batch != "") {
		return nil, cancelScheduledOut{}, errors.New("give either id or batch")
	}
	f := ownFilter(p, store.OutboxFilter{Batch: in.Batch})
	if in.ID > 0 {
		it, err := s.b.Store.OutboxItem(in.ID)
		if err != nil || (f.Actor != "" && it.Actor != f.Actor) {
			return nil, cancelScheduledOut{}, fmt.Errorf("no queued message #%d from this connection", in.ID)
		}
	}
	items, err := s.b.CancelQueued(p.Name, in.ID, f)
	if errors.Is(err, store.ErrNotFound) {
		if it, err := s.b.Store.OutboxItem(in.ID); err == nil {
			return nil, cancelScheduledOut{}, fmt.Errorf("only queued messages can be canceled; #%d is %s", in.ID, it.Status)
		}
	}
	if err != nil {
		return nil, cancelScheduledOut{}, err
	}
	out := cancelScheduledOut{Canceled: []int64{}}
	for _, it := range items {
		out.Canceled = append(out.Canceled, it.ID)
	}
	return nil, out, nil
}
