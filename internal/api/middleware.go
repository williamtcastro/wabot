package api

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// localOnly protects a token-less API from web pages the owner visits: it rejects cross-site
// requests (CSRF) and any Host that isn't loopback (DNS rebinding).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		crossSite := r.Header.Get("Sec-Fetch-Site") == "cross-site"
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			crossSite = crossSite || err != nil || u.Host != r.Host
		}
		remote := net.ParseIP(clientIP(r))
		if !loopback || crossSite || remote == nil || !remote.IsLoopback() {
			writeErr(w, http.StatusForbidden, "without -token only same-origin requests to localhost are allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// quietPaths are polled (long-polls, streams, the admin panel's refreshes, Prometheus scrapes), so a
// successful GET of one logs at debug and doesn't push everything else out of the /logs buffer.
var quietPaths = map[string]bool{
	"/status": true, "/messages/wait": true, "/events": true, "/logs": true, "/stats": true, "/metrics": true,
	"/queue": true, "/tokens": true, "/settings": true, "/audit": true,
}

// withRequestLog logs one line per request. The query string is left out because it may hold the token.
func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.Method == http.MethodGet && quietPaths[r.URL.Path]:
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}
