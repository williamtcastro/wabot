# AGENTS.md

Guidance for AI coding agents working on this repo. Human-facing docs are in `README.md` and `docs/`.

## What this is

`wabot` is a single Go binary that links to a WhatsApp account through
[whatsmeow](https://github.com/tulir/whatsmeow) and exposes it three ways: an HTTP API, a Bubble Tea
TUI and a one-shot CLI. All state lives in one SQLite file (default `wabot.db`).

## Layout

| File | Responsibility |
|---|---|
| `main.go` | subcommands (`login`, `serve`, `tui`, `send`, `logout`), flags, env vars, wiring of limiters/webhook/logger, the loopback-without-token check (`isLoopbackAddr`) |
| `bot.go` | `Bot` type: whatsmeow client, event handling (incl. incoming edits/deletes), sending via `deliver`, replies, reactions, edits, deletes, read receipts, typing, media download (`limitedTransport` 100 MB cap), recipient resolution and the lookup limiter, presence (`-online`), contacts, auto-replies, DB file permissions, pub/sub |
| `store.go` | `wabot_messages` table and its migration from the old rowid schema, `Message`/`Media`/`Chat` types, `Filter` (shared by `/messages`, `/messages/wait`, `/events`, webhook), chat list, history-sync import, LID relinking |
| `api.go` | HTTP routes, bearer auth, `localOnly` (token-less mode: loopback Host, no cross-site), long-poll, SSE, `/media`, idempotency keys, request logging, error mapping (`writeError`), embedded OpenAPI spec and `/docs` page |
| `webhook.go` | webhook queue and workers, retries with backoff, HMAC signing (`Sign`) |
| `limit.go` | `Limiter`: per-minute and per-chat spacing, `RateLimitError` (429). Three instances: `Bot.Limiter` (sends), `Bot.Lookups` (number checks, presence; 3 × rate), `autoLimit` (auto-replies, drop instead of queue) |
| `audit.go` | `wabot_audit` table, `Bot.Audit`, `AuditLog`, `NewLogger` (slog setup) |
| `tui.go` | Bubble Tea model: chat sidebar, conversation pane, message selection and actions, commands, `tuiHelp`, `safe()` sanitizing of untrusted text |
| `docs/openapi.yaml` | OpenAPI 3 spec, embedded into the binary with `//go:embed` (rebuild after editing) |
| `docs/*.md` | user docs: CLI, API, TUI, writing bot logic, admin panel plan |

All code is in `package main`. Keep it that way unless the project grows a lot.

## Build and check

```sh
go build -o wabot .
go vet ./...
gofmt -l .        # must print nothing
```

No cgo: SQLite is `modernc.org/sqlite`, registered as `"sqlite"` and handed to whatsmeow with the `"sqlite3"` dialect.
Don't switch to `mattn/go-sqlite3`.

There's no test suite. To check TUI layout without a terminal or a WhatsApp login, write a throwaway
`zz_*_test.go` (ignored by `.gitignore`) that calls `NewBot` on a scratch DB, inserts rows with `saveMessage`, builds a `model`, sends a
`tea.WindowSizeMsg`, and prints `View()`. Delete the file afterwards.

## Conventions

- **Few comments, more docs.** Code comments only for non-obvious "why". Explanations go in `docs/*.md`.
- **Keep the docs in sync.** A change to an endpoint means updating `api.go`, `docs/openapi.yaml` *and*
  `docs/API.md`. A TUI key or command means updating `tuiHelp` in `tui.go` and `docs/TUI.md`. A flag means
  updating `usage` in `main.go`, `docs/CLI.md` and the README table.
- Errors from the API are always `{"error": "..."}` via `writeError` (401/403 come from the auth
  middleware), which maps errors to codes:
  `inputError` (build it with `badInput(...)`) → 400, `errNotFound` → 404, `RateLimitError` → 429 with
  `Retry-After`, context cancel/deadline → 504, anything else → 502 (WhatsApp failure). Return
  `badInput` from `Bot` methods for caller mistakes so the API, TUI and CLI all report them the same way.
- **Everything that sends a message goes through `Bot.deliver`** (or `Bot.sendControl` for edits and
  revokes, which don't create a stored message), so the rate limiter applies. Never call
  `b.Client.SendMessage` directly from new code paths. The limits exist because bursts get numbers banned.
- **Number lookups and presence updates go through `Bot.lookup`** (`Resolve` on a cache miss, `/check`,
  `SetTyping`). Mass `IsOnWhatsApp` calls are another ban trigger.
- Auto-replies run in a goroutine (`go b.autoReply`) and are budgeted by `autoLimit`; never block the
  whatsmeow event handler.
- Presence: default is "unavailable" so the owner's phone keeps notifications. Use `chatPresence` for
  typing indicators; it goes available and back when `-online` is off.
- **Every control action calls `Bot.Audit(actor, action, target, detail, err)`** once, after the attempt,
  including attempts that fail before anything is sent.
  `actor` is `api`, `tui`, `cli`, `autoreply` or `whatsapp`; pass it into `Bot` methods rather than
  guessing. Never put message text, tokens or secrets in `detail` or in log attributes. New actions go in
  the tables in `docs/ADMIN-PANEL.md` and the `AuditEntry` enum in `docs/openapi.yaml`.
- Logging is `log/slog` via `b.Log`. Errors go in an `err` attribute. Don't log query strings (the token
  can be there).
- New events for subscribers go through `Bot.publish`. The SSE handler forwards `Message` (as `message`,
  read back from the DB so none are lost), `MessageUpdate` (as `update`) and `Status` (as `status`).
  `ChatsChanged` isn't forwarded but triggers the same DB catch-up as `Message`; other types are dropped.
  A numeric `Last-Event-ID` beats `?since=`, so `EventSource` reconnects resume correctly.
- Messages are only published when `saveMessage` actually inserted them (`Bot.store`). Incoming ones
  (`Bot.handle`) then also go to the webhook; messages wabot sends itself don't. `seq` is an explicit
  `INTEGER PRIMARY KEY AUTOINCREMENT` column; never reuse or renumber it.
- Fill `raw`, `rawChat` and `rawSender` when building a `Message` to store: quoting, reactions, deletes,
  read receipts and media download all need the original protobuf and the JIDs as WhatsApp sent them.
  `applyDelete` clears `raw` on purpose.
- Incoming edits/revokes (`handleProtocol`) are applied only when they come from the original author, or
  are a delete by someone who is actually a group admin (checked against the cached participant list in
  `Bot.group`). Keep that check; otherwise anyone could rewrite stored messages.
- Classify message variants in one place (`classify` in `bot.go`).
- Schema changes to `wabot_messages`: new columns only need to be added to `messagesTable`
  (`addMissingColumns` adds them to existing databases on startup, so give them a DEFAULT). Anything else
  needs an upgrade step in `migrate` that keeps the `seq` numbers.
- The TUI is designed for desktop terminals (~100+ columns). Don't spend effort on narrow layouts.
- **Don't block in `Update`.** Network and DB calls go in a `tea.Cmd` that returns a message. Capture the
  values a `Cmd` needs (chat, message) before returning it instead of reading or changing `m` inside the
  goroutine. Chat loads carry `openSeq` so only the latest one is applied.
- Pass any text that strangers control (message text, push names, file names, group names) through `safe()`
  before rendering it in the TUI.

## WhatsApp gotchas (learned the hard way)

- **LIDs.** WhatsApp may identify a user by a hidden `…@lid` ID instead of a phone JID, especially for
  messages sent from the user's own phone. `toMessage` prefers `SenderAlt` / `RecipientAlt`, `phoneJID`
  maps through `Store.LIDs`, and `relinkLIDs` fixes stored rows once the mapping is learned. Any new code
  that stores or compares chat JIDs must go through the same normalization, or chats split in two.
- **Phone numbers aren't canonical.** Always send via `Bot.Resolve`, which asks WhatsApp (`IsOnWhatsApp`)
  for the real JID. For example, Brazilian numbers may be registered with or without the 9th digit.
- **Before login, `Store.Contacts` and `Store.LIDs` are nil.** Guard every use.
- **One connection per session.** Two processes on the same DB kick each other off. `tui -addr` exists
  so the TUI and the API can share one process.
- whatsmeow logs go to stdout. In `tui` and `send` modes the whatsmeow logger is `waLog.Noop` and the app
  logger writes to `io.Discard` (unless `-log-file` is set) so they don't break the screen.
- Use whatsmeow's current API (context-first signatures: `sqlstore.New(ctx, …)`, `GetFirstDevice(ctx)`,
  `GetQRChannel(ctx)`). Check the module source in `go env GOMODCACHE` rather than relying on old examples.

