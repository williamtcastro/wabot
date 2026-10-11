# wabot

A small, self-contained WhatsApp bot built on [whatsmeow](https://github.com/tulir/whatsmeow).
One Go binary, one SQLite file, no cgo. Use it three ways:

- **HTTP API**: send texts and files, reply, react, edit, delete, mark as read; read the stored
  history with filters; long-poll or stream new messages with resumable cursors; download media;
  receive signed webhooks
- **TUI**: chat from your terminal, with replies, reactions, edits, media saving and command autocomplete
- **CLI**: `wabot send 5511999999999 "hello"` from scripts and cron jobs

It links to your account as a "Linked device", the same way WhatsApp Web does. Outgoing messages
are rate limited by default, and every control action is written to an audit log.

> ⚠️ whatsmeow is unofficial. Automated or bulk messaging can get a number banned.
> Use a number you can afford to lose, keep the rate limits on, and don't spam.

## Build

Requires Go 1.26+.

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
WABOT_TOKEN=change-me ./wabot serve          # listens on 127.0.0.1:8080
```

`serve` listens on `127.0.0.1:8080` by default. Without a token it only accepts same-origin
requests to localhost, and it refuses to listen on any other address (`-addr :8080`, a LAN IP)
unless a token is set. Pass secrets through environment variables rather than flags, since flags
show up in `ps`.

```sh
H="Authorization: Bearer change-me"

curl -H "$H" localhost:8080/status

curl -H "$H" -X POST localhost:8080/send \
  -d '{"to":"+55 11 99999-9999","text":"Hello from wabot 👋"}'

# reply to a message, and react to it
curl -H "$H" -X POST localhost:8080/send \
  -d '{"to":"5511999999999","text":"on it","reply_to":"3EB0C767D26A1D0F5E2A"}'
curl -H "$H" -X POST localhost:8080/react \
  -d '{"chat":"5511999999999","id":"3EB0C767D26A1D0F5E2A","emoji":"👍"}'

curl -H "$H" -X POST localhost:8080/send/file \
  -F to=5511999999999 -F caption="look at this" -F file=@photo.jpg

curl -H "$H" "localhost:8080/messages?limit=10&type=text"

# wait up to 60 s for the next incoming message
curl -H "$H" "localhost:8080/messages/wait?from_me=false&timeout=60"

curl -N -H "$H" localhost:8080/events   # live stream (SSE)
```

You can also skip `wabot login` and do it over HTTP: start `serve`, then open
`http://localhost:8080/qr?token=change-me` in a browser and scan it.

While the server runs, **`http://localhost:8080/docs`** has interactive API docs where you
can try every endpoint from the browser. Full reference: [docs/API.md](docs/API.md).

### Webhooks

```sh
export WABOT_TOKEN=change-me WABOT_WEBHOOK_SECRET=another-secret
./wabot serve -webhook https://example.com/hook -webhook-filter "type=text&from_me=false"
```

Each new message is POSTed as JSON, signed with HMAC-SHA256 in `X-Wabot-Signature`, and retried
with backoff on errors. See [docs/API.md#webhook](docs/API.md#webhook) and, for verification code
in Python and Node, [docs/BOT.md](docs/BOT.md#verifying-webhook-signatures).

## 2b. Run the TUI

```sh
./wabot tui                # just the terminal UI
./wabot tui -addr 127.0.0.1:8080    # TUI and HTTP API in the same process
```

A chat list on the left, the open conversation on the right. Press `Tab` to move between the
input, the chat list (type to filter) and the messages, or type `/to maria` or
`/to 5511999999999`. Then anything you type goes to that chat. With a message selected,
`r` replies, `e` edits, `d d` deletes, `1`–`6` react and `s` saves media. Typing `/` shows a
hint bar of commands; `Tab` completes command names, chat and contact names after `/to`, numbers
after `/send` and file paths after `/file`. `/help` lists everything. See [docs/TUI.md](docs/TUI.md).

## 2c. One-shot CLI

```sh
./wabot send 5511999999999 "Backup finished ✅"
./wabot send 120363012345678901@g.us "hello group"
```

Don't run `send` while `serve` or `tui` is running with the same `wabot.db`. Only one
connection per session is allowed, so use the HTTP API in that case.

## Bot behaviour

With `-autoreply` (on by default for `serve` and `tui`), the bot answers these in any chat,
including your own "Message yourself" chat, quoting the command:

| Message | Reply |
|---|---|
| `!ping` | `pong 🏓` |
| `!time` | current server time |
| `!help` | the command list |

Turn it off with `-autoreply=false`. For your own logic, either:

- set `-webhook https://your-app/hook` and reply through `POST /send` (works from any language),
- poll `GET /messages/wait` or stream `GET /events` if you can't expose a port, or
- edit `autoReply()` in `bot.go`.

See [docs/BOT.md](docs/BOT.md) for examples.

## Rate limiting

Sending a lot of messages quickly is the usual way automated numbers get banned, so wabot spaces
out everything it sends (texts, files, reactions, edits, deletes, auto-replies): at most 20 per
minute and one per second per chat by default. A send waits for its slot; if that's more than
30 s away the API answers `429` with `Retry-After`. Tune it with `-rate`, `-chat-gap` and
`-max-wait`. Number lookups (new recipients, `/check`, `/typing`) have their own limit of 3 × `-rate`
per minute, and auto-replies are capped at 6 per minute (extra ones are dropped). By default the
account shows as offline so your phone keeps getting notifications; `-online` changes that. See [docs/API.md#rate-limiting](docs/API.md#rate-limiting).

## Configuration

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-db` | `WABOT_DB` | `wabot.db` | session database |
| `-addr` | `WABOT_ADDR` | `127.0.0.1:8080` for `serve` | HTTP listen address; non-loopback needs a token |
| `-token` | `WABOT_TOKEN` | none | API bearer token (strongly recommended) |
| `-webhook` | `WABOT_WEBHOOK` | none | URL that gets new messages as JSON |
| `-webhook-secret` | `WABOT_WEBHOOK_SECRET` | none | HMAC-SHA256 signing secret for the webhook |
| `-webhook-filter` | `WABOT_WEBHOOK_FILTER` | none | only deliver matching messages, e.g. `type=text&from_me=false` |
| `-rate` | `WABOT_RATE` | `20` | max messages sent per minute; `0` = unlimited |
| `-chat-gap` | | `1s` | min time between messages to the same chat |
| `-max-wait` | | `30s` | longest a send waits in line before a 429 |
| `-typing` | | `false` | show "typing…" before each text |
| `-online` | | `false` | appear "online" while connected (off: your phone keeps getting notifications) |
| `-autoreply` | | `true` | answer `!ping` / `!time` / `!help` |
| `-log-format` | `WABOT_LOG_FORMAT` | `text` | app log format: `text` or `json` |
| `-log-file` | `WABOT_LOG_FILE` | stderr | write app logs to a file |
| `-debug` | | `false` | debug-level logs, including whatsmeow's |
| `-phone` | | | `login` only: use a pairing code instead of a QR |

Details in [docs/CLI.md](docs/CLI.md).

## Recipients

Wherever a recipient is expected you can pass:

- a phone number in any format with the country code: `5511999999999`, `+55 (11) 99999-9999`
- a full JID: `5511999999999@s.whatsapp.net`, a group `1203…@g.us`, or a `…@lid` user

Phone numbers are checked with WhatsApp first; a number that isn't on WhatsApp is rejected
with a 400. Find group JIDs with `GET /groups` or `/groups` in the TUI.

## Documentation

| Doc | |
|---|---|
| [docs/CLI.md](docs/CLI.md) | every command and flag, logs, the database file |
| [docs/API.md](docs/API.md) | every HTTP endpoint with curl examples, filters, events, rate limiting, webhook |
| [docs/openapi.yaml](docs/openapi.yaml) | OpenAPI 3 spec (import into Postman/Insomnia/Bruno); served live at `/docs` |
| [docs/TUI.md](docs/TUI.md) | terminal UI layout, keys and commands |
| [docs/BOT.md](docs/BOT.md) | writing your own bot logic (webhook, polling or Go), verifying signatures, running as a service |
| [docs/ADMIN-PANEL.md](docs/ADMIN-PANEL.md) | plan for a web admin panel; current logging and audit structure |

## Files

| File | What it does |
|---|---|
| `main.go` | CLI commands and flags |
| `bot.go` | whatsmeow client, events, sending, replies, reactions, edits, deletes, read receipts, media download, recipient lookup, contacts, auto-replies |
| `store.go` | message history, filters, chat list (SQLite) |
| `api.go` | HTTP API, long-poll, SSE, idempotency, request logging and the `/docs` page |
| `webhook.go` | webhook queue, retries and HMAC signing |
| `limit.go` | outgoing message rate limiter |
| `audit.go` | audit log table and app logger setup |
| `tui.go` | terminal UI (Bubble Tea + Bubbles + Lip Gloss) |

## License

[MIT](LICENSE): free to use, modify and redistribute, including commercially.
The [whatsmeow](https://github.com/tulir/whatsmeow) library it builds on is MPL-2.0.
