// Package bot wraps the whatsmeow client: connection, events, sending, actions, rate limits and webhooks.
package bot

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"

	"github.com/williamtcastro/wabot/internal/store"
)

type Status struct {
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	JID       string `json:"jid,omitempty"`
	PushName  string `json:"push_name,omitempty"`
	QR        string `json:"qr,omitempty"`
	State     string `json:"state"`
}

// InputError marks errors caused by the caller's input (HTTP 400) rather than WhatsApp (502).
type InputError struct{ error }

func BadInput(format string, args ...any) error { return InputError{fmt.Errorf(format, args...)} }

type Bot struct {
	Client    *whatsmeow.Client
	Store     *store.Store
	Log       *slog.Logger
	AutoReply bool
	Typing    bool
	Online    bool
	Limiter   *Limiter
	Lookups   *Limiter
	Webhook   *Webhook

	autoLimit  *Limiter
	mu         sync.RWMutex
	qr         string
	state      string
	subs       map[chan any]struct{}
	jids       map[string]types.JID
	groups     map[string]groupInfo
	typingIn   map[string]*time.Timer
	groupsAt   time.Time
	groupsBusy atomic.Bool
}

func New(ctx context.Context, dbPath string, waLogger waLog.Logger, log *slog.Logger) (*Bot, error) {
	abs, _ := filepath.Abs(dbPath)
	if err := restrictPerms(abs); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+abs+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLogger.Sub("DB"))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, db)
	if err != nil {
		return nil, err
	}
	b := &Bot{
		Client:  whatsmeow.NewClient(device, waLogger.Sub("Client")),
		Store:   st,
		Log:     log,
		Limiter: NewLimiter(0, 0, 0),
		Lookups: NewLimiter(0, 0, 0),
		// Auto-replies answer strangers, so they get their own small budget and are dropped, not queued.
		autoLimit: NewLimiter(6, 10*time.Second, time.Millisecond),
		state:     "starting",
		subs:      map[chan any]struct{}{},
		jids:      map[string]types.JID{},
	}
	b.loadCachedGroups()
	b.Client.AddEventHandler(b.handle)
	b.Client.SetMediaHTTPClient(&http.Client{Transport: limitedTransport{http.DefaultTransport}})
	return b, nil
}

// Start connects, running the QR login flow when no session exists yet.
func (b *Bot) Start(ctx context.Context) error {
	if b.Client.Store.ID != nil {
		b.setState("connecting", "")
		return b.Client.Connect()
	}
	qrChan, err := b.Client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := b.Client.Connect(); err != nil {
		return err
	}
	go func() {
		for item := range qrChan {
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				b.setState("waiting_for_qr_scan", item.Code)
			case "success":
				b.setState("paired", "")
			case "timeout":
				b.setState("qr_timeout", "")
				b.Client.Disconnect()
				time.Sleep(time.Second)
				_ = b.Start(ctx)
			default:
				b.setState("login_error: "+item.Event, "")
				b.Log.Error("login failed", "event", item.Event, "err", item.Error)
			}
		}
	}()
	return nil
}

func (b *Bot) PairPhone(ctx context.Context, actor, phone string) (code string, err error) {
	defer func() { b.Audit(actor, "pair", "", "", err) }()
	if b.Client.Store.ID != nil {
		return "", BadInput("already logged in")
	}
	return b.Client.PairPhone(ctx, digits(phone), true, whatsmeow.PairClientChrome, "Chrome (Linux)")
}

func (b *Bot) Status() Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := Status{
		Connected: b.Client.IsConnected(),
		LoggedIn:  b.Client.IsLoggedIn(),
		QR:        b.qr,
		State:     b.state,
	}
	if b.Client.Store.ID != nil {
		s.JID = b.selfJID()
		s.PushName = b.Client.Store.PushName
	}
	return s
}

func (b *Bot) setState(state, qr string) {
	b.mu.Lock()
	changed := b.state != state
	b.state, b.qr = state, qr
	b.mu.Unlock()
	if changed {
		b.Log.Info("connection state", "state", state)
	}
	b.publish(b.Status())
}

// Subscribe returns a channel receiving every store.Message, store.MessageUpdate, Status and
// store.ChatsChanged. Slow subscribers miss events rather than block the bot.
func (b *Bot) Subscribe() (chan any, func()) {
	ch := make(chan any, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *Bot) publish(v any) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

func (b *Bot) selfJID() string {
	if id := b.Client.Store.ID; id != nil {
		return id.ToNonAD().String()
	}
	return ""
}

func (b *Bot) isSelfChat(chat string) bool {
	own := b.Client.Store.ID
	return own != nil && chat == own.ToNonAD().String()
}

// SleepUntil waits until t or until ctx is done.
func SleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func restrictPerms(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
