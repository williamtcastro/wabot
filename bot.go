package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

const maxMediaDownload = 100 << 20

type Status struct {
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	JID       string `json:"jid,omitempty"`
	PushName  string `json:"push_name,omitempty"`
	QR        string `json:"qr,omitempty"`
	State     string `json:"state"`
}

// inputError marks errors caused by the caller's input (HTTP 400) rather than WhatsApp (502).
type inputError struct{ error }

func badInput(format string, args ...any) error { return inputError{fmt.Errorf(format, args...)} }

type Bot struct {
	Client    *whatsmeow.Client
	Log       *slog.Logger
	AutoReply bool
	Typing    bool
	Online    bool
	Limiter   *Limiter
	Lookups   *Limiter
	Webhook   *Webhook

	autoLimit *Limiter
	lidKey    string
	db        *sql.DB
	mu        sync.RWMutex
	qr        string
	state     string
	subs      map[chan any]struct{}
	jids      map[string]types.JID
	groups    map[string]groupInfo
	typingIn  map[string]*time.Timer
	groupsAt  time.Time
}

func NewBot(ctx context.Context, dbPath string, waLogger waLog.Logger, log *slog.Logger) (*Bot, error) {
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
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("create message tables: %w", err)
	}
	if err := migrateAudit(ctx, db); err != nil {
		return nil, fmt.Errorf("create audit table: %w", err)
	}
	b := &Bot{
		Client:  whatsmeow.NewClient(device, waLogger.Sub("Client")),
		Log:     log,
		Limiter: NewLimiter(0, 0, 0),
		Lookups: NewLimiter(0, 0, 0),
		// Auto-replies answer strangers, so they get their own small budget and are dropped, not queued.
		autoLimit: NewLimiter(6, 10*time.Second, time.Millisecond),
		db:        db,
		state:     "starting",
		subs:      map[chan any]struct{}{},
		jids:      map[string]types.JID{},
	}
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
		return "", badInput("already logged in")
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

func (b *Bot) handle(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		b.setState("connected", "")
		b.announcePresence()
	case *events.PushNameSetting, *events.AppStateSyncComplete:
		b.announcePresence()
	case *events.Disconnected:
		b.setState("disconnected", "")
	case *events.LoggedOut:
		b.setState("logged_out", "")
		b.Audit("whatsapp", "logged_out", "", fmt.Sprint(v.Reason), nil)
	case *events.PairSuccess:
		b.setState("paired", "")
	case *events.HistorySync:
		b.importHistory(v)
	case *events.Message:
		if pm := v.Message.GetProtocolMessage(); pm != nil {
			b.handleProtocol(v, pm)
			return
		}
		m := b.toMessage(v)
		if m.Type == "other" {
			return
		}
		m, inserted := b.store(m)
		if !inserted {
			return
		}
		if b.Webhook != nil {
			b.Webhook.Deliver("message", m)
		}
		if b.AutoReply && (!m.FromMe || b.isSelfChat(m.Chat)) {
			go b.autoReply(m)
		}
	}
}

func (b *Bot) handleProtocol(v *events.Message, pm *waE2E.ProtocolMessage) {
	actor := b.toMessage(v)
	target, err := b.getMessage(actor.Chat, pm.GetKey().GetID())
	if err != nil {
		return
	}
	sameAuthor := actor.FromMe == target.FromMe && (actor.FromMe || actor.Sender == target.Sender)
	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		if sameAuthor || (target.IsGroup && (actor.FromMe || b.isGroupAdmin(context.Background(), target.Chat, actor.Sender))) {
			b.applyDelete(target.Chat, target.ID)
		}
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		if sameAuthor && target.Type == "text" {
			_, text, _, _ := classify(pm.GetEditedMessage())
			b.applyEdit(target.Chat, target.ID, text)
		}
	}
}

func (b *Bot) isSelfChat(chat string) bool {
	own := b.Client.Store.ID
	return own != nil && chat == own.ToNonAD().String()
}

