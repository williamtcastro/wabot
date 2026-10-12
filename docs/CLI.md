# CLI reference

```
wame <command> [flags] [args]
```

Flags go right after the command and before any other arguments, e.g.
`wame serve -addr 127.0.0.1:9000 -debug` or `wame send -db other.db 5511999999999 hi`.
Boolean flags take `=`: `-autoreply=false`. Durations use Go syntax: `500ms`, `2s`, `1m`.

Pass secrets (`WAME_TOKEN`, `WAME_WEBHOOK_SECRET`) as environment variables rather than flags:
command-line arguments are visible to every user on the machine through `ps`.

## Commands

### `wame login [-phone NUMBER]`

Links the bot to your WhatsApp account as a "Linked device".

- Without `-phone`: prints a QR in the terminal. Scan it from **WhatsApp → Settings →
  Linked devices → Link a device**. Codes refresh about every 20 s.
- With `-phone +5511999999999`: prints an 8-character code. On the phone choose
  **Link with phone number instead** and type it in.

After `✅ logged in` it waits a few seconds for the first sync, then exits. Run it once;
the session lives in the database file from then on. If you're already logged in it says so
and exits.

### `wame serve [-addr 127.0.0.1:8080]`

Runs the [HTTP API](API.md) until Ctrl+C. If the session isn't logged in, it prints QR codes
in the terminal and serves them at `/qr`, so you can log in from a browser.

It listens on `127.0.0.1:8080` unless you pass `-addr`. It also serves the [MCP endpoint](MCP.md)
at `/mcp`.

It refuses to start when:

- the owner secret (`WAME_TOKEN` / `-token`) is shorter than 16 characters
  (generate one with `openssl rand -hex 24`);
- the address isn't loopback (`127.0.0.1`, `::1`, `localhost`) and there's neither an owner secret
  nor any token ever created.

