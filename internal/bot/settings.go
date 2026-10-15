package bot

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	// The distroless image has no zoneinfo, and queue_timezone needs it.
	_ "time/tzdata"

	"github.com/williamtcastro/wame/internal/store"
)

// Settings are the values an admin can change while wame runs. The flags give the defaults;
// overrides live in wame_settings and win over the flags on the next start too.
type Settings struct {
	Rate          int
	ChatGap       time.Duration
	MaxWait       time.Duration
	AutoReply     bool
	WebhookFilter string
	QueuePaused   bool
	QueueJitter   time.Duration
	QuietHours    QuietHours
	QuietOff      bool           // switches the window off without forgetting it
	Timezone      *time.Location // nil means time.Local
	// QueueMediaTTL is how long a queued file's WhatsApp upload is reused (0: upload per message).
	QueueMediaTTL time.Duration
	// QueueFilesQuota caps the bytes of all queued files together.
	QueueFilesQuota int64
}

// SettingKeys are the names used by the API, the table and the audit log, in display order.
var SettingKeys = []string{"rate", "chat_gap", "max_wait", "auto_reply", "webhook_filter", "queue_paused", "queue_jitter",
	"queue_quiet_hours", "queue_quiet", "queue_timezone", "queue_media_ttl", "queue_files_quota"}

const maxSettingDuration = 10 * time.Minute

