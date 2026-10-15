package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/termui"
)

type queueFilesMsg struct {
	seq   int
	files QueueFiles
	err   error
}

func (m *model) loadQueueFiles() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq := m.qfilesReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		files, err := c.ListQueueFiles(ctx)
		return queueFilesMsg{seq, files, err}
	})
}

func (m *model) applyQueueFiles(msg queueFilesMsg) {
	if !m.qfilesReq.done(msg.seq) {
		return
	}
	m.qfilesErr = msg.err
	if msg.err != nil {
		return
	}
	selected, ok := m.selectedQueueFile()
	m.qfiles, m.qfilesLoaded = msg.files, true
	vis := m.visibleQueueFiles()
	if i := slices.IndexFunc(vis, func(f QueueFile) bool { return f.ID == selected.ID }); ok && i >= 0 {
		m.qfileSel = i
	}
	m.qfileSel = clamp(m.qfileSel, len(vis))
}

// queueFilesOff reports a server without the route (404) or a token that can't list files (403):
// asking again won't help, so the tick stops polling until r, F or a tab switch.
func (m *model) queueFilesOff() bool {
	return isNotFound(m.qfilesErr) || isForbidden(m.qfilesErr)
}

// pollQueueFiles reloads the files while the Queue tab shows them; idle skips it while a request
// is pending (the tick).
func (m *model) pollQueueFiles(idle bool) tea.Cmd {
	if m.tab != tabQueue || !m.queueFiles || m.queueFilesOff() || (idle && m.qfilesReq.busy()) {
		return nil
	}
	return m.loadQueueFiles()
}

// queueFilesRefusal is why the list can't be acted on, or "" when it can.
func (m *model) queueFilesRefusal() string {
	switch {
	case m.qfilesErr == nil && !m.qfilesLoaded:
		return "the files are still loading"
	case m.qfilesErr == nil:
		return ""
	case isNotFound(m.qfilesErr):
		return "this server doesn't have queue files yet: update wame on it"
	case isForbidden(m.qfilesErr):
		return "this token can't manage queue files: the list is read-only"
	}
	return "the files didn't load: " + m.qfilesErr.Error()
}

// shortFileID is the id without most of its hex: enough to tell files apart on screen.
func shortFileID(id string) string { return safe(id[:min(len(id), 10)]) }

func (m *model) visibleQueueFiles() []QueueFile {
	return filtered(m.qfiles.Files, m.qfileQuery, func(f QueueFile) string {
		return strings.ToLower(strings.Join([]string{f.ID, f.Kind, f.Actor}, " "))
	})
}

func (m *model) selectedQueueFile() (QueueFile, bool) {
	if vis := m.visibleQueueFiles(); m.qfileSel < len(vis) {
		return vis[m.qfileSel], true
	}
	return QueueFile{}, false
}

func (m *model) queueFilesKey(k string) tea.Cmd {
	n := len(m.visibleQueueFiles())
	if sel, ok := termui.ListMove(k, m.qfileSel, n, m.bodyHeight()/2); ok {
		m.qfileSel = sel
		return nil
	}
	switch k {
	case "F":
		m.queueFiles = false
	case "esc":
		if m.qfileQuery == "" {
			m.queueFiles = false
			return nil
		}
		m.listFilterKey(k, "")
	case "/":
		m.listFilterKey(k, "filter by id, kind or actor")
	case "d":
		if why := m.queueFilesRefusal(); why != "" {
			m.setNotice(why, true)
			return nil
		}
		f, ok := m.selectedQueueFile()
		if !ok {
			m.setNotice("no file marked", true)
			return nil
		}
		m.confirmQFile, m.confirmQFileOpen = f.ID, f.Open
		m.mode = modeConfirmDeleteQueueFile
	}
	return nil
}

func (m *model) deleteQueueFile(id string, open int) tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		err := c.DeleteQueueFile(ctx, id)
		if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Code == http.StatusConflict {
			used := "queued messages"
			if open > 0 {
				used = plural(open, "queued message")
			}
			return doneMsg{err: fmt.Errorf("not deleted: still used by %s; cancel them first", used)}
		}
		if isNotFound(err) {
			return doneMsg{err: errors.New("not deleted: the file is gone already, or this server is older: update wame on it")}
		}
		if err != nil {
			return doneMsg{err: fmt.Errorf("not deleted: %w", err)}
		}
		return doneMsg{notice: "deleted file " + shortFileID(id)}
	})
}

// usedBy is what a file's use column shows: items that used it, and how many still wait.
func usedBy(f QueueFile) string {
	if f.Items == 0 {
		return styleDim.Render("unused")
	}
	return fmt.Sprintf("%d, %d open", f.Items, f.Open)
}

// viewQueueFiles lists the files by id, kind and size, never by name: file names don't reach
// the admin panel.
func (m *model) viewQueueFiles(lines []string) string {
	if m.qfilesErr != nil {
		return strings.Join(append(lines, m.errCard("Queue files", m.qfilesErr, "queue files (GET /queue/files)")), "\n")
	}
	vis := m.visibleQueueFiles()
	head := fmt.Sprintf("files · %d of %d · %s, %s in use", len(vis), len(m.qfiles.Files),
		plural(m.qfiles.Usage.Files, "file"), byteSize(m.qfiles.Usage.Bytes))
	if m.qfileQuery != "" {
		head += fmt.Sprintf(" · filtered by %q (esc clears)", safe(m.qfileQuery))
	}
	lines = append(lines, "   "+styleDim.Render(head))
	cols := []int{11, 9, 8, 19, 16, 12, 12}
	lines = append(lines, "   "+styleHead.Render(table(cols, "ID", "KIND", "SIZE", "UPLOADED", "EXPIRES", "USED", "BY")))
	if len(vis) == 0 {
		msg := "no files uploaded"
		if m.qfileQuery != "" {
			msg = "nothing matches"
		}
		return strings.Join(append(lines, "   "+styleDim.Render(msg)), "\n")
	}
	start, end := window(m.qfileSel, len(vis), m.listRows(lines))
	for i := start; i < end; i++ {
		f := vis[i]
		expires := styleDim.Render("with its queue")
		if !f.Expires.IsZero() {
			expires = relTime(f.Expires)
		}
		line := table(cols, shortFileID(f.ID), safe(f.Kind), byteSize(f.Size), when(f.Created)+" "+styleDim.Render(relTime(f.Created)),
			expires, usedBy(f), safe(f.Actor))
		lines = append(lines, m.row(line, i == m.qfileSel))
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewConfirmDeleteQueueFile() string {
	what := shortFileID(m.confirmQFile)
	if f, ok := m.queueFileByID(m.confirmQFile); ok {
		what += " (" + safe(f.Kind) + ", " + byteSize(f.Size) + ")"
	}
	return styleBold.Render("Delete file "+what+"?") + "\n\n" +
		styleDim.Render("Queued messages still using it block the delete; cancel them first.") + "\n\n" +
		styleKey.Render("y") + styleDim.Render(" delete   any other key: keep it")
}

func (m *model) queueFileByID(id string) (QueueFile, bool) {
	i := slices.IndexFunc(m.qfiles.Files, func(f QueueFile) bool { return f.ID == id })
	if i < 0 {
		return QueueFile{}, false
	}
	return m.qfiles.Files[i], true
}