Without any token it only accepts same-origin requests from and to localhost; see
[API.md → Auth](API.md#auth).

### `wame tui [-addr ADDR]`

Interactive terminal UI. See [TUI.md](TUI.md). With `-addr` it also runs the HTTP API in the
same process, under the same rules (a non-loopback address needs a token; the owner secret must be
at least 16 characters).

### `wame send NUMBER|JID TEXT...`

Sends one message and exits. Handy for scripts and cron:

```sh
./wame send 5511999999999 "Backup finished ✅"
./wame send 120363012345678901@g.us "Deploy done"
```

Needs an existing login. Auto-replies are disabled for this command. Prints
`sent <id> to <chat>` on success.

### `wame token create NAME [-scopes read,send] [-chats N,...] [-rate N] [-expires 720h]`

Creates an API token for an agent, bot or script and prints it **once**:

```sh
./wame token create support-bot -scopes read,send -chats 5511999999999,120363012345678901@g.us -expires 720h
```

| Flag | Default | |
|---|---|---|
| `-scopes` | `read` | comma-separated: `read`, `send`, `admin` (admin implies the others) |
| `-chats` | all chats | comma-separated phone numbers or JIDs the token is limited to. Not allowed with `admin` |
| `-rate` | `10` | the token's own sends per minute, on top of the global `-rate`; `0` = global limit only |
| `-expires` | never | lifetime as a Go duration, e.g. `720h` |

Names are 1–40 of `a-z 0-9 . _ -`, unique forever (revoked names stay taken). `api`, `owner`,
`local`, `tui`, `cli`, `autoreply` and `whatsapp` are reserved. This is the only way to create an
`admin` token besides the owner secret: tokens can't create admin tokens over the API.

**From the first token on, the API requires a token on every request, from localhost too, forever**
(even after every token is revoked). Prefer one token per agent over sharing the owner secret. See
[API.md → Tokens and scopes](API.md#tokens-and-scopes).

The command only touches the database; it doesn't connect to WhatsApp, so it can run while `serve`
is running.

### `wame token list`

Prints every token: name, scopes, rate, status (active, revoked, expired), expiry, last use, the token
that created it (`-` for the CLI or owner; revoking that one revokes this one too) and chats.
Secrets are never shown.

### `wame token revoke NAME`

Revokes the token and every token it created over the API. Open streams using it close within
10 seconds. Fails if there's no active token with that name.

### `wame mcp [-allow-send] [-mcp-token TOKEN]`

Runs an [MCP](MCP.md) server on stdio for AI agents. Read-only by default; `-allow-send` adds
`send_message`, `react` and `mark_read`. `-mcp-token TOKEN` (or `WAME_MCP_TOKEN`) makes it act as
that API token, with its scopes and chat allowlist. It needs an existing login and opens its own
WhatsApp connection, so don't run it alongside `serve` or `tui` on the same database; connect the
agent to `http://127.0.0.1:8080/mcp` instead. stdout carries the protocol; logs go to stderr.

### `wame admin [-server URL] [-insecure]`

Opens a terminal [admin panel](ADMIN.md) for a server running elsewhere: status and remote login
(QR or pairing code), tokens (create, revoke) and the audit log. It only talks to the HTTP API, so
it needs no database and can run next to `serve`. `-server` (or `WAME_SERVER`, default
`http://127.0.0.1:8080`) is the server's URL; the token comes from `WAME_TOKEN` (or `-token`) and
must be the owner secret or have the admin scope. Plain `http://` is refused for non-loopback
addresses unless you pass `-insecure`.

### `wame logout`

Unlinks the device from your phone and deletes the session. Messages stored in the
`wabot_messages` table and the audit log are kept; delete the database file to remove everything.

### `wame version`

Prints `wame <version>`. Release builds carry the tag (e.g. `wame 0.3.0`); anything built
locally prints `wame dev`. `--version` works too.

### `wame help`

Prints usage (also `-h` / `--help`).

## Upgrading from wabot

wame was called `wabot`. After switching binaries nothing has to change:

- **Database:** with no `-db`/`WAME_DB`, wame uses `~/.config/wame/wame.db`, or
  `~/.config/wabot/wabot.db` if that exists and the new one doesn't. To move it, stop wame and run
  `mkdir -p ~/.config/wame && for f in ~/.config/wabot/wabot.db*; do mv "$f" ~/.config/wame/"$(basename "$f" | sed s/wabot/wame/)"; done`.
  The tables inside keep their `wabot_` names.
- **Environment:** every `WAME_*` variable falls back to its old `WABOT_*` name.
- **Tokens:** new tokens start with `wame_`; existing `wabot_…` tokens keep working.
- **Webhooks:** headers are now `X-Wame-Event`, `X-Wame-Delivery`, `X-Wame-Timestamp` and
  `X-Wame-Signature` (same values and signing scheme). Update receivers that read `X-Wabot-*`.

## Flags

| Flag | Env | Default | |
|---|---|---|---|
| `-db PATH` | `WAME_DB` | `~/.config/wame/wame.db` | session and message database (SQLite). See [The database](#the-database) |
| `-addr ADDR` | `WAME_ADDR` | `127.0.0.1:8080` for `serve`, off for `tui` | HTTP listen address. A non-loopback address requires a token |
| `-token TOKEN` | `WAME_TOKEN` | none | owner secret: full access with `Authorization: Bearer TOKEN`. At least 16 characters. Prefer per-agent tokens from `wame token create` |
| `-webhook URL` | `WAME_WEBHOOK` | none | POST incoming messages as JSON to this http(s) URL |
| `-webhook-secret S` | `WAME_WEBHOOK_SECRET` | none | sign webhook bodies with HMAC-SHA256 ([how to verify](BOT.md#verifying-webhook-signatures)) |
| `-webhook-filter Q` | `WAME_WEBHOOK_FILTER` | none | only deliver matching messages, e.g. `type=text&from_me=false` ([syntax](API.md#filters)). A `chat=<number>` is resolved on WhatsApp after login |
| `-rate N` | `WAME_RATE` | `20` | max messages sent per minute; `0` = unlimited. Number lookups get 3 × this. `WAME_RATE` must be a non-negative integer or wame exits with code 2 |
| `-chat-gap D` | | `1s` | min time between two messages to the same chat |
| `-max-wait D` | | `30s` | queue a send at most this long before answering 429; `0` = wait as long as needed |
| `-typing` | | `false` | show "typing…" before each text message |
| `-online` | | `false` | appear "online" while connected. Off by default so your phone keeps showing notifications; typing indicators still switch to "available" briefly |
| `-autoreply` | | `true` | answer `!ping`, `!time`, `!help` |
| `-log-format F` | `WAME_LOG_FORMAT` | `text` | app log format: `text` or `json` |
| `-log-file PATH` | `WAME_LOG_FILE` | stderr | write app logs to PATH (appended, mode 0600) |
| `-debug` | | `false` | debug-level logs, including whatsmeow's |
| `-phone NUMBER` | | | `login` only: use a pairing code instead of a QR |

All flags are accepted by every command, so `-rate`, `-chat-gap` and `-typing` also apply to
`wame send`. See [API.md → Rate limiting](API.md#rate-limiting) for how the limiter behaves
and why it's on by default.

## Logs

There are two log streams:

- **App logs** (`log/slog`): HTTP requests, actions, webhook deliveries, connection state.
  Text or JSON (`-log-format`), to stderr or `-log-file`. Level is info, or debug with `-debug`.
- **whatsmeow logs**: protocol-level messages printed to stdout, info level, debug with `-debug`.

In `tui` and `send` modes whatsmeow logs are always off, and app logs are discarded unless you
set `-log-file`, so they don't break the screen or a script's output.

Each HTTP request produces one line with `method`, `path`, `status`, `duration_ms` and
`remote`. The query string is left out on purpose because it may contain the token.
Requests to `/status`, `/messages/wait` and `/events` log at debug level unless they fail.

```
time=2026-10-10T22:41:03.120-03:00 level=INFO msg=http method=POST path=/send status=200 duration_ms=812 remote=127.0.0.1:52144
time=2026-10-10T22:41:03.119-03:00 level=INFO msg=action actor=api action=send target=5511999999999@s.whatsapp.net detail="text, 6 chars"
```

Message text, the token and the webhook URL are never logged. `*.log` files are in `.gitignore`. More on the log fields and the audit table in
[proposed-features/ADMIN-PANEL.md](proposed-features/ADMIN-PANEL.md#logging-today).

## The database

One SQLite file (`wame.db` plus its `-wal`/`-shm` companions) holds everything. By default it
lives in `~/.config/wame/` on both macOS and Linux, or in `$XDG_CONFIG_HOME/wame/` when that
variable is set to an absolute path (on Windows: `%AppData%\wame\`). `-db` and `WAME_DB` override
it. The directory is created with mode `0700` if it doesn't exist.

Older versions kept `wame.db` in the current directory. If the default file doesn't exist yet but
a `./wame.db` does, wame prints a one-line notice to stderr and starts a fresh session at the
default path. It never moves anything by itself; to keep your session, either move it:

```sh
mkdir -p ~/.config/wame && mv wame.db* ~/.config/wame/
```

or keep using it where it is with `-db wame.db` (or `WAME_DB=wame.db`).

The file holds:

- whatsmeow's tables (`whatsmeow_*`): your encryption keys, session, contacts, and
  phone-number ↔ hidden-ID links. **Anyone with this file can act as your WhatsApp
  account.** Keep it private and out of git (the included `.gitignore` already excludes it).
wame's own tables:

| Table | Holds |
|---|---|
| `wabot_messages` | every message the bot has seen or sent, which powers `/messages`, `/events`, `/chats` and the TUI (columns below) |
| `wabot_messages_fts` | full-text index of `wabot_messages.text` (SQLite FTS5, accents folded) for `q=` and `/search`. Kept in sync by triggers on insert, update of `text` and delete |
| `wabot_chat_state` | per chat, `read_seq`: the read position behind `unread` in `/chats` and the TUI badges |
| `wabot_groups` | cached group names and admin lists (`jid`, `name`, `admins` as JSON, `updated`), so names show before connecting |
| `wabot_audit` | one row per control action (send, react, edit, delete, read, pair, logout…), served by `GET /audit` |
| `wabot_tokens` | API tokens: id, name, SHA-256 hash of the token (never the token itself), scopes, chats, rate, created/expires/last-used times, revoked flag, `created_by`. Rows are never deleted |

`wabot_messages` columns:

| Column | |
|---|---|
| `seq` | `INTEGER PRIMARY KEY AUTOINCREMENT`: the cursor used by `since` and `/media/{seq}`. Never reused, even after deletes |
| `chat`, `id` | `UNIQUE`; a message WhatsApp sends twice is stored once |
| `sender`, `push_name`, `from_me`, `is_group`, `type`, `text`, `ts` | the basics; `ts` is Unix milliseconds |
| `reply_to` | quoted message id, or the target of a reaction |
| `mimetype`, `file_name`, `file_size` | media metadata (empty / 0 when there's no media) |
| `edited`, `deleted` | `1` after an edit or delete |
| `raw` | the original message protobuf. Needed to quote a message in a reply and to download its media. Cleared when the message is deleted |
| `raw_chat`, `raw_sender` | the JIDs exactly as WhatsApp sent them (possibly `…@lid`), needed for replies, reactions, deletes and read receipts |

Databases from older versions are upgraded automatically on startup, with no command to run:
`wabot_messages` is rebuilt with the new columns and the explicit `seq` column (keeping every
message's existing number), missing tables are created, and the search index is built from the
stored messages. When `wabot_chat_state` is first created, every chat's read position is set to
its newest message, so nothing already stored shows as unread.

When WhatsApp reveals which phone number a hidden `…@lid` ID belongs to, messages stored under
the `@lid` chat are moved to the phone-number chat, and leftover duplicates are removed.

At startup wame sets the database file and its `-wal`/`-shm` files to mode `0600` (owner only).

`wabot_audit` columns: `id`, `at` (Unix ms), `actor`, `action`, `target`, `detail`, `ok`, `error`.

You can inspect the database with any SQLite tool, e.g.
`sqlite3 ~/.config/wame/wame.db "select seq, chat, type, ts from wabot_messages order by seq desc limit 10"`.

Only one process can use a database at a time, so don't run `serve`, `tui` and `send` on
the same file at once.

## Exit codes

`0` on success, `1` on any error (printed to stderr), `2` when called without a command, with an
unknown flag, or with an invalid `WAME_RATE`.
