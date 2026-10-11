package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wabot/internal/api"
	"github.com/williamtcastro/wabot/internal/bot"
	"github.com/williamtcastro/wabot/internal/store"
	"github.com/williamtcastro/wabot/internal/tui"
)

const usage = `wabot - a tiny WhatsApp bot built on whatsmeow

Usage:
  wabot login [-phone NUMBER]          link this bot to your WhatsApp (QR or pairing code)
  wabot serve [-addr ADDR]             run the HTTP API
  wabot tui   [-addr ADDR]             interactive terminal UI (optionally also serves the API)
  wabot send  NUMBER|JID TEXT...       send one message and exit
  wabot logout                         unlink the device and delete the session

Flags (right after the command, before other arguments):
  -db PATH               session database (env WABOT_DB, default ~/.config/wabot/wabot.db)
  -addr ADDR             HTTP listen address (env WABOT_ADDR; serve defaults to 127.0.0.1:8080)
  -token TOKEN           require "Authorization: Bearer TOKEN" on the API (env WABOT_TOKEN)
  -webhook URL           POST incoming messages as JSON to URL (env WABOT_WEBHOOK)
  -webhook-secret S      sign webhook bodies with HMAC-SHA256 (env WABOT_WEBHOOK_SECRET)
  -webhook-filter Q      only deliver matching messages, e.g. "type=text&from_me=false" (env WABOT_WEBHOOK_FILTER)
  -rate N                max messages sent per minute, 0 = unlimited (env WABOT_RATE, default 20)
  -chat-gap D            min time between messages to the same chat (default 1s)
  -max-wait D            queue a send at most this long before answering 429 (default 30s)
  -typing                show "typing…" before each text message
  -online                appear "online" while connected (off: your phone keeps getting notifications)
  -autoreply             answer !ping, !time and !help (default true)
  -log-format F          text or json (env WABOT_LOG_FORMAT, default text)
  -log-file PATH         write app logs to PATH instead of stderr (env WABOT_LOG_FILE)
  -debug                 verbose logs, including whatsmeow's

Docs: README.md, docs/*.md, or http://localhost:8080/docs while serving`

