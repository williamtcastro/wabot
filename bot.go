package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Message struct {
	ID        string    `json:"id"`
	Chat      string    `json:"chat"`
	Sender    string    `json:"sender"`
	PushName  string    `json:"push_name"`
	FromMe    bool      `json:"from_me"`
	IsGroup   bool      `json:"is_group"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

type Status struct {
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	JID       string `json:"jid,omitempty"`
	PushName  string `json:"push_name,omitempty"`
	QR        string `json:"qr,omitempty"`
	State     string `json:"state"`
}

type Bot struct {
	Client     *whatsmeow.Client
	AutoReply  bool
	WebhookURL string

	db     *sql.DB
	mu     sync.RWMutex
	qr     string
	state  string
	subs   map[chan any]struct{}
	jids   map[string]types.JID
	groups map[string]string
}

func NewBot(ctx context.Context, dbPath string, log waLog.Logger) (*Bot, error) {
	abs, _ := filepath.Abs(dbPath)
	db, err := sql.Open("sqlite", "file:"+abs+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", log.Sub("DB"))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("create message tables: %w", err)
	}
	b := &Bot{
		Client: whatsmeow.NewClient(device, log.Sub("Client")),
		db:     db,
		state:  "starting",
		subs:   map[chan any]struct{}{},
		jids:   map[string]types.JID{},
	}
	b.Client.AddEventHandler(b.handle)
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
			}
		}
	}()
	return nil
}

func (b *Bot) PairPhone(ctx context.Context, phone string) (string, error) {
	if b.Client.Store.ID != nil {
		return "", errors.New("already logged in")
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
		s.JID = b.Client.Store.ID.ToNonAD().String()
		s.PushName = b.Client.Store.PushName
	}
	return s
}

func (b *Bot) setState(state, qr string) {
	b.mu.Lock()
	b.state, b.qr = state, qr
	b.mu.Unlock()
	b.publish(b.Status())
}

func (b *Bot) handle(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		b.setState("connected", "")
		_ = b.Client.SendPresence(context.Background(), types.PresenceAvailable)
	case *events.Disconnected:
		b.setState("disconnected", "")
	case *events.LoggedOut:
		b.setState("logged_out", "")
	case *events.PairSuccess:
		b.setState("paired", "")
	case *events.HistorySync:
		b.importHistory(v)
	case *events.Message:
		m := b.toMessage(v)
		if m.Type != "other" {
			b.store(m)
		}
		if b.WebhookURL != "" {
			go postJSON(b.WebhookURL, m)
		}
		if b.AutoReply && !m.FromMe {
			b.autoReply(v.Info.Chat, m.Text)
		}
	}
}

func (b *Bot) autoReply(chat types.JID, text string) {
	var reply string
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "!ping":
		reply = "pong 🏓"
	case "!time":
		reply = time.Now().Format(time.RFC1123)
	case "!help":
		reply = "Commands: !ping, !time, !help"
	default:
		return
	}
	_, _ = b.SendText(context.Background(), chat.String(), reply)
}

func (b *Bot) toMessage(v *events.Message) Message {
	msg := v.Message
	text, kind := msg.GetConversation(), "text"
	switch {
	case msg.GetExtendedTextMessage() != nil:
		text = msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		text, kind = msg.GetImageMessage().GetCaption(), "image"
	case msg.GetVideoMessage() != nil:
		text, kind = msg.GetVideoMessage().GetCaption(), "video"
	case msg.GetDocumentMessage() != nil:
		text, kind = msg.GetDocumentMessage().GetFileName(), "document"
	case msg.GetAudioMessage() != nil:
		kind = "audio"
	case msg.GetStickerMessage() != nil:
		kind = "sticker"
	case msg.GetReactionMessage() != nil:
		text, kind = msg.GetReactionMessage().GetText(), "reaction"
	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		text, kind = fmt.Sprintf("%f,%f", l.GetDegreesLatitude(), l.GetDegreesLongitude()), "location"
	case text == "":
		kind = "other"
	}
	sender := v.Info.Sender
	if sender.Server == types.HiddenUserServer && !v.Info.SenderAlt.IsEmpty() {
		sender = v.Info.SenderAlt
	}
	chat := v.Info.Chat
	if chat.Server == types.HiddenUserServer {
		switch {
		case v.Info.IsFromMe && !v.Info.RecipientAlt.IsEmpty():
			chat = v.Info.RecipientAlt
		case !v.Info.IsFromMe && !v.Info.SenderAlt.IsEmpty():
			chat = v.Info.SenderAlt
		}
	}
	return Message{
		ID:        v.Info.ID,
		Chat:      b.phoneJID(chat.ToNonAD()).String(),
		Sender:    b.phoneJID(sender.ToNonAD()).String(),
		PushName:  v.Info.PushName,
		FromMe:    v.Info.IsFromMe,
		IsGroup:   v.Info.IsGroup,
		Type:      kind,
		Text:      text,
		Timestamp: v.Info.Timestamp,
	}
}

func (b *Bot) store(m Message) {
	if err := saveMessage(b.db, m); err != nil {
		b.Client.Log.Warnf("save message: %v", err)
	}
	b.publish(m)
}

// phoneJID maps hidden @lid user IDs back to phone-number JIDs when WhatsApp has told us the link,
// so a person's chat stays in one place no matter which ID a message used.
func (b *Bot) phoneJID(jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer || b.Client.Store.LIDs == nil {
		return jid
	}
	pn, err := b.Client.Store.LIDs.GetPNForLID(context.Background(), jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn
}

// Subscribe returns a channel receiving every Message and Status change.
func (b *Bot) Subscribe() (chan any, func()) {
	ch := make(chan any, 64)
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

func (b *Bot) SendText(ctx context.Context, to, text string) (Message, error) {
	return b.send(ctx, to, &waE2E.Message{Conversation: proto.String(text)}, "text", text)
}

// SendFile sends images as photos and everything else as a document.
func (b *Bot) SendFile(ctx context.Context, to string, data []byte, fileName, caption string) (Message, error) {
	mime := http.DetectContentType(data)
	if strings.HasPrefix(mime, "image/") {
		up, err := b.Client.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return Message{}, err
		}
		return b.send(ctx, to, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String(caption),
			Mimetype:      proto.String(mime),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}, "image", caption)
	}
	up, err := b.Client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return Message{}, err
	}
	return b.send(ctx, to, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption:       proto.String(caption),
		FileName:      proto.String(fileName),
		Title:         proto.String(fileName),
		Mimetype:      proto.String(mime),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}, "document", fileName)
}

func (b *Bot) send(ctx context.Context, to string, msg *waE2E.Message, kind, text string) (Message, error) {
	if !b.Client.IsLoggedIn() {
		return Message{}, errors.New("not logged in")
	}
	jid, err := b.Resolve(ctx, to)
	if err != nil {
		return Message{}, err
	}
	resp, err := b.Client.SendMessage(ctx, jid, msg)
	if err != nil {
		return Message{}, err
	}
	m := Message{
		ID:        resp.ID,
		Chat:      jid.String(),
		Sender:    b.Client.Store.ID.ToNonAD().String(),
		FromMe:    true,
		IsGroup:   jid.Server == types.GroupServer,
		Type:      kind,
		Text:      text,
		Timestamp: resp.Timestamp,
	}
	b.store(m)
	return m, nil
}

type Contact struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone"`
	Name     string `json:"name"`
	PushName string `json:"push_name,omitempty"`
}

