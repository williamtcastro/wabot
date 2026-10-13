package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"modernc.org/sqlite"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

var (
	errInternal = errors.New("internal error")
	errTrailing = errors.New("unexpected data after the JSON value")
)

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONMax(w, r, v, 1<<20)
}

// readJSONMax decodes exactly one JSON value of at most limit bytes; a larger body gets 413.
func readJSONMax(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	err := dec.Decode(v)
	if err == nil {
		if _, extra := dec.Token(); !errors.Is(extra, io.EOF) {
			err = errTrailing
			if _, ok := errors.AsType[*http.MaxBytesError](extra); ok {
				err = extra
			}
		}
	}
	if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body is larger than %d bytes", tooBig.Limit))
		return false
	}
	if err != nil {
		writeError(w, bot.BadInput("invalid JSON body: %v", err))
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeError maps errors to status codes: bad input 400, missing 404, key reuse or a
// cancel of an item no longer queued 409, rate limit 429,
// database errors and panics 500 (logged, never shown), anything else (WhatsApp) 502.
func writeError(w http.ResponseWriter, err error) {
	code, msg := http.StatusBadGateway, err.Error()
	_, db := errors.AsType[*sqlite.Error](err)
	_, input := errors.AsType[bot.InputError](err)
	rl, limited := errors.AsType[bot.RateLimitError](err)
	_, notQueued := errors.AsType[bot.NotQueuedError](err)
	switch {
	case db || errors.Is(err, errInternal):
		code, msg = http.StatusInternalServerError, errInternal.Error()
		if db {
			slog.Error("api: database error", "err", err)
		}
	case input:
		code = http.StatusBadRequest
	case notQueued:
		code = http.StatusConflict
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errConflict):
		code = http.StatusConflict
	case limited:
		code = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())+1))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	writeErr(w, code, msg)
}
