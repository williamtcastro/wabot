package bot

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const typingExpiry = 25 * time.Second

// announcePresence (re)sends our presence after connecting: "online" only with -online or while a
// typing indicator shows, since an always-online linked device stops the phone from showing
// notifications.
func (b *Bot) announcePresence() { b.sendPresence(b.ctx, true) }

// wantedPresence needs b.mu held.
func (b *Bot) wantedPresence() types.Presence {
	if b.Online || len(b.typingIn) > 0 {
		return types.PresenceAvailable
	}
	return types.PresenceUnavailable
}

// sendPresence sends the presence wanted right now unless it was the last one sent. Sends are
// serialized and each reads the current state, so the latest change always goes out last.
func (b *Bot) sendPresence(ctx context.Context, force bool) {
	b.presenceMu.Lock()
	defer b.presenceMu.Unlock()
	b.mu.RLock()
	want := b.wantedPresence()
	b.mu.RUnlock()
	if !force && want == b.presenceSent {
		return
	}
	if err := b.Client.SendPresence(ctx, want); err != nil {
		b.presenceSent = ""
		if !errors.Is(err, whatsmeow.ErrNoPushName) {
			b.Log.Warn("set presence failed", "err", err)
		}
		return
	}
	b.presenceSent = want
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
		var t *time.Timer
		t = time.AfterFunc(typingExpiry, func() { b.typingExpired(chat, t) })
		b.typingIn[chat] = t
	}
	if wasIdle != (len(b.typingIn) == 0) {
		b.Go(func(ctx context.Context) { b.sendPresence(ctx, false) })
	}
}

// typingExpired drops chat's indicator unless t was replaced meanwhile: a timer that fired while
// trackTyping held b.mu must not remove the newer one.
func (b *Bot) typingExpired(chat string, t *time.Timer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.typingIn[chat] != t {
		return
	}
	delete(b.typingIn, chat)
	if len(b.typingIn) == 0 {
		b.Go(func(ctx context.Context) { b.sendPresence(ctx, false) })
	}
}