func (b *Bot) autoReply(m Message) {
	var reply string
	switch strings.ToLower(strings.TrimSpace(m.Text)) {
	case "!ping":
		reply = "pong 🏓"
	case "!time":
		reply = time.Now().Format(time.RFC1123)
	case "!help":
		reply = "Commands: !ping, !time, !help"
	default:
		return
	}
	if _, err := b.autoLimit.Reserve(m.Chat); err != nil {
		b.Log.Debug("auto-reply dropped", "chat", m.Chat, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := b.SendText(ctx, "autoreply", m.Chat, reply, SendOptions{ReplyTo: m.ID})
	if err != nil {
		b.Log.Warn("auto-reply failed", "chat", m.Chat, "err", err)
	}
}

type mediaMessage interface {
	whatsmeow.DownloadableMessage
	GetMimetype() string
	GetFileLength() uint64
}

// classify walks the WhatsApp message variants once and returns our type, readable text, the quoted or
// reacted-to message ID, and the downloadable part if any.
func classify(msg *waE2E.Message) (kind, text, replyTo string, media mediaMessage) {
	switch {
	case msg.GetExtendedTextMessage() != nil:
		x := msg.GetExtendedTextMessage()
		return "text", x.GetText(), x.GetContextInfo().GetStanzaID(), nil
	case msg.GetImageMessage() != nil:
		x := msg.GetImageMessage()
		return "image", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x
	case msg.GetPtvMessage() != nil:
		x := msg.GetPtvMessage()
		return "video", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x
	case msg.GetVideoMessage() != nil:
		x := msg.GetVideoMessage()
		return "video", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x
	case msg.GetDocumentMessage() != nil:
		x := msg.GetDocumentMessage()
		return "document", cmp.Or(x.GetCaption(), x.GetFileName()), x.GetContextInfo().GetStanzaID(), x
	case msg.GetAudioMessage() != nil:
		x := msg.GetAudioMessage()
		return "audio", "", x.GetContextInfo().GetStanzaID(), x
	case msg.GetStickerMessage() != nil:
		x := msg.GetStickerMessage()
		return "sticker", "", x.GetContextInfo().GetStanzaID(), x
	case msg.GetReactionMessage() != nil:
		x := msg.GetReactionMessage()
		return "reaction", x.GetText(), x.GetKey().GetID(), nil
	case msg.GetLocationMessage() != nil:
		x := msg.GetLocationMessage()
		return "location", fmt.Sprintf("%f,%f", x.GetDegreesLatitude(), x.GetDegreesLongitude()), "", nil
	case msg.GetConversation() != "":
		return "text", msg.GetConversation(), "", nil
	}
	return "other", "", "", nil
}

func (b *Bot) toMessage(v *events.Message) Message {
	kind, text, replyTo, media := classify(v.Message)
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
	m := Message{
		ID:        v.Info.ID,
		Chat:      b.phoneJID(chat.ToNonAD()).String(),
		Sender:    b.phoneJID(sender.ToNonAD()).String(),
		PushName:  v.Info.PushName,
		FromMe:    v.Info.IsFromMe,
		IsGroup:   v.Info.IsGroup,
		Type:      kind,
		Text:      text,
		ReplyTo:   replyTo,
		Timestamp: v.Info.Timestamp,
		rawChat:   v.Info.Chat.ToNonAD().String(),
		rawSender: v.Info.Sender.ToNonAD().String(),
	}
	if media != nil {
		m.Media = &Media{Mimetype: cmp.Or(media.GetMimetype(), "application/octet-stream"), Size: media.GetFileLength()}
		if d, ok := media.(*waE2E.DocumentMessage); ok {
			m.Media.FileName = d.GetFileName()
		}
	}
	m.raw, _ = proto.Marshal(v.Message)
	return m
}

// store saves m and, if it's new, publishes it with its seq filled in.
func (b *Bot) store(m Message) (Message, bool) {
	seq, inserted, err := saveMessage(b.db, m)
	if err != nil {
		b.Log.Error("save message", "chat", m.Chat, "id", m.ID, "err", err)
		return m, false
	}
	if !inserted {
		return m, false
	}
	m.Seq = seq
	if strings.HasSuffix(m.Chat, "@lid") || strings.HasSuffix(m.Sender, "@lid") {
		b.mu.Lock()
		b.lidKey = ""
		b.mu.Unlock()
	}
	if m.Media != nil {
		m.Media.Path = fmt.Sprintf("/media/%d", seq)
	}
	b.publish(m)
	return m, true
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

// Subscribe returns a channel receiving every Message, MessageUpdate, Status and ChatsChanged.
// Slow subscribers miss events rather than block the bot.
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

type SendOptions struct {
	ReplyTo string
}

func (b *Bot) SendText(ctx context.Context, actor, to, text string, opt SendOptions) (Message, error) {
	if strings.TrimSpace(text) == "" {
		return Message{}, badInput("text is empty")
	}
	jid, quote, err := b.prepare(ctx, to, opt)
	if err != nil {
		b.Audit(actor, "send", to, "", err)
		return Message{}, err
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if quote != nil {
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: quote}}
	}
	m, err := b.deliver(ctx, jid, msg, Message{Type: "text", Text: text, ReplyTo: opt.ReplyTo})
	b.Audit(actor, "send", jid.String(), fmt.Sprintf("text, %d chars", len([]rune(text))), err)
	return m, err
}

// SendFile sends images as photos and everything else as a document.
func (b *Bot) SendFile(ctx context.Context, actor, to string, data []byte, fileName, caption string, opt SendOptions) (Message, error) {
	if len(data) == 0 {
		return Message{}, badInput("file is empty")
	}
	jid, quote, err := b.prepare(ctx, to, opt)
	if err != nil {
		b.Audit(actor, "send_file", to, fileName, err)
		return Message{}, err
	}
	m, err := b.sendFile(ctx, jid, quote, data, fileName, caption, opt)
	b.Audit(actor, "send_file", jid.String(), fmt.Sprintf("%s, %d bytes", fileName, len(data)), err)
	return m, err
}

func (b *Bot) sendFile(ctx context.Context, jid types.JID, quote *waE2E.ContextInfo, data []byte, fileName, caption string, opt SendOptions) (Message, error) {
	mime := http.DetectContentType(data)
	rec := Message{Type: "document", Text: cmp.Or(caption, fileName), ReplyTo: opt.ReplyTo,
		Media: &Media{Mimetype: mime, FileName: fileName, Size: uint64(len(data))}}
	mediaType := whatsmeow.MediaDocument
	if strings.HasPrefix(mime, "image/") {
		rec.Type, rec.Text, rec.Media.FileName, mediaType = "image", caption, "", whatsmeow.MediaImage
	}
	up, err := b.Client.Upload(ctx, data, mediaType)
	if err != nil {
		return Message{}, err
	}
	msg := &waE2E.Message{}
	if rec.Type == "image" {
		msg.ImageMessage = &waE2E.ImageMessage{
			Caption: proto.String(caption), Mimetype: proto.String(mime), ContextInfo: quote,
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		}
	} else {
		msg.DocumentMessage = &waE2E.DocumentMessage{
			Caption: proto.String(caption), Mimetype: proto.String(mime), ContextInfo: quote,
			FileName: proto.String(fileName), Title: proto.String(fileName),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		}
	}
	return b.deliver(ctx, jid, msg, rec)
}

// prepare resolves the recipient and, for replies, builds the quote of the original message.
func (b *Bot) prepare(ctx context.Context, to string, opt SendOptions) (types.JID, *waE2E.ContextInfo, error) {
	jid, err := b.resolveOnline(ctx, to)
	if err != nil {
		return jid, nil, err
	}
	if opt.ReplyTo == "" {
		return jid, nil, nil
	}
	orig, err := b.getMessage(b.phoneJID(jid).String(), opt.ReplyTo)
	if err != nil {
		return jid, nil, badInput("reply_to %q: no such message in this chat", opt.ReplyTo)
	}
	if orig.Deleted {
		return jid, nil, badInput("reply_to %q: that message was deleted", opt.ReplyTo)
	}
	return jid, b.quote(orig), nil
}

func (b *Bot) quote(orig Message) *waE2E.ContextInfo {
	quoted := &waE2E.Message{}
	if orig.Edited || len(orig.raw) == 0 || proto.Unmarshal(orig.raw, quoted) != nil {
		quoted = &waE2E.Message{Conversation: proto.String(orig.Text)}
	}
	participant := orig.rawSender
	if participant == "" {
		participant = b.selfJID()
	}
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(orig.ID),
		Participant:   proto.String(participant),
		QuotedMessage: quoted,
	}
}

// deliver applies rate limiting and the optional typing indicator, sends, and stores the result.
func (b *Bot) deliver(ctx context.Context, jid types.JID, msg *waE2E.Message, rec Message) (Message, error) {
	self := b.selfJID()
	if self == "" {
		return Message{}, errors.New("not logged in")
	}
	if err := b.throttle(ctx, jid); err != nil {
		return Message{}, err
	}
	if b.Typing && rec.Type == "text" {
		b.simulateTyping(ctx, jid, rec.Text)
	}
	resp, err := b.Client.SendMessage(ctx, jid, msg)
	if err != nil {
		return Message{}, err
	}
	rec.ID = resp.ID
	rec.Chat = b.phoneJID(jid).String()
	rec.Sender = self
	rec.FromMe = true
	rec.IsGroup = jid.Server == types.GroupServer
	rec.Timestamp = resp.Timestamp
	rec.rawChat = jid.String()
	rec.rawSender = rec.Sender
	rec.raw, _ = proto.Marshal(msg)
	stored, _ := b.store(rec)
	return stored, nil
}

func (b *Bot) simulateTyping(ctx context.Context, jid types.JID, text string) {
	d := time.Duration(len([]rune(text))) * 50 * time.Millisecond
	d = min(max(d, 800*time.Millisecond), 4*time.Second)
	_ = b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	_ = sleepUntil(ctx, time.Now().Add(d))
	_ = b.chatPresence(ctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
}

func (b *Bot) throttle(ctx context.Context, jid types.JID) error {
	slot, err := b.Limiter.Reserve(b.phoneJID(jid).String())
	if err != nil {
		return err
	}
	return sleepUntil(ctx, slot)
}

// sendControl sends an edit or revoke, which bypass deliver because they don't create a stored message.
func (b *Bot) sendControl(ctx context.Context, actor, action string, jid types.JID, id string, msg *waE2E.Message) error {
	err := b.throttle(ctx, jid)
	if err == nil {
		_, err = b.Client.SendMessage(ctx, jid, msg)
	}
	b.Audit(actor, action, jid.String(), id, err)
	return err
}

func sleepUntil(ctx context.Context, t time.Time) error {
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

// findTarget looks up a stored message for react/edit/delete/read.
func (b *Bot) findTarget(ctx context.Context, chat, id string) (Message, types.JID, error) {
	if id == "" {
		return Message{}, types.JID{}, badInput("id is required")
	}
	jid, err := b.resolveOnline(ctx, chat)
	if err != nil {
		return Message{}, jid, err
	}
	m, err := b.getMessage(b.phoneJID(jid).String(), id)
	if err != nil {
		return m, jid, err
	}
	if raw, err := types.ParseJID(m.rawChat); err == nil && m.rawChat != "" {
		jid = raw
	}
	return m, jid, nil
}

// senderJID is the original sender as BuildReaction/BuildRevoke want it: empty for our own messages.
func (m Message) senderJID() types.JID {
	if m.FromMe {
		return types.EmptyJID
	}
	jid, _ := types.ParseJID(m.rawSender)
	return jid
}

func (b *Bot) React(ctx context.Context, actor, chat, id, emoji string) (Message, error) {
	target, jid, err := b.findTarget(ctx, chat, id)
	if err != nil {
		b.Audit(actor, "react", chat, id, err)
		return Message{}, err
	}
	m, err := b.deliver(ctx, jid, b.Client.BuildReaction(jid, target.senderJID(), id, emoji), Message{Type: "reaction", Text: emoji, ReplyTo: id})
	b.Audit(actor, "react", jid.String(), fmt.Sprintf("%s on %s", emoji, id), err)
	return m, err
}

func (b *Bot) Edit(ctx context.Context, actor, chat, id, text string) (Message, error) {
	if strings.TrimSpace(text) == "" {
		return Message{}, badInput("text is empty")
	}
	target, jid, err := b.findTarget(ctx, chat, id)
	if err != nil {
		b.Audit(actor, "edit", chat, id, err)
		return Message{}, err
	}
	if !target.FromMe {
		return Message{}, badInput("you can only edit your own messages")
	}
	if time.Since(target.Timestamp) > whatsmeow.EditWindow {
		return Message{}, badInput("WhatsApp only allows edits within %s of sending", whatsmeow.EditWindow)
	}
	if target.Type != "text" {
		return Message{}, badInput("only text messages can be edited")
	}
	edit := b.Client.BuildEdit(jid, id, &waE2E.Message{Conversation: proto.String(text)})
	if err := b.sendControl(ctx, actor, "edit", jid, id, edit); err != nil {
		return Message{}, err
	}
	b.applyEdit(target.Chat, id, text)
	return b.getMessage(target.Chat, id)
}

func (b *Bot) Delete(ctx context.Context, actor, chat, id string) (Message, error) {
	target, jid, err := b.findTarget(ctx, chat, id)
	if err != nil {
		b.Audit(actor, "delete", chat, id, err)
		return Message{}, err
	}
	if !target.FromMe && !target.IsGroup {
		return Message{}, badInput("you can only delete other people's messages in groups where you're an admin")
	}
	if err := b.sendControl(ctx, actor, "delete", jid, id, b.Client.BuildRevoke(jid, target.senderJID(), id)); err != nil {
		return Message{}, err
	}
	b.applyDelete(target.Chat, id)
	return b.getMessage(target.Chat, id)
}

// MarkRead sends read receipts for the given incoming messages, or the latest 50 in the chat when ids is empty.
func (b *Bot) MarkRead(ctx context.Context, actor, chat string, ids []string) (int, error) {
	jid, err := b.resolveOnline(ctx, chat)
	if err != nil {
		return 0, err
	}
	key := b.phoneJID(jid).String()
	var msgs []Message
	if len(ids) == 0 {
		f := false
		msgs = b.Query(Filter{Chat: key, ChatOnly: true, FromMe: &f}, 50, false)
	} else {
		for _, id := range ids {
			if m, err := b.getMessage(key, id); err == nil && !m.FromMe {
				msgs = append(msgs, m)
			}
		}
	}
	type group struct{ chat, sender string }
	batches := map[group][]types.MessageID{}
	for _, m := range msgs {
		if m.Chat != key || m.Type == "reaction" {
			continue
		}
		g := group{cmp.Or(m.rawChat, m.Chat), cmp.Or(m.rawSender, m.Sender)}
		batches[g] = append(batches[g], m.ID)
	}
	n := 0
	for g, batch := range batches {
		chatJID, err1 := types.ParseJID(g.chat)
		senderJID, err2 := types.ParseJID(g.sender)
		if err1 != nil || err2 != nil {
			continue
		}
		if err := b.Client.MarkRead(ctx, batch, time.Now(), chatJID, senderJID); err != nil {
			b.Audit(actor, "read", key, fmt.Sprintf("%d messages", n), err)
			return n, err
		}
		n += len(batch)
	}
	if n > 0 {
		b.Audit(actor, "read", key, fmt.Sprintf("%d messages", n), nil)
	}
	return n, nil
}

func (b *Bot) SetTyping(ctx context.Context, chat, state string) error {
	if !slices.Contains([]string{"", "typing", "recording", "paused"}, state) {
		return badInput("state must be typing, recording or paused")
	}
	jid, err := b.resolveOnline(ctx, chat)
	if err == nil {
		err = b.lookup(ctx, 1)
	}
	if err != nil {
		return err
	}
	switch state {
	case "typing", "":
		return b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	case "recording":
		return b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaAudio)
	case "paused":
		return b.chatPresence(ctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}
	return badInput("state must be typing, recording or paused")
}

// DownloadMedia fetches and decrypts the media of a stored message.
func (b *Bot) DownloadMedia(ctx context.Context, seq int64) ([]byte, Message, error) {
	m, err := b.getMessageBySeq(seq)
	if err != nil {
		return nil, m, err
	}
	if m.Media == nil || len(m.raw) == 0 {
		return nil, m, errNotFound
	}
	if m.Media.Size > maxMediaDownload {
		return nil, m, badInput("file is %d MB, over the %d MB download limit", m.Media.Size>>20, maxMediaDownload>>20)
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(m.raw, &msg); err != nil {
		return nil, m, err
	}
	_, _, _, media := classify(&msg)
	if media == nil {
		return nil, m, errNotFound
	}
	data, err := b.Client.Download(ctx, media)
	return data, m, err
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
		name := cmp.Or(c.FullName, c.FirstName, c.BusinessName, c.PushName)
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
	slices.SortFunc(out, func(x, y Contact) int { return cmp.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)) })
	return out, nil
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
	if err := b.lookup(ctx, 1); err != nil {
		return types.JID{}, err
	}
	res, err := b.Client.IsOnWhatsApp(ctx, []string{"+" + jid.User})
	if err != nil {
		return types.JID{}, fmt.Errorf("lookup %s: %w", to, err)
	}
	for _, r := range res {
		if r.IsIn {
			b.mu.Lock()
			if len(b.jids) > 5000 {
				clear(b.jids)
			}
			b.jids[jid.User] = r.JID
			b.mu.Unlock()
			return r.JID, nil
		}
	}
	return types.JID{}, badInput("+%s is not on WhatsApp", jid.User)
}

func (b *Bot) resolveOnline(ctx context.Context, to string) (types.JID, error) {
	if !b.Client.IsLoggedIn() {
		return types.JID{}, errors.New("not logged in")
	}
	return b.Resolve(ctx, to)
}

// ParseRecipient accepts "+55 11 99999-9999", "5511999999999" or a full JID like "1203...@g.us".
func ParseRecipient(to string) (types.JID, error) {
	to = strings.TrimSpace(to)
	if strings.Contains(to, "@") {
		jid, err := types.ParseJID(to)
		if err != nil || jid.User == "" {
			return types.JID{}, badInput("invalid JID %q", to)
		}
		return jid, nil
	}
	d := digits(to)
	if len(d) < 8 || len(d) > 15 {
		return types.JID{}, badInput("invalid recipient %q", to)
	}
	return types.NewJID(d, types.DefaultUserServer), nil
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// lookup charges n number checks (or presence updates) to the Lookups limiter.
func (b *Bot) lookup(ctx context.Context, n int) error {
	slot, err := b.Lookups.ReserveN("lookup", n)
	if err != nil {
		return err
	}
	return sleepUntil(ctx, slot)
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

// limitedTransport refuses media downloads bigger than maxMediaDownload, whatever size the sender declared.
type limitedTransport struct{ http.RoundTripper }

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err == nil && req.Method == http.MethodGet {
		resp.Body = &cappedBody{rc: resp.Body, left: maxMediaDownload}
	}
	return resp, err
}

// cappedBody fails with a plain (non-network) error past the limit, which whatsmeow treats as final.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, badInput("file is over the %d MB download limit", maxMediaDownload>>20)
	}
	n, err := c.rc.Read(p[:min(int64(len(p)), c.left)])
	c.left -= int64(n)
	if c.left <= 0 && err == nil {
		if extra, _ := c.rc.Read(make([]byte, 1)); extra > 0 {
			return n, badInput("file is over the %d MB download limit", maxMediaDownload>>20)
		}
		err = io.EOF
	}
	return n, err
}

func (c *cappedBody) Close() error { return c.rc.Close() }

func (b *Bot) selfJID() string {
	if id := b.Client.Store.ID; id != nil {
		return id.ToNonAD().String()
	}
	return ""
}

// announcePresence marks the account "online" only with -online: an always-online linked device
// stops the phone from showing notifications.
func (b *Bot) announcePresence() {
	presence := types.PresenceUnavailable
	if b.Online {
		presence = types.PresenceAvailable
	}
	if err := b.Client.SendPresence(context.Background(), presence); err != nil && !errors.Is(err, whatsmeow.ErrNoPushName) {
		b.Log.Warn("set presence failed", "err", err)
	}
}

// chatPresence shows typing/recording. WhatsApp only relays it while we're "available", so without
// -online we go available while any chat shows an indicator, and back once all are paused or expired.
func (b *Bot) chatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	if !b.Online {
		b.trackTyping(jid.String(), state == types.ChatPresenceComposing)
	}
	return b.Client.SendChatPresence(ctx, jid, state, media)
}

func (b *Bot) trackTyping(chat string, typing bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.typingIn == nil {
		b.typingIn = map[string]*time.Timer{}
	}
	wasIdle := len(b.typingIn) == 0
	if t, ok := b.typingIn[chat]; ok {
		t.Stop()
		delete(b.typingIn, chat)
	}
	if typing {
		b.typingIn[chat] = time.AfterFunc(25*time.Second, func() { b.trackTyping(chat, false) })
	}
	switch idle := len(b.typingIn) == 0; {
	case wasIdle && !idle:
		go b.Client.SendPresence(context.Background(), types.PresenceAvailable)
	case !wasIdle && idle:
		go b.Client.SendPresence(context.Background(), types.PresenceUnavailable)
	}
}
