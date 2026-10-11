package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const usage = `wabot - a tiny WhatsApp bot built on whatsmeow

Usage:
  wabot login [-phone NUMBER]          link this bot to your WhatsApp (QR or pairing code)
  wabot serve [-addr :8080]            run the HTTP API
  wabot tui   [-addr :8080]            interactive terminal UI (optionally also serves the API)
  wabot send  NUMBER|JID TEXT...       send one message and exit
  wabot logout                         unlink the device and delete the session

Flags (right after the command, before other arguments):
  -db PATH        session database (env WABOT_DB, default wabot.db)
  -token TOKEN    require "Authorization: Bearer TOKEN" on the API (env WABOT_TOKEN)
  -webhook URL    POST every incoming message as JSON to URL (env WABOT_WEBHOOK)
  -autoreply      answer !ping, !time and !help (default true)
  -debug          verbose whatsmeow logs

Docs: README.md, docs/*.md, or http://localhost:8080/docs while serving`

type config struct {
	db, addr, token, webhook, phone string
	autoreply, debug                bool
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
	fs.StringVar(&c.db, "db", envOr("WABOT_DB", "wabot.db"), "")
	fs.StringVar(&c.addr, "addr", envOr("WABOT_ADDR", ""), "")
	fs.StringVar(&c.token, "token", os.Getenv("WABOT_TOKEN"), "")
	fs.StringVar(&c.webhook, "webhook", os.Getenv("WABOT_WEBHOOK"), "")
	fs.StringVar(&c.phone, "phone", "", "")
	fs.BoolVar(&c.autoreply, "autoreply", true, "")
	fs.BoolVar(&c.debug, "debug", false, "")
	_ = fs.Parse(args)

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
	var log waLog.Logger = waLog.Stdout("wabot", level, true)
	if cmd == "tui" || cmd == "send" {
		log = waLog.Noop
	}
	if dir := filepath.Dir(c.db); dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}

	b, err := NewBot(ctx, c.db, log)
	if err != nil {
		return err
	}
	b.AutoReply = c.autoreply && cmd != "send"
	b.WebhookURL = c.webhook
	defer b.Client.Disconnect()

	switch cmd {
	case "login":
		return login(ctx, b, c.phone)

	case "serve":
		if c.addr == "" {
			c.addr = ":8080"
		}
		if err := b.Start(ctx); err != nil {
			return err
		}
		go printQRUpdates(ctx, b)
		return serve(ctx, b, c)

	case "tui":
		if err := b.Start(ctx); err != nil {
			return err
		}
		if c.addr != "" {
			go func() { _ = serve(ctx, b, c) }()
		}
		return RunTUI(b)

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
		m, err := b.SendText(ctx, args[0], joinArgs(args[1:]))
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
		if err := b.Client.Logout(ctx); err != nil {
			return err
		}
		fmt.Println("logged out")
		return nil
	}
	return fmt.Errorf("unknown command %q (try 'wabot help')", cmd)
}

func login(ctx context.Context, b *Bot, phone string) error {
	if b.Client.Store.ID != nil {
		fmt.Println("already logged in as", b.Client.Store.ID.ToNonAD())
		return nil
	}
	if err := b.Start(ctx); err != nil {
		return err
	}
	if phone != "" {
		for b.Status().QR == "" {
			time.Sleep(200 * time.Millisecond)
		}
		code, err := b.PairPhone(ctx, phone)
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

func printQRUpdates(ctx context.Context, b *Bot) {
	ch, unsub := b.Subscribe()
	defer unsub()
	last := ""
	show := func(s Status) {
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
			if s, ok := v.(Status); ok {
				show(s)
			}
		}
	}
}

func waitLoggedIn(ctx context.Context, b *Bot, timeout time.Duration) error {
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

func serve(ctx context.Context, b *Bot, c config) error {
	srv := &http.Server{Addr: c.addr, Handler: NewAPI(b, c.token)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if c.token == "" {
		fmt.Fprintln(os.Stderr, "warning: API has no token; anyone who can reach", c.addr, "can send messages as you")
	}
	fmt.Fprintln(os.Stderr, "HTTP API listening on", c.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func joinArgs(a []string) string {
	s := a[0]
	for _, x := range a[1:] {
		s += " " + x
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