// SettingView is one setting as GET /settings shows it.
type SettingView struct {
	Key        string    `json:"key"`
	Value      string    `json:"value"`
	Default    string    `json:"default"`
	Overridden bool      `json:"overridden"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

func (s Settings) get(key string) string {
	switch key {
	case "rate":
		return strconv.Itoa(s.Rate)
	case "chat_gap":
		return s.ChatGap.String()
	case "max_wait":
		return s.MaxWait.String()
	case "auto_reply":
		return strconv.FormatBool(s.AutoReply)
	case "webhook_filter":
		return s.WebhookFilter
	case "queue_paused":
		return strconv.FormatBool(s.QueuePaused)
	case "queue_jitter":
		return s.QueueJitter.String()
	case "queue_quiet_hours":
		return s.QuietHours.String()
	case "queue_quiet":
		return strconv.FormatBool(!s.QuietOff)
	case "queue_timezone":
		return zoneName(s.location())
	case "queue_media_ttl":
		if s.QueueMediaTTL == 0 {
			return "off"
		}
		return s.QueueMediaTTL.String()
	case "queue_files_quota":
		return FormatBytes(s.QueueFilesQuota)
	}
	return ""
}

// quietUntil is when the current quiet hours end, or zero when the queue may send now.
func (s Settings) quietUntil(now time.Time) time.Time {
	if s.QuietOff {
		return time.Time{}
	}
	return s.QuietHours.Until(now, s.location())
}

func (s Settings) location() *time.Location {
	if s.Timezone == nil {
		return time.Local
	}
	return s.Timezone
}

func (s *Settings) set(key, value string) error {
	value = strings.TrimSpace(value)
	duration := func() (time.Duration, error) {
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 || d > maxSettingDuration {
			return 0, BadInput("%s must be a duration between 0s and %s, like 2s", key, maxSettingDuration)
		}
		return d, nil
	}
	var err error
	switch key {
	case "rate":
		n, perr := strconv.Atoi(value)
		if perr != nil || n < 0 || n > 1000 {
			return BadInput("rate must be a whole number from 0 (unlimited) to 1000")
		}
		s.Rate = n
	case "chat_gap":
		s.ChatGap, err = duration()
	case "max_wait":
		s.MaxWait, err = duration()
	case "auto_reply", "queue_paused", "queue_quiet":
		b, perr := strconv.ParseBool(value)
		if perr != nil {
			return BadInput("%s must be true or false", key)
		}
		switch key {
		case "auto_reply":
			s.AutoReply = b
		case "queue_paused":
			s.QueuePaused = b
		default:
			s.QuietOff = !b
		}
	case "queue_jitter":
		s.QueueJitter, err = duration()
	case "queue_quiet_hours":
		q, perr := ParseQuietHours(value)
		if perr != nil {
			return BadInput("queue_quiet_hours: %v", perr)
		}
		s.QuietHours = q
	case "queue_timezone":
		loc, perr := ParseTimezone(value)
		if perr != nil {
			return BadInput("queue_timezone: %v", perr)
		}
		s.Timezone = loc
	case "queue_media_ttl":
		if strings.EqualFold(value, "off") {
			value = "0s"
		}
		d, perr := time.ParseDuration(value)
		if perr != nil || d < 0 || d > maxQueueMediaTTL {
			return BadInput("queue_media_ttl must be off or a duration up to %s, like 24h", maxQueueMediaTTL)
		}
		s.QueueMediaTTL = d
	case "queue_files_quota":
		n, perr := ParseBytes(value)
		if perr != nil {
			return BadInput("queue_files_quota %v", perr)
		}
		s.QueueFilesQuota = n
	case "webhook_filter":
		if _, err := ParseWebhookFilter(value); err != nil {
			return BadInput("webhook_filter: %v", err)
		}
		s.WebhookFilter = value
	default:
		return BadInput("unknown setting %q (known: %s)", key, strings.Join(SettingKeys, ", "))
	}
	return err
}

// ParseWebhookFilter parses a -webhook-filter query. A phone number in chat= becomes a JID as typed;
// the bot resolves it with WhatsApp once logged in.
func ParseWebhookFilter(raw string) (store.Filter, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return store.Filter{}, err
	}
	return store.ParseFilter(q, func(s string) (string, error) {
		jid, err := ParseRecipient(s)
		return jid.String(), err
	})
}

// InitSettings records the flag values and applies the stored overrides on top. Stored values that
// no longer parse are skipped with a warning.
func (b *Bot) InitSettings(base Settings) (Settings, error) {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	base = base.withFileDefaults()
	b.baseSettings = base
	overrides, err := b.Store.Settings()
	if err != nil {
		return base, err
	}
	eff := base
	for _, k := range SettingKeys {
		if o, ok := overrides[k]; ok {
			if err := eff.set(k, o.Value); err != nil {
				b.Log.Warn("ignoring stored setting", "key", k, "err", err)
			}
		}
	}
	b.applySettings(eff)
	return eff, nil
}

func (b *Bot) applySettings(s Settings) {
	b.settings = s
	b.Limiter.Set(s.Rate, s.ChatGap, s.MaxWait)
	b.Lookups.Set(LookupsPerMinute(s.Rate), 0, s.MaxWait)
	b.replies.Store(s.AutoReply)
	b.setWebhookFilter(s.WebhookFilter)
	b.wakeOutbox()
}

// SetAutoReply turns the !ping / !time / !help replies on or off for this process only.
func (b *Bot) SetAutoReply(on bool) { b.replies.Store(on) }

func (b *Bot) AutoReplyOn() bool { return b.replies.Load() }

// SettingsView lists every setting with its effective value, its flag default and who changed it.
func (b *Bot) SettingsView() ([]SettingView, error) {
	overrides, err := b.Store.Settings()
	if err != nil {
		return nil, err
	}
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	out := make([]SettingView, len(SettingKeys))
	for i, k := range SettingKeys {
		v := SettingView{Key: k, Value: b.settings.get(k), Default: b.baseSettings.get(k)}
		if o, ok := overrides[k]; ok {
			v.Overridden, v.UpdatedBy, v.UpdatedAt = true, o.UpdatedBy, o.UpdatedAt
		}
		out[i] = v
	}
	return out, nil
}

// UpdateSettings validates every change first, then stores and applies them together and audits
// the change once. A nil value goes back to the flag's value.
func (b *Bot) UpdateSettings(actor string, changes map[string]*string) error {
	if len(changes) == 0 {
		return BadInput("no settings given")
	}
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	next := b.settings
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var detail []string
	for _, k := range keys {
		v := changes[k]
		value := b.baseSettings.get(k)
		if v != nil {
			value = *v
		}
		if err := next.set(k, value); err != nil {
			b.Audit(actor, "settings", "", strings.Join(keys, ","), err)
			return err
		}
		if v == nil {
			detail = append(detail, k+" reset")
		} else {
			detail = append(detail, fmt.Sprintf("%s=%s", k, next.get(k)))
		}
	}
	err := b.Store.SetSettings(changes, actor)
	b.Audit(actor, "settings", "", strings.Join(detail, ", "), err)
	if err != nil {
		return err
	}
	b.applySettings(next)
	return nil
}
