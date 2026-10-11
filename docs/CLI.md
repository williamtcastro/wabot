# CLI reference

```
wabot <command> [flags] [args]
```

Flags go right after the command and before any other arguments, e.g.
`wabot serve -addr :9000 -token secret` or `wabot send -db other.db 5511999999999 hi`.
Every flag except `-phone`, `-autoreply` and `-debug` can also be set with an environment variable.

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

### `wabot serve [-addr :8080]`

Runs the [HTTP API](API.md) until Ctrl+C. If the session isn't logged in, it prints QR codes
in the terminal and serves them at `/qr`, so you can log in from a browser.

### `wabot tui [-addr ADDR]`

Interactive terminal UI. See [TUI.md](TUI.md). With `-addr` it also runs the HTTP API in the
same process.

### `wabot send NUMBER|JID TEXT...`

Sends one message and exits. Handy for scripts and cron:

```sh
./wabot send 5511999999999 "Backup finished ✅"
./wabot send 120363012345678901@g.us "Deploy done"
```

Needs an existing login. Auto-replies are disabled for this command.

### `wabot logout`

Unlinks the device from your phone and deletes the session. Messages stored in the
`wabot_messages` table are kept; delete the database file to remove everything.

### `wabot help`

Prints usage.

## Flags

| Flag | Env | Default | |
|---|---|---|---|
| `-db PATH` | `WABOT_DB` | `wabot.db` | session and message database (SQLite) |
| `-addr ADDR` | `WABOT_ADDR` | `:8080` for `serve`, off for `tui` | HTTP listen address |
| `-token TOKEN` | `WABOT_TOKEN` | none | require `Authorization: Bearer TOKEN` on the API |
| `-webhook URL` | `WABOT_WEBHOOK` | none | POST every incoming message to this URL |
| `-autoreply` | | `true` | answer `!ping`, `!time`, `!help` |
| `-phone NUMBER` | | | `login` only: use a pairing code |
| `-debug` | | `false` | verbose whatsmeow logs (not shown in the TUI) |

## The database

One SQLite file (`wabot.db` plus its `-wal`/`-shm` companions) holds:

- whatsmeow's tables (`whatsmeow_*`): your encryption keys, session, contacts, and
  phone-number ↔ hidden-ID links. **Anyone with this file can act as your WhatsApp
  account.** Keep it private and out of git (the included `.gitignore` already excludes it).
- `wabot_messages`: every message the bot has seen or sent, which powers `/messages`,
  `/chats` and the TUI chat list.

You can inspect it with any SQLite tool, e.g.
`sqlite3 wabot.db "select chat, text from wabot_messages order by ts desc limit 10"`.

Only one process can use a database at a time, so don't run `serve`, `tui` and `send` on
the same file at once.

## Exit codes

`0` on success, `1` on any error (printed to stderr), `2` when called without a command.
