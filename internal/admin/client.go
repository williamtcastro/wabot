// Package admin is a terminal admin panel for a wame server, talking to it only over the HTTP API.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient checks the server URL. Plain http is only allowed to loopback addresses unless insecure
// is set, because the token travels in every request.
func NewClient(server, token string, insecure bool) (*Client, error) {
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("server must be an http(s) URL, like https://wame.example.com")
	}
	if u.Scheme == "http" && !insecure && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("refusing to send the token over plain http to %s: use https, an SSH tunnel to 127.0.0.1, or -insecure", u.Host)
	}
	if token == "" {
		return nil, errors.New("a token is required: set WAME_TOKEN (in the environment, ./.env or ~/.config/wame/admin.env) to the owner secret or an admin token")
	}
	u.RawQuery, u.Fragment = "", ""
	return &Client{base: u, token: token, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Server is the URL shown in the panel header.
func (c *Client) Server() string { return c.base.String() }

// APIError is a non-2xx answer from the server.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	switch e.Code {
	case http.StatusUnauthorized:
		return "the server rejected the token (401)"
	case http.StatusForbidden:
		return "this token can't do that, it needs the admin scope (403): " + e.Msg
	}
	if e.Msg == "" {
		return http.StatusText(e.Code)
	}
	return e.Msg
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Code: resp.StatusCode, Msg: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", path, err)
	}
	return nil
}

func (c *Client) Status(ctx context.Context) (bot.Status, error) {
	var s bot.Status
	err := c.do(ctx, "GET", "status", nil, nil, &s)
	return s, err
}

func (c *Client) Pair(ctx context.Context, phone string) (string, error) {
	var out struct {
		Code string `json:"code"`
	}
	err := c.do(ctx, "POST", "pair", nil, map[string]string{"phone": phone}, &out)
	return out.Code, err
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, "POST", "logout", nil, struct{}{}, nil)
}

func (c *Client) Tokens(ctx context.Context) ([]store.Token, error) {
	var out []store.Token
	err := c.do(ctx, "GET", "tokens", nil, nil, &out)
	return out, err
}

// TokenRequest is the body of POST /tokens.
type TokenRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Chats     []string `json:"chats,omitempty"`
	Rate      *int     `json:"rate,omitempty"`
	ExpiresIn string   `json:"expires_in,omitempty"`
}

// CreateToken returns the plain token, which the server shows only this once.
func (c *Client) CreateToken(ctx context.Context, r TokenRequest) (string, store.Token, error) {
	var out struct {
		Token   string      `json:"token"`
		Details store.Token `json:"details"`
	}
	err := c.do(ctx, "POST", "tokens", nil, r, &out)
	return out.Token, out.Details, err
}

func (c *Client) RevokeToken(ctx context.Context, name string) (int, error) {
	var out struct {
		Revoked int `json:"revoked"`
	}
	err := c.do(ctx, "DELETE", "tokens/"+url.PathEscape(name), nil, nil, &out)
	return out.Revoked, err
}

// Audit returns up to limit entries, newest first, older than before (0 = from the newest).
func (c *Client) Audit(ctx context.Context, limit int, before int64) ([]store.AuditEntry, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	var out []store.AuditEntry
	err := c.do(ctx, "GET", "audit", q, nil, &out)
	return out, err
}
