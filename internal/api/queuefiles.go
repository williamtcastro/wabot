package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

// queueFile is a file as /queue/files shows it: never its content, and other actors' names only
// to nobody (bot.QueueFiles drops them).
type queueFile struct {
	store.OutboxFile
	Expires time.Time `json:"expires_at,omitzero"`
}

func showQueueFile(f store.OutboxFile) queueFile {
	return queueFile{OutboxFile: f, Expires: bot.QueueFileExpires(f)}
}

// uploadQueueFile takes a multipart body with one "file" part, streamed to disk: 201 for a new
// file, 200 when the caller had already uploaded the same bytes.
func (a *server) uploadQueueFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, bot.BadInput("send the file as multipart/form-data in a field named 'file'"))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeError(w, bot.BadInput("multipart field 'file' is required"))
			return
		}
		if tooLarge(w, err) {
			return
		}
		if err != nil {
			writeError(w, bot.BadInput("invalid multipart form: %v", err))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		p := principal(r)
		f, existed, err := a.b.AddQueueFile(p.Name, p.TokenID, part.FileName(), part, maxUpload)
		if err != nil {
			writeQueueFileError(w, err)
			return
		}
		code := http.StatusCreated
		if existed {
			code = http.StatusOK
		}
		writeJSON(w, code, showQueueFile(f))
		return
	}
}

// writeQueueFileError adds the upload's own codes to writeError: 413 over the size or a quota,
// 415 for a type off the allow-list, 507 when the server couldn't write it, 409 for a delete of
// a file queued messages need.
func writeQueueFileError(w http.ResponseWriter, err error) {
	_, tooBig := errors.AsType[bot.FileTooLargeError](err)
	_, quota := errors.AsType[bot.FileQuotaError](err)
	_, badType := errors.AsType[bot.FileTypeError](err)
	_, inUse := errors.AsType[bot.FileInUseError](err)
	switch {
	case tooBig || quota:
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case badType:
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
	case inUse:
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, bot.ErrFileStorage):
		writeErr(w, http.StatusInsufficientStorage, err.Error())
	default:
		if !tooLarge(w, err) {
			writeError(w, err)
		}
	}
}

// listQueueFiles shows the caller's live files with its usage; admin sees everyone's (others'
// without names) and the total.
func (a *server) listQueueFiles(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	admin := p.Has(auth.Admin)
	files, err := a.b.QueueFiles(p.Name, admin)
	if err != nil {
		writeError(w, err)
		return
	}
	who := p.Name
	if admin {
		who = ""
	}
	usage, err := a.b.Store.OutboxFileUsage(who)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]queueFile, len(files))
	for i, f := range files {
		out[i] = showQueueFile(f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out, "usage": usage})
}

func (a *server) deleteQueueFile(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := a.b.DeleteQueueFile(p.Name, r.PathValue("id"), p.Has(auth.Admin)); err != nil {
		writeQueueFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
