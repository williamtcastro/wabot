package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/tui"
)

const (
	me     = "5521987654321@s.whatsapp.net"
	maria  = "5511999999999@s.whatsapp.net"
	john   = "5511988887777@s.whatsapp.net"
	ana    = "552187654321@s.whatsapp.net"
	family = "120363012345678901@g.us"
	work   = "120363098765432109@g.us"
)

func main() {
	path := os.Args[1]
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := bot.New(ctx, path, waLog.Noop, quiet)
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	at := func(dayOffset, h, m int) time.Time {
		return today.AddDate(0, 0, dayOffset).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	in := func(id, chat, from, name, text string, t time.Time) store.Message {
		return store.Message{ID: id, Chat: chat, Sender: from, PushName: name, Type: "text", Text: text, Timestamp: t, IsGroup: chat == family || chat == work}
	}
	out := func(id, chat, text string, t time.Time) store.Message {
		return store.Message{ID: id, Chat: chat, Sender: me, FromMe: true, Type: "text", Text: text, Timestamp: t, IsGroup: chat == family || chat == work}
	}
	msgs := []store.Message{
		in("J1", john, john, "John", "Did you get the invoice?", at(-1, 18, 2)),
		out("J2", john, "Yes, paying it tomorrow morning", at(-1, 18, 10)),
		in("J3", john, john, "John", "ok, até amanhã 👋", at(-1, 18, 11)),

		in("W1", work, ana, "Ana", "Deploy is green ✅", at(0, 9, 30)),
		out("W2", work, "Nice, shipping the release notes now", at(0, 9, 32)),

		in("M1", maria, maria, "Maria", "oi, tudo bem?", at(-1, 21, 40)),
		out("M2", maria, "tudo, e você?", at(-1, 21, 41)),
		in("M3", maria, maria, "Maria", "bem também! vamos almoçar amanhã?", at(0, 11, 2)),
		{ID: "M4", Chat: maria, Sender: me, FromMe: true, Type: "text", Text: "bora! 12h30 no lugar de sempre?", ReplyTo: "M3", Timestamp: at(0, 11, 5)},
		{ID: "M5", Chat: maria, Sender: maria, Type: "reaction", Text: "❤️", ReplyTo: "M4", Timestamp: at(0, 11, 6)},
		in("M6", maria, maria, "Maria", "perfeito. te mando o cardápio", at(0, 11, 6)),
		{ID: "M7", Chat: maria, Sender: maria, PushName: "Maria", Type: "document", Media: &store.Media{Mimetype: "application/pdf", FileName: "cardapio.pdf", Size: 86016}, Timestamp: at(0, 11, 7)},
		out("M8", maria, "valeu! 🙌", at(0, 11, 9)),

		in("F1", family, ana, "Ana", "Who's bringing dessert on Sunday?", at(0, 12, 40)),
		{ID: "F2", Chat: family, Sender: john, PushName: "John", IsGroup: true, Type: "image", Text: "foto da festa", Media: &store.Media{Mimetype: "image/jpeg", Size: 120000}, Timestamp: at(0, 12, 44)},
		in("F3", family, ana, "Ana", "haha que saudade 😂", at(0, 12, 45)),
	}
	for _, m := range msgs {
		if _, _, err := b.Store.Save(m); err != nil {
			log.Fatal(err)
		}
	}
	for _, c := range []string{maria, john, work} {
		b.Store.MarkChatRead(c)
	}
	if err := b.Store.SaveGroups([]store.Group{{JID: family, Name: "Família"}, {JID: work, Name: "Release crew"}}); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 2 && os.Args[2] == "seed" {
		return
	}
	b, err = bot.New(ctx, path, waLog.Noop, quiet)
	if err != nil {
		log.Fatal(err)
	}
	if err := tui.Run(b); err != nil {
		log.Fatal(err)
	}
}
