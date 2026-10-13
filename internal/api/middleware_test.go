package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLogLevels(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := withRequestLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	for _, c := range []struct{ method, target, level string }{
		{"GET", "/status", "DEBUG"},
		{"GET", "/stats", "DEBUG"},
		{"GET", "/metrics", "DEBUG"},
		{"GET", "/queue?status=queued", "DEBUG"},
		{"GET", "/tokens", "DEBUG"},
		{"GET", "/settings", "DEBUG"},
		{"GET", "/audit", "DEBUG"},
		{"GET", "/optouts", "DEBUG"},
		{"POST", "/optouts", "INFO"},
		{"GET", "/stats?fail=1", "WARN"},
		{"POST", "/queue", "INFO"},
		{"PATCH", "/settings", "INFO"},
		{"GET", "/queue/7", "INFO"},
		{"GET", "/messages", "INFO"},
	} {
		buf.Reset()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.target, nil))
		if !strings.Contains(buf.String(), "level="+c.level+" ") {
			t.Errorf("%s %s: %s", c.method, c.target, buf.String())
		}
	}
}