// Contacts lists address-book contacts synced from the phone, filtered by name or number.
func (b *Bot) Contacts(ctx context.Context, query string) ([]Contact, error) {
	if b.Client.Store.Contacts == nil {
		return []Contact{}, nil
	}
	all, err := b.Client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	out := []Contact{}
	for jid, c := range all {
		if jid.Server != types.DefaultUserServer {
			continue
		}
		name := firstNonEmpty(c.FullName, c.FirstName, c.BusinessName, c.PushName)
		if name == "" {
			continue
		}
		if q != "" {
			byName := strings.Contains(strings.ToLower(name+" "+c.PushName), q)
			byPhone := digits(q) != "" && strings.Contains(jid.User, digits(q))
			if !byName && !byPhone {
				continue
			}
		}
		out = append(out, Contact{JID: jid.String(), Phone: "+" + jid.User, Name: name, PushName: c.PushName})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// Resolve turns a recipient into the JID WhatsApp actually uses. Plain phone numbers are
// looked up on the server, which fixes variants like Brazil's optional 9th digit.
func (b *Bot) Resolve(ctx context.Context, to string) (types.JID, error) {
	jid, err := ParseRecipient(to)
	if err != nil || strings.Contains(to, "@") {
		return jid, err
	}
	b.mu.RLock()
	cached, ok := b.jids[jid.User]
	b.mu.RUnlock()
	if ok {
		return cached, nil
	}
	res, err := b.Client.IsOnWhatsApp(ctx, []string{"+" + jid.User})
	if err != nil {
		return types.JID{}, fmt.Errorf("lookup %s: %w", to, err)
	}
	for _, r := range res {
		if r.IsIn {
			b.mu.Lock()
			b.jids[jid.User] = r.JID
			b.mu.Unlock()
			return r.JID, nil
		}
	}
	return types.JID{}, fmt.Errorf("+%s is not on WhatsApp", jid.User)
}

// ParseRecipient accepts "+55 11 99999-9999", "5511999999999" or a full JID like "1203...@g.us".
func ParseRecipient(to string) (types.JID, error) {
	to = strings.TrimSpace(to)
	if strings.Contains(to, "@") {
		return types.ParseJID(to)
	}
	d := digits(to)
	if len(d) < 8 {
		return types.JID{}, fmt.Errorf("invalid recipient %q", to)
	}
	return types.NewJID(d, types.DefaultUserServer), nil
}

func digits(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
