package bot

import (
	"cmp"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

// Audit records a control action in the wame_audit table and the log. Message text is never stored.
func (b *Bot) Audit(actor, action, target, detail string, err error) {
	e := store.AuditEntry{At: time.Now(), Actor: actor, Action: action, Target: target, Detail: detail, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
	}
	dbErr := b.Store.Audit(e)
	attrs := []any{"actor", actor, "action", action, "target", target, "detail", detail}
	switch {
	case dbErr != nil:
		b.Log.Error("audit write failed", append(attrs, "err", dbErr)...)
	case err != nil:
		b.Log.Warn("action failed", append(attrs, "err", err)...)
	default:
		b.Log.Info("action", attrs...)
	}
}

// audited runs one control action and records it in the audit log exactly once, whatever the outcome.
func (b *Bot) audited(actor, action, target, detail string, fn func() (store.Message, error)) (store.Message, error) {
	m, err := fn()
	b.Audit(actor, action, cmp.Or(m.Chat, target), detail, err)
	if action == "send" || action == "send_file" {
		b.recordSend(actor, err == nil)
	}
	return m, err
}