type config struct {
	db, addr, token, phone                string
	webhook, webhookSecret, webhookFilter string
	logFormat, logFile                    string
	rate                                  int
	chatGap, maxWait                      time.Duration
	autoreply, typing, online, debug      bool
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Println(usage)
		return
	}

	var c config
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Println(usage) }
	fs.StringVar(&c.db, "db", envOr("WABOT_DB", defaultDB()), "")
	fs.StringVar(&c.addr, "addr", envOr("WABOT_ADDR", ""), "")
	fs.StringVar(&c.token, "token", os.Getenv("WABOT_TOKEN"), "")
	fs.StringVar(&c.webhook, "webhook", os.Getenv("WABOT_WEBHOOK"), "")
	fs.StringVar(&c.webhookSecret, "webhook-secret", os.Getenv("WABOT_WEBHOOK_SECRET"), "")
	fs.StringVar(&c.webhookFilter, "webhook-filter", os.Getenv("WABOT_WEBHOOK_FILTER"), "")
	fs.StringVar(&c.logFormat, "log-format", envOr("WABOT_LOG_FORMAT", "text"), "")
	fs.StringVar(&c.logFile, "log-file", os.Getenv("WABOT_LOG_FILE"), "")
	fs.StringVar(&c.phone, "phone", "", "")
	rate, err := strconv.Atoi(envOr("WABOT_RATE", "20"))
	if err != nil || rate < 0 {
		fmt.Fprintln(os.Stderr, "error: WABOT_RATE must be a non-negative number")
		os.Exit(2)
	}
	fs.IntVar(&c.rate, "rate", rate, "")
	fs.DurationVar(&c.chatGap, "chat-gap", time.Second, "")
	fs.DurationVar(&c.maxWait, "max-wait", 30*time.Second, "")
	fs.BoolVar(&c.autoreply, "autoreply", true, "")
	fs.BoolVar(&c.typing, "typing", false, "")
	fs.BoolVar(&c.online, "online", false, "")
	fs.BoolVar(&c.debug, "debug", false, "")
	_ = fs.Parse(args)
	if c.rate < 0 {
		fmt.Fprintln(os.Stderr, "error: -rate must be 0 or more")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cmd, fs.Args(), c); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string, c config) error {
	level := "INFO"
	if c.debug {
		level = "DEBUG"
	}
	quiet := cmd == "tui" || cmd == "send"
	var waLogger waLog.Logger = waLog.Stdout("whatsmeow", level, true)
	if quiet {
		waLogger = waLog.Noop
	}
	log, closeLog, err := newLogger(c.logFormat, c.logFile, level, quiet)
	if err != nil {
		return err
	}
	defer closeLog.Close()
	if dir := filepath.Dir(c.db); dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if c.db == defaultDB() {
		legacyDBNotice(c.db)
	}

	b, err := bot.New(ctx, c.db, waLogger, log)
	if err != nil {
		return err
	}
	b.AutoReply = c.autoreply && cmd != "send"
	b.Typing = c.typing
	b.Online = c.online
	b.Limiter = bot.NewLimiter(c.rate, c.chatGap, c.maxWait)
	b.Lookups = bot.NewLimiter(lookupsPerMinute(c.rate), 0, c.maxWait)
	if c.webhook != "" {
		if u, err := url.Parse(c.webhook); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("-webhook must be an http(s) URL")
		}
		q, err := url.ParseQuery(c.webhookFilter)
		if err != nil {
			return fmt.Errorf("-webhook-filter: %w", err)
		}
		f, err := store.ParseFilter(q, func(s string) (string, error) {
			jid, err := bot.ParseRecipient(s)
			return jid.String(), err
		})
		if err != nil {
			return fmt.Errorf("-webhook-filter: %w", err)
		}
		b.Webhook = bot.NewWebhook(ctx, c.webhook, c.webhookSecret, f, log)
		if chat := q.Get("chat"); chat != "" && !strings.Contains(chat, "@") {
			go resolveWebhookChat(ctx, b, chat)
		}
	}
	defer b.Client.Disconnect()

	switch cmd {
	case "login":
		return login(ctx, b, c.phone)

	case "serve":
		if c.addr == "" {
			c.addr = "127.0.0.1:8080"
		}
		if err := b.Start(ctx); err != nil {
			return err
		}
		go printQRUpdates(ctx, b)
		return serve(ctx, b, c)

	case "tui":
		if c.addr != "" && c.token == "" && !isLoopbackAddr(c.addr) {
			return fmt.Errorf("refusing to serve on %s without -token (set WABOT_TOKEN, or bind to 127.0.0.1)", c.addr)
		}
		if err := b.Start(ctx); err != nil {
			return err
		}
		if c.addr != "" {
			go func() {
				if err := serve(ctx, b, c); err != nil {
					b.Log.Error("HTTP API stopped", "err", err)
				}
			}()
		}
		return tui.Run(b)

	case "send":
		if len(args) < 2 {
			return errors.New("usage: wabot send NUMBER|JID TEXT")
		}
		if b.Client.Store.ID == nil {
			return errors.New("not logged in, run 'wabot login' first")
		}
		if err := b.Start(ctx); err != nil {
			return err
		}
		if err := waitLoggedIn(ctx, b, 20*time.Second); err != nil {
			return err
		}
		m, err := b.SendText(ctx, "cli", args[0], strings.Join(args[1:], " "), bot.SendOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("sent %s to %s\n", m.ID, m.Chat)
		return nil

	case "logout":
		if b.Client.Store.ID == nil {
			return errors.New("not logged in")
		}
		if err := b.Start(ctx); err != nil {
			return err
		}
		_ = waitLoggedIn(ctx, b, 20*time.Second)
		err := b.Client.Logout(ctx)
		b.Audit("cli", "logout", "", "", err)
		if err != nil {
			return err
		}
		fmt.Println("logged out")
		return nil
	}
	return fmt.Errorf("unknown command %q (try 'wabot help')", cmd)
}

func login(ctx context.Context, b *bot.Bot, phone string) error {
	if b.Client.Store.ID != nil {
		fmt.Println("already logged in as", b.Client.Store.ID.ToNonAD())
		return nil
	}
	if err := b.Start(ctx); err != nil {
		return err
	}
	if phone != "" {
		deadline := time.Now().Add(30 * time.Second)
		for b.Status().QR == "" {
			if time.Now().After(deadline) {
				return fmt.Errorf("WhatsApp didn't start the login (state: %s)", b.Status().State)
			}
			if err := bot.SleepUntil(ctx, time.Now().Add(200*time.Millisecond)); err != nil {
				return err
			}
		}
		code, err := b.PairPhone(ctx, "cli", phone)
		if err != nil {
			return err
		}
		fmt.Println("Pairing code:", code)
		fmt.Println("On your phone: WhatsApp → Linked devices → Link a device → Link with phone number instead")
	} else {
		go printQRUpdates(ctx, b)
	}
	if err := waitLoggedIn(ctx, b, 5*time.Minute); err != nil {
		return err
	}
	fmt.Println("✅ logged in as", b.Status().JID)
	fmt.Println("finishing initial sync…")
	select {
	case <-ctx.Done():
	case <-time.After(8 * time.Second):
	}
	return nil
}

