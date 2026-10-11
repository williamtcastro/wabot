# CLI reference

```
wabot <command> [flags] [args]
```

Flags go right after the command and before any other arguments, e.g.
`wabot serve -addr :9000 -token secret` or `wabot send -db other.db 5511999999999 hi`.
Boolean flags take `=`: `-autoreply=false`. Durations use Go syntax: `500ms`, `2s`, `1m`.

Pass secrets (`WABOT_TOKEN`, `WABOT_WEBHOOK_SECRET`) as environment variables rather than flags:
command-line arguments are visible to every user on the machine through `ps`.

## Commands

### `wabot login [-phone NUMBER]`

Links the bot to your WhatsApp account as a "Linked device".

- Without `-phone`: prints a QR in the terminal. Scan it from **WhatsApp → Settings →
  Linked devices → Link a device**. Codes refresh about every 20 s.
- With `-phone +5511999999999`: prints an 8-character code. On the phone choose
  **Link with phone number instead** and type it in.

After `✅ logged in` it waits a few seconds for the first sync, then exits. Run it once;
the session lives in the database file from then on. If you're already logged in it says so
and exits.

### `wabot serve [-addr 127.0.0.1:8080]`

Runs the [HTTP API](API.md) until Ctrl+C. If the session isn't logged in, it prints QR codes
in the terminal and serves them at `/qr`, so you can log in from a browser.

It listens on `127.0.0.1:8080` unless you pass `-addr`. Without a token it refuses to start on
an address that isn't loopback (`127.0.0.1`, `::1`, `localhost`), and only accepts same-origin
requests to localhost; see [API.md → Auth](API.md#auth).

### `wabot tui [-addr ADDR]`

Interactive terminal UI. See [TUI.md](TUI.md). With `-addr` it also runs the HTTP API in the
same process, under the same rule: a non-loopback address needs a token.

### `wabot send NUMBER|JID TEXT...`

Sends one message and exits. Handy for scripts and cron:

```sh
./wabot send 5511999999999 "Backup finished ✅"
./wabot send 120363012345678901@g.us "Deploy done"
```

Needs an existing login. Auto-replies are disabled for this command. Prints
`sent <id> to <chat>` on success.

### `wabot logout`

Unlinks the device from your phone and deletes the session. Messages stored in the
`wabot_messages` table and the audit log are kept; delete the database file to remove everything.

### `wabot help`

Prints usage (also `-h` / `--help`).

## Flags

| Flag | Env | Default | |
|---|---|---|---|
| `-db PATH` | `WABOT_DB` | `wabot.db` | session and message database (SQLite) |
| `-addr ADDR` | `WABOT_ADDR` | `127.0.0.1:8080` for `serve`, off for `tui` | HTTP listen address. A non-loopback address requires a token |
| `-token TOKEN` | `WABOT_TOKEN` | none | require `Authorization: Bearer TOKEN` on the API |
| `-webhook URL` | `WABOT_WEBHOOK` | none | POST incoming messages as JSON to this http(s) URL |
| `-webhook-secret S` | `WABOT_WEBHOOK_SECRET` | none | sign webhook bodies with HMAC-SHA256 ([how to verify](BOT.md#verifying-webhook-signatures)) |
| `-webhook-filter Q` | `WABOT_WEBHOOK_FILTER` | none | only deliver matching messages, e.g. `type=text&from_me=false` ([syntax](API.md#filters)). A `chat=<number>` is resolved on WhatsApp after login |
| `-rate N` | `WABOT_RATE` | `20` | max messages sent per minute; `0` = unlimited. Number lookups get 3 × this. `WABOT_RATE` must be a non-negative integer or wabot exits with code 2 |
| `-chat-gap D` | | `1s` | min time between two messages to the same chat |
| `-max-wait D` | | `30s` | queue a send at most this long before answering 429; `0` = wait as long as needed |
| `-typing` | | `false` | show "typing…" before each text message |
| `-online` | | `false` | appear "online" while connected. Off by default so your phone keeps showing notifications; typing indicators still switch to "available" briefly |
| `-autoreply` | | `true` | answer `!ping`, `!time`, `!help` |
| `-log-format F` | `WABOT_LOG_FORMAT` | `text` | app log format: `text` or `json` |
| `-log-file PATH` | `WABOT_LOG_FILE` | stderr | write app logs to PATH (appended, mode 0600) |
| `-debug` | | `false` | debug-level logs, including whatsmeow's |
| `-phone NUMBER` | | | `login` only: use a pairing code instead of a QR |

All flags are accepted by every command, so `-rate`, `-chat-gap` and `-typing` also apply to
`wabot send`. See [API.md → Rate limiting](API.md#rate-limiting) for how the limiter behaves
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
[ADMIN-PANEL.md](ADMIN-PANEL.md#logging-today).

## The database

One SQLite file (`wabot.db` plus its `-wal`/`-shm` companions) holds:

- whatsmeow's tables (`whatsmeow_*`): your encryption keys, session, contacts, and
  phone-number ↔ hidden-ID links. **Anyone with this file can act as your WhatsApp
  account.** Keep it private and out of git (the included `.gitignore` already excludes it).
- `wabot_messages`: every message the bot has seen or sent, which powers `/messages`,
  `/events`, `/chats` and the TUI.
- `wabot_audit`: one row per control action (send, react, edit, delete, read, pair, logout…).
  Served by `GET /audit`.

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

Databases from older versions are upgraded automatically on startup: the table is rebuilt with
the new columns and the explicit `seq` column, keeping every message's existing number.

When WhatsApp reveals which phone number a hidden `…@lid` ID belongs to, messages stored under
the `@lid` chat are moved to the phone-number chat, and leftover duplicates are removed.

At startup wabot sets the database file and its `-wal`/`-shm` files to mode `0600` (owner only).

`wabot_audit` columns: `id`, `at` (Unix ms), `actor`, `action`, `target`, `detail`, `ok`, `error`.

You can inspect the database with any SQLite tool, e.g.
`sqlite3 wabot.db "select seq, chat, type, ts from wabot_messages order by seq desc limit 10"`.

Only one process can use a database at a time, so don't run `serve`, `tui` and `send` on
the same file at once.

## Exit codes

`0` on success, `1` on any error (printed to stderr), `2` when called without a command, with an
unknown flag, or with an invalid `WABOT_RATE`.
