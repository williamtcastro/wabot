package admin

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/termui"
)

type (
	settingsMsg struct {
		settings []bot.SettingView
		notice   string
		err      error
	}
	webhookMsg struct {
		what string
		res  bot.WebhookResult
		err  error
	}
	alertMsg struct{ err error }
)

var settingLabels = map[string][2]string{
	"rate":              {"Send rate", "messages per minute, 0 = unlimited; number lookups get 3×"},
	"chat_gap":          {"Chat gap", "minimum time between two messages to the same chat, e.g. 1s"},
	"max_wait":          {"Max wait", "how long a send may queue before the API answers 429, e.g. 30s"},
	"auto_reply":        {"Auto-replies", "answer !ping, !time and !help"},
	"webhook_filter":    {"Webhook filter", "which messages go to the webhook, e.g. type=text&from_me=false"},
	"queue_paused":      {"Queue paused", "hold queued messages; sending resumes where it stopped"},
	"queue_jitter":      {"Queue jitter", "random extra wait, 0 up to this, between queued sends, e.g. 2s"},
	"queue_quiet_hours": {"Quiet hours", "a window when queued messages wait, e.g. 21:00-08:00; empty removes it"},
	"queue_quiet":       {"Quiet hours on", "switch the quiet hours off and on again without losing the window"},
	"queue_timezone":    {"Queue time zone", "the zone quiet hours are in, e.g. America/Sao_Paulo"},
	"queue_media_ttl":   {"Queue file reuse", "how long one WhatsApp upload of a queued file serves later messages, e.g. 24h; off uploads per message"},
	"queue_files_quota": {"Queue files quota", "disk space all queued files may use together, e.g. 2GB"},
}

// settingLabel is a setting's name and description; a key added by a newer server shows as itself.
func settingLabel(key string) (label, desc string) {
	if l, ok := settingLabels[key]; ok {
		return l[0], l[1]
	}
	return key, ""
}

func (m *model) loadSettings() tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		s, err := c.Settings(ctx)
		return settingsMsg{settings: s, err: err}
	})
}

func (m *model) changeSetting(key string, value *string, notice string) tea.Cmd {
	m.setNotice("saving…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		s, err := c.UpdateSettings(ctx, map[string]*string{key: value})
		return settingsMsg{settings: s, notice: notice, err: err}
	})
}

func (m *model) applySettingsMsg(msg settingsMsg) tea.Cmd {
	if msg.err != nil {
		if msg.notice != "" {
			m.setNotice("not saved: "+msg.err.Error(), true)
		} else {
			m.settingsErr = msg.err
		}
		return nil
	}
	m.settings, m.settingsErr = msg.settings, nil
	m.setSel = clamp(m.setSel, len(m.settings))
	if msg.notice != "" {
		m.setNotice(msg.notice, false)
		return tea.Batch(m.loadStats(), m.loadAudit(false))
	}
	return nil
}

func (m *model) selectedSetting() (bot.SettingView, bool) {
	if m.setSel < len(m.settings) {
		return m.settings[m.setSel], true
	}
	return bot.SettingView{}, false
}

func (m *model) settingsKey(k string) tea.Cmd {
	if sel, ok := termui.ListMove(k, m.setSel, len(m.settings), m.bodyHeight()/2); ok {
		m.setSel = sel
		return nil
	}
	s, ok := m.selectedSetting()
	label, desc := settingLabel(s.Key)
	switch k {
	case "enter", "e", " ":
		if !ok {
			return nil
		}
		if s.Value == "true" || s.Value == "false" {
			next := strconv.FormatBool(s.Value != "true")
			return m.changeSetting(s.Key, &next, label+": "+map[string]string{"true": "on", "false": "off"}[next])
		}
		m.editKey = s.Key
		m.startPrompt(modeSetting, desc)
		m.prompt.SetValue(s.Value)
		m.prompt.CursorEnd()
	case "x", "d":
		if !ok || !s.Overridden {
			return nil
		}
		return m.changeSetting(s.Key, nil, label+" back to the server's flag ("+cmp.Or(s.Default, "empty")+")")
	}
	return nil
}