func printQRUpdates(ctx context.Context, b *bot.Bot) {
	ch, unsub := b.Subscribe()
	defer unsub()
	last := ""
	show := func(s bot.Status) {
		if s.QR != "" && s.QR != last {
			last = s.QR
			fmt.Println("\nScan this QR with WhatsApp → Linked devices → Link a device:")
			qrterminal.GenerateHalfBlock(s.QR, qrterminal.L, os.Stdout)
		}
	}
	show(b.Status())
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-ch:
			if s, ok := v.(bot.Status); ok {
				show(s)
			}
		}
	}
}

func waitLoggedIn(ctx context.Context, b *bot.Bot, timeout time.Duration) error {
	deadline := time.After(timeout)
	for !b.Client.IsLoggedIn() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("timed out waiting for WhatsApp login")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

func serve(ctx context.Context, b *bot.Bot, c config) error {
	if c.token == "" && !isLoopbackAddr(c.addr) {
		return fmt.Errorf("refusing to serve on %s without -token: anyone on the network could send messages as you (set WABOT_TOKEN, or bind to 127.0.0.1)", c.addr)
	}
	srv := &http.Server{
		Addr:              c.addr,
		Handler:           api.NewHandler(b, c.token),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if c.token == "" {
		b.Log.Warn("API has no token: only local, same-origin requests are accepted", "addr", c.addr)
	}
	b.Log.Info("HTTP API listening", "addr", c.addr, "docs", "/docs")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// lookupsPerMinute caps number checks and presence updates at 3× the send rate.
func lookupsPerMinute(rate int) int { return rate * 3 }

// resolveWebhookChat swaps a phone number in -webhook-filter for the JID WhatsApp uses
// (e.g. with or without Brazil's 9th digit) once the bot is connected.
func resolveWebhookChat(ctx context.Context, b *bot.Bot, number string) {
	if waitLoggedIn(ctx, b, 24*time.Hour) != nil {
		return
	}
	var jid types.JID
	var err error
	for wait := 10 * time.Second; ; wait = min(wait*2, 10*time.Minute) {
		if jid, err = b.Resolve(ctx, number); err == nil {
			break
		}
		var in bot.InputError
		if errors.As(err, &in) {
			b.Log.Warn("webhook filter: chat isn't on WhatsApp, keeping it as typed", "chat", number, "err", err)
			return
		}
		b.Log.Warn("webhook filter: chat lookup failed, retrying", "chat", number, "err", err, "retry_in", wait)
		if bot.SleepUntil(ctx, time.Now().Add(wait)) != nil {
			return
		}
	}
	b.Webhook.SetChat(b.PhoneJID(jid).String())
	b.Log.Info("webhook filter chat resolved", "chat", number, "jid", b.PhoneJID(jid).String())
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// defaultDB is ~/.config/wabot/wabot.db (or $XDG_CONFIG_HOME/wabot/wabot.db) on macOS and Linux alike.
func defaultDB() string {
	if runtime.GOOS == "windows" {
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "wabot", "wabot.db")
		}
		return "wabot.db"
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "wabot", "wabot.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "wabot.db"
	}
	return filepath.Join(home, ".config", "wabot", "wabot.db")
}

// legacyDBNotice points at a wabot.db in the current directory, where older versions kept it.
func legacyDBNotice(path string) {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if _, err := os.Stat("wabot.db"); err != nil {
		return
	}
	dir := filepath.Dir(path)
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
			dir = filepath.Join("~", rest)
		}
	}
	fmt.Fprintf(os.Stderr, "notice: ./wabot.db is no longer the default; move it with 'mv wabot.db* %s/' or pass '-db wabot.db'\n", dir)
}

// newLogger builds the app logger. format is "text" or "json"; file "" means stderr.
func newLogger(format, file, level string, quiet bool) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stderr
	var closer io.Closer = io.NopCloser(nil)
	switch {
	case file != "":
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w, closer = f, f
	case quiet:
		w = io.Discard
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts)), closer, nil
	}
	return slog.New(slog.NewTextHandler(w, opts)), closer, nil
}
