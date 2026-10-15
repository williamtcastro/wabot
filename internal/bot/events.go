package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/williamtcastro/wame/internal/store"
)

func (b *Bot) handle(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		b.setState("connected", "")
		b.announcePresence()
		b.Go(b.refreshGroups)
	case *events.Receipt:
		// "read-self" means we read the chat on another device, e.g. the phone.
		if v.Type == types.ReceiptTypeReadSelf || (v.IsFromMe && v.Type == types.ReceiptTypeRead) {
			chat := b.PhoneJID(v.Chat.ToNonAD()).String()
			if seq := b.Store.MaxSeqOf(chat, v.MessageIDs); seq > 0 && b.Store.MarkReadUpTo(chat, seq) {
				b.publish(store.ChatRead{Chat: chat, Seq: seq})
			}
		} else if !v.IsFromMe && (v.Type == types.ReceiptTypeDelivered || v.Type == types.ReceiptTypeRead) {
			b.outboxReceipt(v.MessageIDs, v.Type == types.ReceiptTypeRead, v.Timestamp)
		}
	case *events.PushNameSetting, *events.AppStateSyncComplete:
		b.announcePresence()
	case *events.Contact, *events.PushName:
		b.namesChanged()
	case *events.BusinessName:
		b.cacheEvent(v.JID, func(jid string) (bool, error) {
			return b.Store.CacheVerifiedName(jid, capRunes(cleanName(v.NewBusinessName), maxVerifiedName))
		})
		b.namesChanged()
	case *events.Picture:
		id := capRunes(v.PictureID, maxPictureID)
		if v.Remove {
			id = ""
		}
		b.cacheEvent(v.JID, func(jid string) (bool, error) { return b.Store.CachePicture(jid, id) })
	case *events.UserAbout:
		b.cacheEvent(v.JID, func(jid string) (bool, error) {
			return b.Store.CacheAbout(jid, capRunes(StripInvisible(v.Status), maxAbout), v.Timestamp)
		})
	case *events.Disconnected:
		b.setState("disconnected", "")
	case *events.LoggedOut:
		b.setState("logged_out", "")
		b.Audit("whatsapp", "logged_out", "", fmt.Sprint(v.Reason), nil)
		b.wipeContacts("whatsapp")
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
		m, inserted := b.save(m)
		if !inserted {
			return
		}
		if b.Webhook != nil {
			b.Webhook.Deliver("message", m)
		}
		b.noteOptOut(m)
		if b.replies.Load() && (!m.FromMe || b.isSelfChat(m.Chat)) {
			b.autoReply(m)
		}
	}
}

func (b *Bot) handleProtocol(v *events.Message, pm *waE2E.ProtocolMessage) {
	actor := b.toMessage(v)
	target, err := b.Store.Get(actor.Chat, pm.GetKey().GetID())
	if err != nil {
		return
	}
	sameAuthor := actor.FromMe == target.FromMe && (actor.FromMe || actor.Sender == target.Sender)
	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		switch {
		case sameAuthor || (target.IsGroup && actor.FromMe):
			b.applyDelete(target.Chat, target.ID)
		case target.IsGroup:
			// The admin check may have to fetch the group list, which mustn't block the event handler.
			b.Go(func(ctx context.Context) {
				if b.isGroupAdmin(ctx, target.Chat, actor.Sender) {
					b.applyDelete(target.Chat, target.ID)
				}
			})
		}
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		if sameAuthor && target.Type == "text" {
			_, text, _, _ := classify(pm.GetEditedMessage())
			b.applyEdit(target.Chat, target.ID, text)
		}
	}
}

func (b *Bot) applyEdit(chat, id, text string) {
	if b.Store.MarkEdited(chat, id, text) == nil {
		b.publishUpdate(chat, id)
	}
}

func (b *Bot) applyDelete(chat, id string) {
	if b.Store.MarkDeleted(chat, id) == nil {
		b.publishUpdate(chat, id)
	}
}

func (b *Bot) publishUpdate(chat, id string) {
	m, err := b.Store.Get(chat, id)
	if err != nil {
		return
	}
	b.publish(store.MessageUpdate{Message: m})
	if b.Webhook != nil {
		b.Webhook.Deliver("update", m)
	}
}

// importHistory stores the recent conversations WhatsApp sends right after a fresh login.
func (b *Bot) importHistory(h *events.HistorySync) {
	var batch []store.Message
	for _, conv := range h.Data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		for _, hm := range conv.GetMessages() {
			evt, err := b.Client.ParseWebMessage(chat, hm.GetMessage())
			if err != nil || evt.Message.GetProtocolMessage() != nil {
				continue
			}
			m := b.toMessage(evt)
			if m.Type == "other" {
				continue
			}
			// ParseWebMessage turns an edit into the new content under the original ID.
			m.Edited = evt.IsEdit
			batch = append(batch, m)
		}
	}
	if b.Store.Import(batch) == nil {
		b.publish(store.ChatsChanged{})
	}
}

func autoReplyText(text string) string {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "!ping":
		return "pong 🏓"
	case "!time":
		return time.Now().Format(time.RFC1123)
	case "!help":
		return "Commands: !ping, !time, !help"
	}
	return ""
}

// autoReply answers a command message in the background; anything else costs nothing.
func (b *Bot) autoReply(m store.Message) {
	reply := autoReplyText(m.Text)
	if reply == "" {
		return
	}
	if _, err := b.autoLimit.Reserve(m.Chat); err != nil {
		b.Log.Debug("auto-reply dropped", "chat", m.Chat, "err", err)
		return
	}
	b.Go(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := b.SendText(ctx, "autoreply", m.Chat, reply, SendOptions{ReplyTo: m.ID}); err != nil {
			b.Log.Warn("auto-reply failed", "chat", m.Chat, "err", err)
		}
	})
}

// cacheEvent applies a profile notification to the person's cached profile, if there is one: the
// update changes an existing row only, so strangers' notifications can't grow the table. Groups
// are ignored.
func (b *Bot) cacheEvent(from types.JID, update func(jid string) (bool, error)) {
	if from.Server != types.DefaultUserServer && from.Server != types.HiddenUserServer {
		return
	}
	if _, err := update(b.PhoneJID(from.ToNonAD()).String()); err != nil {
		b.Log.Warn("profile cache: update", "err", err)
	}
}

// wipeContacts deletes everything wame keeps about people when the account logs out: your own
// contacts, the WhatsApp profile cache and group members (messages and the audit log stay). It is
// audited as contacts_wipe with the number of own contacts.
func (b *Bot) wipeContacts(actor string) {
	b.contactsMu.Lock()
	n, err := b.Store.ClearContactState()
	b.mu.Lock()
	b.own, b.ownLIDs, b.refreshes = nil, 0, nil
	b.mu.Unlock()
	b.contactsMu.Unlock()
	b.Audit(actor, "contacts_wipe", "", fmt.Sprintf("%d contacts", n), err)
	if err == nil {
		b.namesChanged()
	}
}
