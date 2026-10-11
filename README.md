# wabot

A small, self-contained WhatsApp bot built on [whatsmeow](https://github.com/tulir/whatsmeow).
One Go binary, one SQLite file, no cgo. Use it three ways:

- **HTTP API**: send messages and files, read the inbox, stream events, receive webhooks
- **TUI**: chat from your terminal
- **CLI**: `wabot send 5511999999999 "hello"` from scripts and cron jobs

It links to your account as a "Linked device", the same way WhatsApp Web does.

> ⚠️ whatsmeow is unofficial. Automated or bulk messaging can get a number banned.
> Use a number you can afford to lose and don't spam.

## Build

Requires Go 1.24+.

```sh
go build -o wabot .
```

## 1. Log in (once)

```sh
./wabot login                       # prints a QR: WhatsApp → Linked devices → Link a device
./wabot login -phone +5511999999999 # or get an 8-character pairing code instead
```

The session is saved to `wabot.db` (change it with `-db` or `WABOT_DB`). Keep that file private:
anyone who has it can act as your WhatsApp account.

## 2a. Run the HTTP API

```sh
WABOT_TOKEN=change-me ./wabot serve -addr :8080
```

```sh
curl -H "Authorization: Bearer change-me" localhost:8080/status

curl -H "Authorization: Bearer change-me" -X POST localhost:8080/send \
  -d '{"to":"+55 11 99999-9999","text":"Hello from wabot 👋"}'

curl -H "Authorization: Bearer change-me" -X POST localhost:8080/send/file \
  -F to=5511999999999 -F caption="look at this" -F file=@photo.jpg

curl -H "Authorization: Bearer change-me" "localhost:8080/messages?limit=10"

curl -N -H "Authorization: Bearer change-me" localhost:8080/events   # live stream
```

You can also skip `wabot login` and do it over HTTP: start `serve`, then open
`http://localhost:8080/qr?token=change-me` in a browser and scan it.

While the server runs, **`http://localhost:8080/docs`** has interactive API docs where you
can try every endpoint from the browser. Full reference: [docs/API.md](docs/API.md).

## 2b. Run the TUI

```sh
./wabot tui                # just the terminal UI
./wabot tui -addr :8080    # TUI and HTTP API in the same process
```

A chat list on the left, the open conversation on the right. Press `Tab` to pick a chat,
or type `/to maria` or `/to 5511999999999`. Then anything you type goes to that chat.
`/contacts` lists your address book and `/help` lists everything else.
See [docs/TUI.md](docs/TUI.md).

## 2c. One-shot CLI

```sh
./wabot send 5511999999999 "Backup finished ✅"
./wabot send 120363012345678901@g.us "hello group"
```

Don't run `send` while `serve` or `tui` is running with the same `wabot.db`. Only one
connection per session is allowed, so use the HTTP API in that case.

## Bot behaviour

With `-autoreply` (on by default), the bot answers these in any chat, including your own:

| Message | Reply |
|---|---|
| `!ping` | `pong 🏓` |
| `!time` | current server time |
| `!help` | the command list |

Turn it off with `-autoreply=false`. For your own logic, either:

- set `-webhook https://your-app/hook` and reply through `POST /send` (works from any language), or
- edit `autoReply()` in `bot.go`.

See [docs/BOT.md](docs/BOT.md) for a webhook example.

## Configuration

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-db` | `WABOT_DB` | `wabot.db` | session database |
| `-addr` | `WABOT_ADDR` | `:8080` for `serve` | HTTP listen address |
| `-token` | `WABOT_TOKEN` | none | API bearer token (strongly recommended) |
| `-webhook` | `WABOT_WEBHOOK` | none | URL that gets every incoming message as JSON |
| `-autoreply` | | `true` | answer `!ping` / `!time` / `!help` |
| `-debug` | | `false` | verbose whatsmeow logs |
| `-phone` | | | `login` only: use a pairing code instead of a QR |

## Recipients

Wherever a recipient is expected you can pass:

- a phone number in any format with the country code: `5511999999999`, `+55 (11) 99999-9999`
- a full JID: `5511999999999@s.whatsapp.net`, a group `1203…@g.us`, or a `…@lid` user

Find group JIDs with `GET /groups` or `/groups` in the TUI.

## Documentation

| Doc | |
|---|---|
| [docs/CLI.md](docs/CLI.md) | every command and flag, the database file |
| [docs/API.md](docs/API.md) | every HTTP endpoint with curl examples, events, webhook |
| [docs/openapi.yaml](docs/openapi.yaml) | OpenAPI 3 spec (import into Postman/Insomnia/Bruno); served live at `/docs` |
| [docs/TUI.md](docs/TUI.md) | terminal UI layout, keys and commands |
| [docs/BOT.md](docs/BOT.md) | writing your own bot logic (webhook or Go), running as a service |

## Files

| File | What it does |
|---|---|
| `main.go` | CLI commands and flags |
| `bot.go` | whatsmeow client, events, sending, recipient lookup, contacts, auto-replies |
| `store.go` | message history and chat list (SQLite) |
| `api.go` | HTTP API and the `/docs` page |
| `tui.go` | terminal UI (Bubble Tea + Bubbles + Lip Gloss) |