## Safety and privacy

- **Never commit `wabot.db*`.** It holds the account's encryption keys and message history. `.gitignore`
  covers it; don't weaken that.
- **No real data in the repo.** Docs and examples use fake numbers (`5511999999999`, `5511988887777`,
  `552187654321`) and fake names (Maria, John, Ana). Before committing, grep the staged diff for real phone
  numbers, names and tokens.
- Don't run `wabot` against the user's real `wabot.db` while they may have it open. Copy it to a scratch
  directory to inspect it, and treat message contents as private.
- The bot can message real people. Don't send test messages to anyone except the account owner's own
  number, and only when asked.
- The API must stay token-protected when `-token` is set. Only `/docs` and `/openapi.yaml` are public.
  Without a token, keep both guards: `serve` refuses non-loopback addresses, and `localOnly` rejects
  non-loopback `Host` and cross-site requests (403).
- `/media/{seq}` serves files received from other people: keep the inline allow-list (`inlineSafe`: images
  except SVG, audio, video, PDF; everything else as an `attachment`), `nosniff`, `CSP: sandbox;
  default-src 'none'` and `Referrer-Policy: no-referrer`. The 100 MB cap counts actual bytes.
- The DB and its `-wal`/`-shm` files are chmod 0600 at startup; files saved from the TUI are 0600 and never
  overwrite existing ones.
- Never log the webhook URL (it may carry a secret) or the request query string.
- Webhook signing is HMAC-SHA256 over `"<timestamp>.<body>"`. Changing that breaks every receiver; if it
  must change, add a new header version instead.

## Commits

Small, focused commits with a plain summary line. The repo is public under MIT.
