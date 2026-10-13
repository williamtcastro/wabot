package bot

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

// optOutKeywords are compared with the whole message, folded (lowercase, no accents).
var optOutKeywords = []string{"stop", "sair", "parar", "cancelar", "unsubscribe", "descadastrar"}

const errOptedOut = "recipient opted out"

func optOutKeyword(text string) string {
	if f := store.Fold(strings.TrimSpace(text)); slices.Contains(optOutKeywords, f) {
		return strings.ToUpper(f)
	}
	return ""
}

func isPersonChat(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer
}

// noteOptOut lists the sender of an incoming one-to-one message that is just an opt-out keyword.
// It never answers: the person only stops getting queued messages.
func (b *Bot) noteOptOut(m store.Message) {
	if m.FromMe || m.IsGroup || m.Type != "text" {
		return
	}
	kw := optOutKeyword(m.Text)
	if jid, err := types.ParseJID(m.Chat); kw == "" || err != nil || !isPersonChat(jid) {
		return
	}
	added, err := b.Store.AddOptOut(store.OptOut{Chat: m.Chat, Reason: kw, At: time.Now(), By: "whatsapp"})
	if err != nil {
		b.Log.Error("opt-out: store", "err", err)
		return
	}
	if added {
		b.Audit("whatsapp", "optout_add", m.Chat, "keyword "+kw, nil)
	}
}

// checkOptOut returns an InputError when chat (a canonical chat key) opted out of queued messages.
func (b *Bot) checkOptOut(chat string) error {
	out, err := b.Store.OptedOut(chat)
	if err == nil && out {
		err = BadInput(errOptedOut)
	}
	return err
}

func (b *Bot) OptOuts() ([]store.OptOut, error) { return b.Store.OptOuts() }

// optOutChat turns a number or JID into the chat key the outbox compares against, asking WhatsApp
// for a number's real JID when logged in (as a send would).
func (b *Bot) optOutChat(ctx context.Context, to string) (string, error) {
	jid, err := ParseRecipient(to)
	if err != nil {
		return "", err
	}
	if !isPersonChat(jid) {
		return "", BadInput("only a person's chat can opt out, not %s", jid.Server)
	}
	if b.Client.IsLoggedIn() && !strings.Contains(to, "@") {
		return b.CanonicalChat(ctx, to)
	}
	return b.PhoneJID(jid).String(), nil
}

// AddOptOut lists a chat by hand. added is false when it was already listed.
func (b *Bot) AddOptOut(ctx context.Context, actor, to string) (o store.OptOut, added bool, err error) {
	defer func() {
		detail := "added"
		if err == nil && !added {
			detail = "already listed"
		}
		b.Audit(actor, "optout_add", cmp.Or(o.Chat, to), detail, err)
	}()
	if o.Chat, err = b.optOutChat(ctx, to); err != nil {
		return o, false, err
	}
	o.Reason, o.At, o.By = "manual", time.Now(), actor
	added, err = b.Store.AddOptOut(o)
	return o, added, err
}

// RemoveOptOut takes a chat off the list, by the key it was stored under or, for a number, the JID
// WhatsApp resolves it to.
func (b *Bot) RemoveOptOut(ctx context.Context, actor, to string) (err error) {
	chat, err := b.LocalChat(to)
	defer func() { b.Audit(actor, "optout_remove", cmp.Or(chat, to), "", err) }()
	if err != nil {
		return err
	}
	err = b.Store.RemoveOptOut(chat)
	if errors.Is(err, store.ErrNotFound) && b.Client.IsLoggedIn() && !strings.Contains(to, "@") {
		if canon, cerr := b.CanonicalChat(ctx, to); cerr == nil && canon != chat {
			chat, err = canon, b.Store.RemoveOptOut(canon)
		}
	}
	return err
}
