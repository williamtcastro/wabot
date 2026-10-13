package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const (
	liveWindow  = time.Hour
	liveMaxKept = 2000
)

type (
	liveMsg struct {
		gen int
		on  bool
		err error
	}
	eventMsg struct {
		gen int
		ev  Event
	}
)

// liveMessage is what the panel keeps of a streamed message: never its text.
type liveMessage struct {
	at     time.Time
	chat   string
	name   string
	fromMe bool
}

// live follows GET /events in the background, so status changes and new messages show up at once
// instead of on the next poll.
type live struct {
	on     bool
	err    error
	since  time.Time
	ch     chan tea.Msg
	cancel context.CancelFunc
	recent []liveMessage
}

func (l *live) start(c *Client, gen int) tea.Cmd {
	l.stop()
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan tea.Msg)
	l.ch, l.cancel = ch, cancel
	send := func(msg tea.Msg) {
		select {
		case ch <- msg:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(ch)
		backoff := 2 * time.Second
		for ctx.Err() == nil {
			err := c.Events(ctx, "status,message,queue", func() {
				backoff = 2 * time.Second
				send(liveMsg{gen: gen, on: true})
			}, func(ev Event) { send(eventMsg{gen, ev}) })
			if ctx.Err() != nil {
				return
			}
			send(liveMsg{gen: gen, err: err})
			if apiErr, ok := errors.AsType[*APIError](err); ok && (apiErr.Code == http.StatusUnauthorized || apiErr.Code == http.StatusForbidden) {
				return
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
	return l.wait()
}

func (l *live) wait() tea.Cmd {
	ch := l.ch
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (l *live) stop() {
	if l.cancel != nil {
		l.cancel()
	}
	l.cancel, l.ch, l.on = nil, nil, false
}

func (l *live) add(msg store.Message) {
	cutoff := time.Now().Add(-liveWindow)
	i := 0
	for i < len(l.recent) && l.recent[i].at.Before(cutoff) {
		i++
	}
	l.recent = append(l.recent[i:], liveMessage{at: time.Now(), chat: msg.Chat, name: msg.PushName, fromMe: msg.FromMe})
	if len(l.recent) > liveMaxKept {
		l.recent = l.recent[len(l.recent)-liveMaxKept:]
	}
}

type chatCount struct {
	chat, name string
	n          int
}

// counts returns incoming and outgoing messages in the last hour and the busiest chats.
func (l *live) counts() (in, out int, top []chatCount) {
	cutoff := time.Now().Add(-liveWindow)
	by := map[string]*chatCount{}
	for _, r := range l.recent {
		if r.at.Before(cutoff) {
			continue
		}
		if r.fromMe {
			out++
		} else {
			in++
		}
		c, ok := by[r.chat]
		if !ok {
			c = &chatCount{chat: r.chat}
			by[r.chat] = c
		}
		c.n++
		if !r.fromMe && r.name != "" {
			c.name = r.name
		}
	}
	for _, c := range by {
		top = append(top, *c)
	}
	slices.SortFunc(top, func(a, b chatCount) int {
		if a.n != b.n {
			return b.n - a.n
		}
		return strings.Compare(a.chat, b.chat)
	})
	return in, out, top
}

func (m *model) handleLive(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case liveMsg:
		if msg.gen != m.gen {
			return nil
		}
		m.live.on, m.live.err = msg.on, msg.err
		if msg.on {
			m.live.since = time.Now()
		}
	case eventMsg:
		if msg.gen != m.gen {
			return nil
		}
		switch msg.ev.Type {
		case "status":
			if s, ok := decode[bot.Status](msg.ev.Data); ok {
				m.applyStatus(s, nil)
			}
		case "message":
			if mm, ok := decode[store.Message](msg.ev.Data); ok {
				m.live.add(mm)
				return tea.Batch(m.live.wait(), m.onMessage(mm))
			}
		case "queue":
			return tea.Batch(m.live.wait(), m.queueChanged())
		}
	}
	return m.live.wait()
}
