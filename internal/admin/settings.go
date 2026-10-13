package admin

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/bot"
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
)

var settingLabels = map[string][2]string{
	"rate":           {"Send rate", "messages per minute, 0 = unlimited; number lookups get 3×"},
	"chat_gap":       {"Chat gap", "minimum time between two messages to the same chat, e.g. 1s"},
	"max_wait":       {"Max wait", "how long a send may queue before the API answers 429, e.g. 30s"},
	"auto_reply":     {"Auto-replies", "answer !ping, !time and !help"},
	"webhook_filter": {"Webhook filter", "which messages go to the webhook, e.g. type=text&from_me=false"},
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
	s, ok := m.selectedSetting()
	switch k {
	case "j", "down":
		m.setSel = clamp(m.setSel+1, len(m.settings))
	case "k", "up":
		m.setSel = clamp(m.setSel-1, len(m.settings))
	case "enter", "e", " ":
		if !ok {
			return nil
		}
		if s.Key == "auto_reply" {
			next := "true"
			if s.Value == "true" {
				next = "false"
			}
			return m.changeSetting(s.Key, &next, "auto-replies "+map[string]string{"true": "on", "false": "off"}[next])
		}
		m.editKey = s.Key
		m.startPrompt(modeSetting, settingLabels[s.Key][1])
		m.prompt.SetValue(s.Value)
		m.prompt.CursorEnd()
	case "x", "d":
		if !ok || !s.Overridden {
			return nil
		}
		return m.changeSetting(s.Key, nil, settingLabels[s.Key][0]+" back to the server's flag ("+cmpOr(s.Default, "empty")+")")
	}
	return nil
}

func (m *model) viewSettings() string {
	switch {
	case isNotFound(m.settingsErr):
		return indent(card("Settings", min(m.width-2, 90), styleWarn.Render("This server can't change settings at runtime yet.")+"\n"+
			styleDim.Render("Update wame on the server to get GET/PATCH /settings.")), " ")
	case m.settingsErr != nil:
		return indent(card("Settings", min(m.width-2, 90), styleErr.Render(m.settingsErr.Error())), " ")
	case len(m.settings) == 0:
		return "  " + styleDim.Render("loading…")
	}
	cols := []int{16, 28, 14}
	lines := []string{"   " + styleHead.Render(table(cols, "SETTING", "VALUE", "FLAG", "SOURCE"))}
	for i, s := range m.settings {
		value := cmpOr(s.Value, styleDim.Render("(none)"))
		source := styleDim.Render("flag")
		if s.Overridden {
			value = styleAccent.Render(cmpOr(s.Value, "(none)"))
			source = styleWarn.Render("changed") + styleDim.Render(" by "+s.UpdatedBy+", "+relTime(s.UpdatedAt))
		}
		if s.Key == "webhook_filter" && m.stats.Webhook == nil && !m.statsAt.IsZero() {
			source += styleDim.Render(" · no webhook configured")
		}
		line := table(cols, settingLabels[s.Key][0], value, styleDim.Render(cmpOr(s.Default, "(none)")), source)
		if i == m.setSel {
			lines = append(lines, m.selectedRow(line))
		} else {
			lines = append(lines, "   "+line)
		}
	}
	if s, ok := m.selectedSetting(); ok {
		lines = append(lines, "", indent(card(settingLabels[s.Key][0], min(m.width-2, 100),
			styleDim.Render(settingLabels[s.Key][1])+"\n\n"+
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
		m.setNotice(fmt.Sprintf("%s: the webhook didn't accept it (%s)", msg.what, cmpOr(msg.res.Error, fmt.Sprintf("HTTP %d", msg.res.Status))), true)
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
	switch k {
	case "j", "down":
		m.healthSel = clamp(m.healthSel+1, n)
	case "k", "up":
		m.healthSel = clamp(m.healthSel-1, n)
	case "t":
		if m.stats.Webhook == nil {
			m.setNotice("no webhook configured on this server", true)
			return nil
		}
		return m.testWebhook()
	case "R":
		return m.replayWebhook()
	}
	return nil
}

func isNotFound(err error) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.Code == 404
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
