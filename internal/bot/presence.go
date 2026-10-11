package bot

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

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
