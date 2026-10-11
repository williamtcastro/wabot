package bot

import (
	"cmp"
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/williamtcastro/wabot/internal/store"
)

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

func (b *Bot) toMessage(v *events.Message) store.Message {
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
	m := store.Message{
		ID:        v.Info.ID,
		Chat:      b.PhoneJID(chat.ToNonAD()).String(),
		Sender:    b.PhoneJID(sender.ToNonAD()).String(),
		PushName:  v.Info.PushName,
		FromMe:    v.Info.IsFromMe,
		IsGroup:   v.Info.IsGroup,
		Type:      kind,
		Text:      text,
		ReplyTo:   replyTo,
		Timestamp: v.Info.Timestamp,
		RawChat:   v.Info.Chat.ToNonAD().String(),
		RawSender: v.Info.Sender.ToNonAD().String(),
	}
	if media != nil {
		m.Media = &store.Media{Mimetype: cmp.Or(media.GetMimetype(), "application/octet-stream"), Size: media.GetFileLength()}
		if d, ok := media.(*waE2E.DocumentMessage); ok {
			m.Media.FileName = d.GetFileName()
		}
	}
	m.Raw, _ = proto.Marshal(v.Message)
	return m
}

// save stores m and, if it's new, publishes it with its seq filled in.
func (b *Bot) save(m store.Message) (store.Message, bool) {
	stored, inserted, err := b.Store.Save(m)
	if err != nil {
		b.Log.Error("save message", "chat", m.Chat, "id", m.ID, "err", err)
		return m, false
	}
	if !inserted {
		return m, false
	}
	b.publish(stored)
	return stored, true
}

// PhoneJID maps hidden @lid user IDs back to phone-number JIDs when WhatsApp has told us the link,
// so a person's chat stays in one place no matter which ID a message used.
func (b *Bot) PhoneJID(jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer || b.Client.Store.LIDs == nil {
		return jid
	}
	pn, err := b.Client.Store.LIDs.GetPNForLID(context.Background(), jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn
}

// senderJID is the original sender as BuildReaction/BuildRevoke want it: empty for our own messages.
func senderJID(m store.Message) types.JID {
	if m.FromMe {
		return types.EmptyJID
	}
	jid, _ := types.ParseJID(m.RawSender)
	return jid
}
