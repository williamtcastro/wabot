package tui

import (
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

func formatContacts(cs []bot.Contact, limit int) string {
	var sb strings.Builder
	for i, c := range cs {
		if limit > 0 && i == limit {
			fmt.Fprintf(&sb, "… and %d more\n", len(cs)-limit)
			break
		}
		name := safeLine(c.Name)
		if c.PushName != "" && c.PushName != c.Name {
			name += styleDim.Render("  ~" + safeLine(c.PushName))
		}
		fmt.Fprintf(&sb, "%s  %s\n", styleDim.Render(fmt.Sprintf("%-16s", c.Phone)), name)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func separator(label string, width int) string {
	label = " " + label + " "
	side := max(2, (width-lipgloss.Width(label))/2)
	return styleFaint.Render(strings.Repeat("─", side)) + styleDim.Render(label) + styleFaint.Render(strings.Repeat("─", side))
}

func dayLabel(t time.Time) string {
	t = t.Local()
	now := time.Now()
	y, mo, d := t.Date()
	switch ny, nm, nd := now.Date(); {
	case y == ny && mo == nm && d == nd:
		return "Today"
	case now.Sub(t) < 48*time.Hour && now.AddDate(0, 0, -1).Day() == d:
		return "Yesterday"
	case y == ny:
		return t.Format("Mon, Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

func shortTime(t time.Time) string {
	switch label := dayLabel(t); label {
	case "Today":
		return t.Local().Format("15:04")
	case "Yesterday":
		return "Yesterday"
	}
	if time.Since(t) < 7*24*time.Hour {
		return t.Local().Format("Mon")
	}
	return t.Local().Format("02/01/06")
}

func joinReactions(r map[string]string) string {
	counts := map[string]int{}
	for _, e := range r {
		if e != "" {
			counts[e]++
		}
	}
	var parts []string
	for _, e := range slices.Sorted(maps.Keys(counts)) {
		if counts[e] > 1 {
			parts = append(parts, fmt.Sprintf("%s%d", e, counts[e]))
		} else {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, " ")
}

func colorFor(s string) lipgloss.Color {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return senderColors[h.Sum32()%uint32(len(senderColors))]
}

func userOf(jid string) string { return termui.UserOf(jid) }

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func humanSize(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func isReaction(m store.Message) bool { return m.Type == "reaction" }

func chatIndex(chats []store.Chat, jid string) int {
	return slices.IndexFunc(chats, func(c store.Chat) bool { return c.JID == jid })
}

func replaceMessage(msgs []store.Message, updated store.Message) {
	for i := range msgs {
		if msgs[i].Chat == updated.Chat && msgs[i].ID == updated.ID {
			msgs[i] = updated
		}
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

func shortText(m store.Message) string {
	if m.Deleted {
		return "🚫 deleted"
	}
	t := strings.Join(strings.Fields(m.Text), " ")
	if label, ok := mediaLabels[m.Type]; ok {
		t = strings.TrimSpace(label + " " + t)
	}
	return t
}

var mediaLabels = map[string]string{
	"image": "📷", "video": "🎥", "audio": "🎤 voice message", "document": "📄", "sticker": "💟 sticker",
	"location": "📍", "reaction": "reacted",
}

// safe and safeLine sanitize text that strangers control (messages, names, file names) before it's
// rendered; see termui.Safe.
func safe(s string) string { return termui.Safe(s) }

func safeLine(s string) string { return termui.SafeLine(s) }

func safeMessage(x store.Message) store.Message {
	x.Text, x.PushName = safe(x.Text), safeLine(x.PushName)
	if x.Media != nil {
		media := *x.Media
		media.FileName = safeLine(media.FileName)
		x.Media = &media
	}
	return x
}

func safeAll(msgs []store.Message) []store.Message {
	for i := range msgs {
		msgs[i] = safeMessage(msgs[i])
	}
	return msgs
}
