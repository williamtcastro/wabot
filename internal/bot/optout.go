package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	jid, err := types.ParseJID(m.Chat)
	if kw == "" || err != nil || !isPersonChat(jid) {
		return
	}
	if out, err := b.Store.OptedOut(b.optOutKeys(jid)...); err != nil || out {
		if err != nil {
			b.Log.Error("opt-out: store", "err", err)
		}
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

// errOptOutCheck marks a failed opt-out lookup: the send is retried, not counted as a WhatsApp error.
var errOptOutCheck = errors.New("opt-out check")

// optOutKeys lists every key a person's opt-out may be stored under: the phone JID and, when
// WhatsApp told us, its hidden @lid (an opt-out arriving before the link is known stays under it).
func (b *Bot) optOutKeys(jid types.JID) []string {
	pn := b.PhoneJID(jid)
	keys := []string{pn.String()}
	if pn.Server == types.DefaultUserServer && b.Client.Store.LIDs != nil {
		if lid, err := b.Client.Store.LIDs.GetLIDForPN(context.Background(), pn); err == nil && !lid.IsEmpty() {
			keys = append(keys, lid.String())
		}
	}
	return keys
}

// optedOut returns an InputError when the recipient jid, resolved from to, opted out of queued
// messages. to's own form counts too: an opt-out added by number while logged out was stored as
// typed, which may differ from the JID WhatsApp resolves it to.
func (b *Bot) optedOut(jid types.JID, to string) error {
	keys := b.optOutKeys(jid)
	if !strings.Contains(to, "@") {
		if local, err := ParseRecipient(to); err == nil && local != jid {
			keys = append(keys, b.optOutKeys(local)...)
		}
	}
	out, err := b.Store.OptedOut(keys...)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", errOptOutCheck, err)
	case out:
		return BadInput(errOptedOut)
	}
	return nil
}

func (b *Bot) OptOuts() ([]store.OptOut, error) { return b.Store.OptOuts() }

// optOutChat turns a number or JID into the chat key the outbox compares against, asking WhatsApp
// for a number's real JID when logged in (as a send would) and falling back to the number as typed.
func (b *Bot) optOutChat(ctx context.Context, to string) (string, error) {
	jid, err := ParseRecipient(to)
	if err != nil {
		return "", err
	}
	if !isPersonChat(jid) {
		return "", BadInput("only a person's chat can opt out, not %s", jid.Server)
	}
	if b.Client.IsLoggedIn() && !strings.Contains(to, "@") {
		chat, err := b.CanonicalChat(ctx, to)
		if _, isInput := errors.AsType[InputError](err); err == nil || isInput {
			return chat, err
		}
	}
	return b.LocalChat(to)
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

// RemoveOptOut takes a chat off the list under every key it may be stored under: as typed, its
// @lid, and for a number the JID WhatsApp resolves it to.
func (b *Bot) RemoveOptOut(ctx context.Context, actor, to string) (err error) {
	chat, err := b.LocalChat(to)
	defer func() { b.Audit(actor, "optout_remove", cmp.Or(chat, to), "", err) }()
	if err != nil {
		return err
	}
	jid, _ := types.ParseJID(chat)
	keys := b.optOutKeys(jid)
	if b.Client.IsLoggedIn() && !strings.Contains(to, "@") {
		if canon, cerr := b.Resolve(ctx, to); cerr == nil && canon != jid {
			keys = append(keys, b.optOutKeys(canon)...)
		}
	}
	return b.Store.RemoveOptOut(keys...)
}
