package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/williamtcastro/wame/internal/bot"
)

// LogPage is the body of GET /logs.
type LogPage struct {
	Entries []bot.LogEntry `json:"entries"`
}

// logs serves the server's recent app logs, newest first. ?since= tails (only newer entries), ?before=
// pages back, ?level= is the lowest level shown.
func (a *server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := bot.LogQuery{Level: slog.LevelDebug, Limit: intParam(r, "limit", 200, maxPageSize)}
	if v := q.Get("level"); v != "" {
		if err := lq.Level.UnmarshalText([]byte(v)); err != nil {
			writeError(w, bot.BadInput("level must be debug, info, warn or error"))
			return
		}
	}
	var err error
	if lq.Since, err = logID(q.Get("since")); err != nil {
		writeError(w, bot.BadInput("since must be a log entry id"))
		return
	}
	if lq.Before, err = logID(q.Get("before")); err != nil {
		writeError(w, bot.BadInput("before must be a log entry id"))
		return
	}
	page := LogPage{Entries: []bot.LogEntry{}}
	if a.b.Logs != nil {
		page.Entries = a.b.Logs.Entries(lq)
	}
	writeJSON(w, 200, page)
}

func logID(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 63)
	return int64(n), err
}
