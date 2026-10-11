package main

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"
)

const maxUpload = 64 << 20

//go:embed docs/openapi.yaml
var openapiSpec []byte

const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>wabot API</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
</head><body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "openapi.yaml", dom_id: "#ui", persistAuthorization: true});</script>
</body></html>`

func NewAPI(b *Bot, token string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, b.Status())
	})

	mux.HandleFunc("GET /qr", func(w http.ResponseWriter, r *http.Request) {
		s := b.Status()
		if s.LoggedIn || s.QR == "" {
			writeErr(w, 404, fmt.Errorf("no QR available (state: %s)", s.State))
			return
		}
		code, err := qr.Encode(s.QR, qr.M)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		code.Scale = 8
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(code.PNG())
	})

	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Phone string `json:"phone"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		code, err := b.PairPhone(r.Context(), req.Phone)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]string{"code": code})
	})

	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.To == "" || req.Text == "" {
			writeErr(w, 400, fmt.Errorf("'to' and 'text' are required"))
			return
		}
		m, err := b.SendText(r.Context(), req.To, req.Text)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, m)
	})

	mux.HandleFunc("POST /send/file", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			writeErr(w, 400, err)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeErr(w, 400, fmt.Errorf("multipart field 'file' is required: %w", err))
			return
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxUpload))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		m, err := b.SendFile(r.Context(), r.FormValue("to"), data, hdr.Filename, r.FormValue("caption"))
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, m)
	})

	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		chat := r.URL.Query().Get("chat")
		if chat != "" && !strings.Contains(chat, "@") {
			if jid, err := ParseRecipient(chat); err == nil {
				chat = jid.String()
			}
		}
		writeJSON(w, 200, b.Messages(chat, limit))
	})

	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, 500, fmt.Errorf("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		ch, unsub := b.Subscribe()
		defer unsub()
		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
			case v := <-ch:
				var name string
				switch v.(type) {
				case Message:
					name = "message"
				case Status:
					name = "status"
				default:
					continue
				}
				data, _ := json.Marshal(v)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			}
			flusher.Flush()
		}
	})

	mux.HandleFunc("POST /check", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Phones []string `json:"phones"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		phones := make([]string, len(req.Phones))
		for i, p := range req.Phones {
			phones[i] = "+" + digits(p)
		}
		res, err := b.Client.IsOnWhatsApp(r.Context(), phones)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		type item struct {
			Query string `json:"query"`
			IsIn  bool   `json:"on_whatsapp"`
			JID   string `json:"jid"`
		}
		out := make([]item, len(res))
		for i, x := range res {
			out[i] = item{x.Query, x.IsIn, x.JID.String()}
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("GET /chats", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		chats, err := b.Chats(r.Context(), limit)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, chats)
	})

	mux.HandleFunc("GET /contacts", func(w http.ResponseWriter, r *http.Request) {
		contacts, err := b.Contacts(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, contacts)
	})

	mux.HandleFunc("GET /groups", func(w http.ResponseWriter, r *http.Request) {
		groups, err := b.Client.GetJoinedGroups(r.Context())
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		type item struct {
			JID     string `json:"jid"`
			Name    string `json:"name"`
			Members int    `json:"members"`
		}
		out := make([]item, len(groups))
		for i, g := range groups {
			out[i] = item{g.JID.String(), g.Name, len(g.Participants)}
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if err := b.Client.Logout(r.Context()); err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	root := http.NewServeMux()
	root.Handle("/", withAuth(token, mux))
	root.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, docsPage)
	})
	root.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	return root
}

func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if r.URL.Query().Get("token") == token {
			got = want
		}
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, 401, fmt.Errorf("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, 400, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func postJSON(url string, v any) {
	data, _ := json.Marshal(v)
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Post(url, "application/json", bytes.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}
