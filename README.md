# wame

**WhatsApp manager for your terminal: a TUI, an HTTP API, a bot and an MCP server in one binary.**

[![CI](https://github.com/williamtcastro/wame/actions/workflows/ci.yml/badge.svg)](https://github.com/williamtcastro/wame/actions/workflows/ci.yml)

wame links to your WhatsApp account and lets you read, search and answer your chats from the
terminal, script them over HTTP, or hand them to an AI agent, with scoped tokens, rate limits and an
audit log keeping each one in check. Built on [whatsmeow](https://github.com/tulir/whatsmeow): one Go
binary, one SQLite file, no cgo.

- **HTTP API**: send texts and files, reply, react, edit, delete, mark as read; read the stored
  history with filters and full-text search; unread counts per chat; long-poll or stream new messages with resumable cursors; download media;
  receive signed webhooks; **scoped API tokens** per agent or bot (read/send/admin, chat allowlists,
  per-token rate, expiry, revocation)
- **MCP server** for AI agents (Claude Code, Claude Desktop…): over HTTP at `/mcp` or `wame mcp` on
  stdio, with the same scopes, limits and audit log
- **TUI**: chat from your terminal, with replies, reactions, edits, media saving, search across all
  chats, unread badges that survive restarts, and command autocomplete
- **CLI**: `wame send 5511999999999 "hello"` from scripts and cron jobs; `wame token` to manage API tokens

It links to your account as a "Linked device", the same way WhatsApp Web does. Outgoing messages
are rate limited by default, and every control action is written to an audit log.

> **Renamed from `wabot`.** Existing setups keep working: a database at `~/.config/wabot/wabot.db`
> is still picked up, `WABOT_*` environment variables are still read, and `wabot_…` tokens still
> authenticate. Webhook headers are now `X-Wame-*`. See [docs/CLI.md](docs/CLI.md#upgrading-from-wabot).

> ⚠️ whatsmeow is unofficial. Automated or bulk messaging can get a number banned.
> Use a number you can afford to lose, keep the rate limits on, and don't spam.

## Install

**Prebuilt binaries:** download the archive for your system (Linux or macOS, amd64 or arm64) from
the [Releases page](https://github.com/williamtcastro/wame/releases), check it against
`checksums.txt`, and put `wame` somewhere on your `PATH`:

```sh
tar xzf wame_*_darwin_arm64.tar.gz
shasum -a 256 -c checksums.txt --ignore-missing
mv wame /usr/local/bin/
wame version
```

**With Go** (1.26+):

```sh
go install github.com/williamtcastro/wame@latest   # installs to $(go env GOPATH)/bin
```

**From source:**

```sh
go build -o wame .
go test ./...          # optional: offline test suite
```

`wame version` prints the release version, or `dev` for a local build.

## 1. Log in (once)

```sh
./wame login                       # prints a QR: WhatsApp → Linked devices → Link a device
./wame login -phone +5511999999999 # or get an 8-character pairing code instead
```

The session is saved to `~/.config/wame/wame.db` on both macOS and Linux (or
`$XDG_CONFIG_HOME/wame/wame.db`; change it with `-db` or `WAME_DB`). Keep that file private:
anyone who has it can act as your WhatsApp account. Older versions kept `wame.db` in the current
directory; if wame finds one there it prints how to move it (`mv wame.db* ~/.config/wame/`) or
keep using it (`-db wame.db`).

## 2a. Run the HTTP API

```sh
export WAME_TOKEN=$(openssl rand -hex 24)   # owner secret, at least 16 characters; keep it safe
./wame serve                                # listens on 127.0.0.1:8080
```

`serve` listens on `127.0.0.1:8080` by default. The owner secret must be at least 16 characters.
Without any token it only accepts same-origin requests from and to localhost, and it refuses to
listen on any other address (`-addr :8080`, a LAN IP). Pass secrets through environment variables
rather than flags, since flags show up in `ps`.

**Give each agent or bot its own token** instead of sharing the owner secret:

```sh
./wame token create support-bot -scopes read,send -chats 5511999999999 -expires 720h
./wame token list
./wame token revoke support-bot
```

Once a token exists, every request needs one, from localhost too. See
[docs/API.md → Tokens and scopes](docs/API.md#tokens-and-scopes).

```sh
H="Authorization: Bearer $WAME_TOKEN"

curl -H "$H" localhost:8080/status

curl -H "$H" -X POST localhost:8080/send \
  -d '{"to":"+55 11 99999-9999","text":"Hello from wame 👋"}'

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

You can also skip `wame login` and do it over HTTP: start `serve`, then open
`http://localhost:8080/qr?token=<your owner secret>` in a browser and scan it.

While the server runs, **`http://localhost:8080/docs`** has interactive API docs where you
can try every endpoint from the browser. Full reference: [docs/API.md](docs/API.md).

### Webhooks

```sh
export WAME_TOKEN=change-me-to-a-long-secret WAME_WEBHOOK_SECRET=another-long-webhook-secret
./wame serve -webhook https://example.com/hook -webhook-filter "type=text&from_me=false"
```

Each new message is POSTed as JSON, signed with HMAC-SHA256 in `X-Wame-Signature`, and retried
with backoff on errors. See [docs/API.md#webhook](docs/API.md#webhook) and, for verification code
in Python and Node, [docs/BOT.md](docs/BOT.md#verifying-webhook-signatures).

## 2b. Run the TUI

```sh
./wame tui                # just the terminal UI
./wame tui -addr 127.0.0.1:8080    # TUI and HTTP API in the same process
```

A chat list on the left, the open conversation on the right. Press `Tab` to move between the
input, the chat list (type to filter) and the messages, or type `/to maria` or
`/to 5511999999999`. Then anything you type goes to that chat. `Ctrl+J` / `Ctrl+K` jump to the
next / previous chat and `Ctrl+D` / `Ctrl+U` scroll half a page. With a message selected,
`j` / `k` move, `g` / `G` go to the first / last, `r` replies, `e` edits, `d d` deletes, `1`–`6`
react and `s` saves media. Typing `/` shows a
hint bar of commands; `Tab` completes command names, chat and contact names after `/to`, numbers
after `/send` and file paths after `/file`. `/search <words>` finds messages in every chat and
jumps to them. `/help` lists everything. See [docs/TUI.md](docs/TUI.md).

## 2c. One-shot CLI

```sh
./wame send 5511999999999 "Backup finished ✅"
./wame send 120363012345678901@g.us "hello group"
```

Don't run `send` while `serve` or `tui` is running with the same database. Only one
connection per session is allowed, so use the HTTP API in that case.

## 2d. Connect an AI agent (MCP)

```sh
./wame token create claude -scopes read -chats 5511999999999 -expires 720h
claude mcp add --transport http wame http://127.0.0.1:8080/mcp --header "Authorization: Bearer wame_…"
```

Or, without a running server, `claude mcp add wame -- wame mcp` (read-only; `-allow-send` to send).
See [docs/MCP.md](docs/MCP.md).

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
- edit `autoReply()` in `internal/bot/events.go`.

See [docs/BOT.md](docs/BOT.md) for examples.

## Rate limiting

Sending a lot of messages quickly is the usual way automated numbers get banned, so wame spaces
out everything it sends (texts, files, reactions, edits, deletes, auto-replies): at most 20 per
minute and one per second per chat by default. A send waits for its slot; if that's more than
30 s away the API answers `429` with `Retry-After`. Tune it with `-rate`, `-chat-gap` and
`-max-wait`. Number lookups (new recipients, `/check`, `/typing`) have their own limit of 3 × `-rate`
per minute, and auto-replies are capped at 6 per minute (extra ones are dropped). By default the
account shows as offline so your phone keeps getting notifications; `-online` changes that. See [docs/API.md#rate-limiting](docs/API.md#rate-limiting).

## Configuration

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-db` | `WAME_DB` | `~/.config/wame/wame.db` | session database |
| `-addr` | `WAME_ADDR` | `127.0.0.1:8080` for `serve` | HTTP listen address; non-loopback needs a token |
| `-token` | `WAME_TOKEN` | none | owner secret for the API, at least 16 characters (prefer per-agent tokens from `wame token create`) |
| `-webhook` | `WAME_WEBHOOK` | none | URL that gets new messages as JSON |
| `-webhook-secret` | `WAME_WEBHOOK_SECRET` | none | HMAC-SHA256 signing secret for the webhook |
| `-webhook-filter` | `WAME_WEBHOOK_FILTER` | none | only deliver matching messages, e.g. `type=text&from_me=false` |
| `-rate` | `WAME_RATE` | `20` | max messages sent per minute; `0` = unlimited |
| `-chat-gap` | | `1s` | min time between messages to the same chat |
| `-max-wait` | | `30s` | longest a send waits in line before a 429 |
| `-typing` | | `false` | show "typing…" before each text |
| `-online` | | `false` | appear "online" while connected (off: your phone keeps getting notifications) |
| `-autoreply` | | `true` | answer `!ping` / `!time` / `!help` |
| `-log-format` | `WAME_LOG_FORMAT` | `text` | app log format: `text` or `json` |
| `-log-file` | `WAME_LOG_FILE` | stderr | write app logs to a file |
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
| [docs/MCP.md](docs/MCP.md) | MCP server for AI agents: HTTP and stdio, tools, safety, client setup |
| [docs/security/SCOPED-TOKENS.md](docs/security/SCOPED-TOKENS.md) | security review of the token system, findings and fixes, operational guidance |
| [docs/proposed-features/](docs/proposed-features/README.md) | ideas not built yet: [web admin panel](docs/proposed-features/ADMIN-PANEL.md) (also documents today's logging and audit structure), [image previews in the TUI](docs/proposed-features/IMAGE-PREVIEWS.md) |

## Files

| Path | What it does |
|---|---|
| `main.go` | CLI commands, flags, default database path, logger setup |
| `internal/bot/` | whatsmeow client, events, sending, replies, reactions, edits, deletes, read receipts, media download, recipient lookup, contacts, auto-replies, rate limiter, webhook |
| `internal/store/` | message history, filters and search, chat list with unread counts, group cache, tokens and audit log (SQLite) |
| `internal/api/` | HTTP API, scopes and chat allowlists per route, long-poll, SSE, idempotency, request logging, `/mcp` and the `/docs` page |
| `internal/auth/` | API tokens: issuing, hashing, verifying, scopes |
| `internal/mcpserver/` | MCP tools (used by `/mcp` and `wame mcp`) |
| `internal/tui/` | terminal UI (Bubble Tea + Bubbles + Lip Gloss) |
| `docs/` | user docs and the OpenAPI spec (embedded into the binary by `docs/docs.go`) |
| `.github/workflows/` | CI (`ci.yml`: gofmt, vet, race tests on Linux and macOS, static build) and releases (`release.yml`) |
| `.goreleaser.yaml` | release builds: Linux/macOS × amd64/arm64 archives and checksums |

## License

[MIT](LICENSE): free to use, modify and redistribute, including commercially.
The [whatsmeow](https://github.com/tulir/whatsmeow) library it builds on is MPL-2.0.
