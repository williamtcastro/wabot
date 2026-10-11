# AGENTS.md

Guidance for AI coding agents working on this repo. Human-facing docs are in `README.md` and `docs/`.

## What this is

`wabot` is a single Go binary that links to a WhatsApp account through
[whatsmeow](https://github.com/tulir/whatsmeow) and exposes it three ways: an HTTP API, a Bubble Tea
TUI and a one-shot CLI. All state lives in one SQLite file (default `wabot.db`).

## Layout

| File | Responsibility |
|---|---|
| `main.go` | subcommands (`login`, `serve`, `tui`, `send`, `logout`), flags, env vars |
| `bot.go` | `Bot` type: whatsmeow client, event handling, sending, recipient resolution, contacts, auto-replies, pub/sub |
| `store.go` | `wabot_messages` table, message history, chat list, history-sync import, LID relinking |
| `api.go` | HTTP routes, bearer auth, SSE, embedded OpenAPI spec and `/docs` page |
| `tui.go` | Bubble Tea model: chat sidebar, message pane, commands |
| `docs/openapi.yaml` | OpenAPI 3 spec, embedded into the binary with `//go:embed` |
| `docs/*.md` | user docs: CLI, API, TUI, writing bot logic |

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
`_test.go` that calls `NewBot` on a scratch DB, inserts rows with `saveMessage`, builds a `model`, sends a
`tea.WindowSizeMsg`, and prints `View()`. Delete the file afterwards.

## Conventions

- **Few comments, more docs.** Code comments only for non-obvious "why". Explanations go in `docs/*.md`.
- **Keep the docs in sync.** A change to an endpoint means updating `api.go`, `docs/openapi.yaml` *and*
  `docs/API.md`. A TUI key or command means updating `tuiHelp` in `tui.go` and `docs/TUI.md`. A flag means
  updating `usage` in `main.go`, `docs/CLI.md` and the README table.
- Errors from the API are always `{"error": "..."}` via `writeErr`. Use 400 for bad input and 502 for
  WhatsApp failures.
- New events for subscribers go through `Bot.publish`. The SSE handler only forwards `Message` and `Status`
  and drops other types, so internal signals like `ChatsChanged` stay internal.
- The TUI is designed for desktop terminals (~100+ columns). Don't spend effort on narrow layouts.
- Don't block in `Update`: network calls go in a `tea.Cmd` that returns a message.

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
- whatsmeow logs go to stdout. In `tui` and `send` modes the logger is `waLog.Noop` so it doesn't break the screen.
- Use whatsmeow's current API (context-first signatures: `sqlstore.New(ctx, …)`, `GetFirstDevice(ctx)`,
  `GetQRChannel(ctx)`). Check the module source in `go env GOMODCACHE` rather than relying on old examples.

## Safety and privacy

- **Never commit `wabot.db*`.** It holds the account's encryption keys and message history. `.gitignore`
  covers it; don't weaken that.
- **No real data in the repo.** Docs and examples use fake numbers (`5511999999999`, `5511988887777`,
  `552187654321`) and fake names (Maria, John). Before committing, grep the staged diff for real phone
  numbers, names and tokens.
- Don't run `wabot` against the user's real `wabot.db` while they may have it open. Copy it to a scratch
  directory to inspect it, and treat message contents as private.
- The bot can message real people. Don't send test messages to anyone except the account owner's own
  number, and only when asked.
- The API must stay token-protected when `-token` is set. Only `/docs` and `/openapi.yaml` are public.

## Commits

Small, focused commits with a plain summary line. The repo is public under MIT.