func (m *model) viewSettings() string {
	switch {
	case m.settingsErr != nil:
		return m.errCard("Settings", m.settingsErr, "GET/PATCH /settings")
	case len(m.settings) == 0:
		return "  " + styleDim.Render("loading…")
	}
	cols := []int{16, 28, 14}
	lines := []string{"   " + styleHead.Render(table(cols, "SETTING", "VALUE", "FLAG", "SOURCE"))}
	for i, s := range m.settings {
		value := cmp.Or(safe(s.Value), styleDim.Render("(none)"))
		source := styleDim.Render("flag")
		if s.Overridden {
			value = styleAccent.Render(cmp.Or(safe(s.Value), "(none)"))
			source = styleWarn.Render("changed") + styleDim.Render(" by "+safe(s.UpdatedBy)+", "+relTime(s.UpdatedAt))
		}
		if s.Key == "webhook_filter" && m.stats.Webhook == nil && !m.statsAt.IsZero() {
			source += styleDim.Render(" · no webhook configured")
		}
		label, _ := settingLabel(s.Key)
		line := table(cols, safe(label), value, styleDim.Render(cmp.Or(safe(s.Default), "(none)")), source)
		lines = append(lines, m.row(line, i == m.setSel))
	}
	if s, ok := m.selectedSetting(); ok {
		label, desc := settingLabel(s.Key)
		lines = append(lines, "", indent(card(safe(label), m.cardWidth(cardWide),
			styleDim.Render(cmp.Or(desc, "Added by a newer server than this panel."))+"\n\n"+
				styleDim.Render("Changes apply at once and are saved on the server, so they also win over its\nflags after a restart, until reset with x. Every change is in the audit log.")), " "))
	}
	return strings.Join(lines, "\n")
}

func (m *model) testWebhook() tea.Cmd {
	m.setNotice("sending a test delivery…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		r, err := c.WebhookTest(ctx)
		return webhookMsg{"test", r, err}
	})
}

func (m *model) replayWebhook() tea.Cmd {
	w := m.stats.Webhook
	if w == nil || m.healthSel >= len(w.Recent) {
		m.setNotice("pick a failed delivery first (j/k)", true)
		return nil
	}
	f := w.Recent[m.healthSel]
	if f.Replayed {
		m.setNotice(f.Delivery+" was already replayed", true)
		return nil
	}
	m.setNotice("replaying "+f.Delivery+"…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		r, err := c.WebhookReplay(ctx, f.Delivery)
		return webhookMsg{"replay of " + f.Delivery, r, err}
	})
}

func (m *model) applyWebhookMsg(msg webhookMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		m.setNotice(msg.what+" failed: "+msg.err.Error(), true)
	case !msg.res.OK:
		m.setNotice(fmt.Sprintf("%s: the webhook didn't accept it (%s)", msg.what, cmp.Or(msg.res.Error, fmt.Sprintf("HTTP %d", msg.res.Status))), true)
	default:
		m.setNotice(fmt.Sprintf("%s: HTTP %d in %d ms", msg.what, msg.res.Status, msg.res.DurationMS), false)
	}
	return tea.Batch(m.loadStats(), m.loadAudit(false))
}

func (m *model) healthKey(k string) tea.Cmd {
	n := 0
	if m.stats.Webhook != nil {
		n = len(m.stats.Webhook.Recent)
	}
	if sel, ok := termui.ListMove(k, m.healthSel, n, m.bodyHeight()/2); ok {
		m.healthSel = sel
		return nil
	}
	switch k {
	case "t":
		if m.stats.Webhook == nil {
			m.setNotice("no webhook configured on this server", true)
			return nil
		}
		return m.testWebhook()
	case "R":
		return m.replayWebhook()
	case "a":
		m.setNotice("sending a test alert…", false)
		return m.call(func(ctx context.Context, c *Client) tea.Msg {
			return alertMsg{c.AlertTest(ctx)}
		})
	}
	return nil
}

func (m *model) applyAlertMsg(msg alertMsg) tea.Cmd {
	switch {
	case isNotFound(msg.err):
		m.setNotice("this server can't send test alerts yet: update wame on it", true)
		return nil
	case msg.err != nil:
		m.setNotice("test alert failed: "+msg.err.Error(), true)
	default:
		m.setNotice("test alert sent", false)
	}
	return tea.Batch(m.loadStats(), m.loadAudit(false))
}

// callerUsage finds a caller's 24-hour usage in the last stats, by token name.
func (m *model) callerUsage(name string) (calls, sends, failed int64, streams int, ok bool) {
	for _, c := range m.stats.Callers {
		if c.Name == name {
			return c.Requests, c.Sends, c.Failed, c.Streams, true
		}
	}
	return 0, 0, 0, 0, false
}

func usageCell(n int64, warnAt int64) string {
	switch {
	case n == 0:
		return styleDim.Render("0")
	case warnAt > 0 && n >= warnAt:
		return styleWarn.Render(fmt.Sprint(n))
	}
	return fmt.Sprint(n)
}
