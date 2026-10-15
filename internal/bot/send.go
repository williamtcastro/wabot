package bot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/williamtcastro/wame/internal/store"
)

type SendOptions struct {
	ReplyTo string

	queued    int64
	messageID types.MessageID
}

func (b *Bot) SendText(ctx context.Context, actor, to, text string, opt SendOptions) (store.Message, error) {
	detail := fmt.Sprintf("text, %d chars", len([]rune(text)))
	if opt.queued > 0 {
		detail += fmt.Sprintf(", queue #%d", opt.queued)
	}
	return b.audited(actor, "send", to, detail, func() (store.Message, error) {
		if strings.TrimSpace(text) == "" {
			return store.Message{}, BadInput("text is empty")
		}
		jid, quote, err := b.prepare(ctx, to, opt)
		if err != nil {
			return store.Message{}, err
		}
		// The outbox checked before taking a slot; this covers an opt-out that arrived since.
		if opt.queued > 0 {
			if err := b.optedOut(jid, to); err != nil {
				return store.Message{}, err
			}
		}
		msg := &waE2E.Message{Conversation: new(text)}
		if quote != nil {
			msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: new(text), ContextInfo: quote}}
		}
		return b.deliver(ctx, jid, msg, store.Message{Type: "text", Text: text, ReplyTo: opt.ReplyTo}, opt)
	})
}

// SendFile sends images as photos and everything else as a document.
func (b *Bot) SendFile(ctx context.Context, actor, to string, data []byte, fileName, caption string, opt SendOptions) (store.Message, error) {
	return b.audited(actor, "send_file", to, fmt.Sprintf("%s, %d bytes", fileName, len(data)), func() (store.Message, error) {
		if len(data) == 0 {
			return store.Message{}, BadInput("file is empty")
		}
		jid, quote, err := b.prepare(ctx, to, opt)
		if err != nil {
			return store.Message{}, err
		}
		return b.sendFile(ctx, jid, quote, data, fileName, caption, opt)
	})
}

func (b *Bot) sendFile(ctx context.Context, jid types.JID, quote *waE2E.ContextInfo, data []byte, fileName, caption string, opt SendOptions) (store.Message, error) {
	mime := http.DetectContentType(data)
	photo := strings.HasPrefix(mime, "image/")
	mediaType := whatsmeow.MediaDocument
	if photo {
		mediaType = whatsmeow.MediaImage
	}
	up, err := b.uploadMedia(ctx, data, mediaType)
	if err != nil {
		return store.Message{}, uploadError(err)
	}
	msg, rec := fileMessage(up, photo, mime, fileName, caption, quote, uint64(len(data)))
	rec.ReplyTo = opt.ReplyTo
	return b.deliver(ctx, jid, msg, rec, opt)
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
	orig, err := b.Store.Get(b.PhoneJID(jid).String(), opt.ReplyTo)
	if err != nil {
		return jid, nil, BadInput("reply_to %q: no such message in this chat", opt.ReplyTo)
	}
	if orig.Deleted {
		return jid, nil, BadInput("reply_to %q: that message was deleted", opt.ReplyTo)
	}
	return jid, b.quote(orig), nil
}

func (b *Bot) quote(orig store.Message) *waE2E.ContextInfo {
	quoted := &waE2E.Message{}
	if orig.Edited || len(orig.Raw) == 0 || proto.Unmarshal(orig.Raw, quoted) != nil {
		quoted = &waE2E.Message{Conversation: new(orig.Text)}
	}
	participant := orig.RawSender
	if participant == "" {
		participant = b.selfJID()
	}
	return &waE2E.ContextInfo{
		StanzaID:      new(orig.ID),
		Participant:   new(participant),
		QuotedMessage: quoted,
	}
}

// deliver applies rate limiting (unless the outbox already took a slot) and the optional typing
// indicator, sends, and stores the result.
func (b *Bot) deliver(ctx context.Context, jid types.JID, msg *waE2E.Message, rec store.Message, opt SendOptions) (store.Message, error) {
	self := b.selfJID()
	if self == "" {
		return store.Message{}, errors.New("not logged in")
	}
	if opt.queued == 0 {
		if err := b.throttle(ctx, jid); err != nil {
			return store.Message{}, err
		}
	}
	if b.Typing && rec.Type == "text" {
		b.simulateTyping(ctx, jid, rec.Text)
	}
	resp, err := b.Client.SendMessage(ctx, jid, msg, whatsmeow.SendRequestExtra{ID: opt.messageID})
	if err != nil {
		return store.Message{}, err
	}
	rec.ID = resp.ID
	rec.Chat = b.PhoneJID(jid).String()
	rec.Sender = self
	rec.FromMe = true
	rec.IsGroup = jid.Server == types.GroupServer
	rec.Timestamp = resp.Timestamp
	rec.RawChat = jid.String()
	rec.RawSender = rec.Sender
	rec.Raw, _ = proto.Marshal(msg)
	stored, _ := b.save(rec)
	return stored, nil
}

func (b *Bot) simulateTyping(ctx context.Context, jid types.JID, text string) {
	d := time.Duration(len([]rune(text))) * 50 * time.Millisecond
	d = min(max(d, 800*time.Millisecond), 4*time.Second)
	_ = b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	_ = SleepUntil(ctx, time.Now().Add(d))
	_ = b.chatPresence(ctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
}

func (b *Bot) throttle(ctx context.Context, jid types.JID) error {
	return b.Limiter.Wait(ctx, b.PhoneJID(jid).String(), 1)
}

// sendControl sends an edit or revoke, which bypass deliver because they don't create a stored message.
func (b *Bot) sendControl(ctx context.Context, jid types.JID, msg *waE2E.Message) error {
	err := b.throttle(ctx, jid)
	if err == nil {
		_, err = b.Client.SendMessage(ctx, jid, msg)
	}
	return err
}
