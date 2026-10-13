# AGENTS.md

Guidance for AI coding agents working on this repo. Human-facing docs are in `README.md` and `docs/`.

## What this is

`wame` (WhatsApp manager: TUI & bot, formerly `wabot`) is a single Go binary that links to a WhatsApp
account through [whatsmeow](https://github.com/tulir/whatsmeow) and exposes it as an HTTP API, an MCP
server, a Bubble Tea TUI and a one-shot CLI. All state lives in one SQLite file (default
`~/.config/wame/wame.db`).

**Kept from the old name, on purpose:** SQLite tables stay `wabot_*` (renaming them would need a
migration of live databases), `WABOT_*` env vars are read as a fallback (`envOr`), a pre-rename
`~/.config/wabot/wabot.db` is used when no `wame.db` exists (`defaultDB`), and `wabot_` tokens still
authenticate (`auth.legacyPrefix`). Don't "finish the rename" on these.

## Layout

Go's "server project" layout: the `wame` command at the root, everything else under `internal/`.

| Path | Package | Responsibility |
|---|---|---|
| `main.go` | `main` | subcommands (`login`, `serve`, `tui`, `send`, `token`, `mcp`, `version`, `logout`), flags, env vars, `usage`, the default DB path (`defaultDB`, `legacyDBNotice`), `newLogger` (slog setup), wiring of limiters/webhook, the startup guard `guardPublicAddr` (owner secret ≥ 16 chars; non-loopback needs an owner secret or any token ever created; `isLoopbackAddr`), `tokenCommand`, `mcpCommand` (stdio MCP), resolving a `-webhook-filter` chat number after login (`resolveWebhookChat`) |
| `docs/docs.go` | `docs` | `//go:embed openapi.yaml` as `docs.OpenAPI` (embed can't reach a parent directory, so it lives next to the file) |
| `internal/store/store.go` | `store` | `Store` (wraps `*sql.DB`, owns the LID relink cache key), `Open`, `wabot_messages` table and its migration from the old rowid schema, `Save`, `Import` (history sync, incl. edits), `Query`/`Messages`/`Get`/`GetBySeq`/`LastSeq`, `MarkEdited`/`MarkDeleted`, `RelinkLIDs`, `Chats` |
| `internal/store/message.go` | `store` | `Message` (raw fields exported but `json:"-"`), `Media`, `MessageUpdate`, `Chat`, `ChatsChanged`, `ErrNotFound`, `Message.MediaFileName` |
| `internal/store/filter.go` | `store` | `Filter`, `ParseFilter`, `Match` and its SQL twin `where` (shared by `/messages`, `/messages/wait`, `/events`, webhook), including the `q` text search (`textMatches` in Go mirrors the FTS query) |
| `internal/store/state.go` | `store` | `migrateState`: `wabot_chat_state` (read positions, `MarkReadUpTo`/`MarkChatRead`/`ReadSeq`), `wabot_groups` (`SaveGroups`/`Groups`), the `wabot_messages_fts` index and its triggers, `ftsQuery`; the `ChatRead` event |
| `internal/store/audit.go` | `store` | `wabot_audit` table, `AuditEntry`, `Store.Audit`, `Store.AuditLog` |
| `internal/store/settings.go` | `store` | `wabot_settings` (runtime overrides: key, value, updated_by, updated_at), `Settings`, `SetSettings` (nil deletes) |
| `internal/store/tokens.go` | `store` | `wabot_tokens` table, `Token`, `InsertToken` (`ErrTokenName` / `ErrTokenID`), `TokenByID`, `Tokens`, `RevokeToken` (cascades to tokens it created), `TouchToken`, `HasTokens` (any row ever) |
| `internal/auth/auth.go` | `auth` | `Scope` (`read`/`send`/`admin`), `Principal` (`Has`, `Restricted`), `Owner()`, `Issue` (token format, reserved names, `DefaultRate`, child expiry), `Authenticator` (`Required` fails closed, `Authenticate`), `CheckOwnerSecret` (`MinOwnerSecret` = 16) |
| `internal/bot/bot.go` | `bot` | `Bot` type, `New`, `Start`, `PairPhone`, `Status`, pub/sub (`Subscribe`, `publish`), `InputError`/`BadInput`, `SleepUntil`, DB file permissions |
| `internal/bot/events.go` | `bot` | whatsmeow event handler, incoming edits/deletes (`handleProtocol`), history import, auto-replies |
| `internal/bot/message.go` | `bot` | `classify`, `toMessage`, `PhoneJID`, `save` (store + publish) |
| `internal/bot/send.go` | `bot` | `SendText`, `SendFile`, replies (`prepare`, `quote`), `deliver`, `sendControl`, throttling |
| `internal/bot/actions.go` | `bot` | `React`, `Edit`, `Delete`, `MarkRead`, `SetTyping` |
| `internal/bot/contacts.go` | `bot` | `Contacts`, `Check`, `Resolve`, `ParseRecipient`, the lookup limiter, `Chats`/`ChatName`, the group cache and admin list (`refreshGroups`, `loadCachedGroups`, the `NamesChanged` event) |
| `internal/bot/media.go` | `bot` | `DownloadMedia`, `limitedTransport` / `cappedBody` (100 MB cap) |
| `internal/bot/presence.go` | `bot` | presence (`-online`) and typing indicators |
| `internal/bot/audit.go` | `bot` | `Bot.Audit`, `Bot.audited` |
| `internal/bot/allow.go` | `bot` | chat allowlists for restricted tokens: `LocalChat` (no network), `ChatAllowed`, `ExpandAllowlist` (resolves the list's own phone entries, cached 10 min), `CanonicalChat` (unrestricted callers) |
| `internal/bot/limit.go` | `bot` | `Limiter`: per-minute and per-chat spacing, `RateLimitError` (429), `Set` (limits change at runtime; every read is under the lock), `Stats`, `LookupsPerMinute`. Three instances: `Bot.Limiter` (sends), `Bot.Lookups` (number checks, presence; 3 × rate), `autoLimit` (auto-replies, drop instead of queue) |
| `internal/bot/webhook.go` | `bot` | webhook queue and workers, retries with backoff, HMAC signing (`Sign`), `WebhookStats` (in memory), `SetFilter`/`SetChatIf`, `TestWebhook`, `ReplayWebhook` (only ids in the recent failures) |
| `internal/bot/settings.go` | `bot` | runtime settings: `Settings`, `SettingKeys`, `InitSettings` (flags + stored overrides), `UpdateSettings` (validate all, store, apply, audit once), `SettingsView`, `ParseWebhookFilter`, the auto-reply switch (`SetAutoReply`/`AutoReplyOn`, atomic) |
| `internal/bot/usage.go` | `bot` | per-caller usage in hourly buckets for 24 h (`RecordRequest` from the API/MCP auth, sends from `audited`), `Usage` |
| `internal/api/api.go` | `api` | `NewHandler` (the route table: every route wrapped in `need(scope, …)`), handlers, long-poll, SSE (with token re-checks), `/media`, `/mcp` mount, the `/docs` page |
| `internal/api/authz.go` | `api` | `withAuth` (header/`?token=` rules, failed-login throttle), `need`, `unrestricted`, `visibleStatus`, `authorizeChat`, `restrictFilter`, `sendGuard` (allowlist + per-token rate), stream cap and `stillValid`, `/tokens` handlers, `NormalizeChats` |
| `internal/api/mcp.go` | `api` | `/mcp`: streamable HTTP MCP handler; bearer verification puts the `Principal` in the SDK's `TokenInfo`; token-less mode via `localOnly` |
| `internal/api/middleware.go` | `api` | `localOnly` (token-less mode: loopback RemoteAddr and Host, no cross-site), request logging |
| `internal/mcpserver/server.go` | `mcpserver` | MCP tools (`status`, `list_chats`, `read_messages`, `search_messages`, `find_contact`, `send_message`, `react`, `mark_read`), server instructions, `require`/`chat` checks, `clean()` of untrusted text |
| `internal/api/idempotency.go` | `api` | idempotency keys (bound to a request fingerprint; reuse for a different request → 409 `errConflict`) |
| `internal/api/respond.go` | `api` | `readJSON`, `writeJSON`, error mapping (`writeError`) |
| `internal/tui/model.go` | `tui` | `Run`, the Bubble Tea model, `Update`, loading chats and messages |
| `internal/tui/keys.go` | `tui` | key routing (`key`), focus, per-panel keys |
| `internal/tui/commands.go` | `tui` | `tuiHelp`, `tuiCommands`, slash commands, sending, saving media |
| `internal/tui/complete.go` | `tui` | autocomplete (`suggest`, `pathSuggestions`, `chatTarget`) |
| `internal/tui/view.go` | `tui` | rendering: panes, sidebar, context bar, footer, styles |
| `internal/tui/format.go` | `tui` | text helpers and `safe()` sanitizing of untrusted text |
| `internal/admin/client.go` | `admin` | `wame admin`: HTTP client for a remote server (`NewClient` refuses plain http beyond loopback), typed on `bot.Status`, `store.Token`, `store.AuditEntry` |
| `internal/admin/config.go` | `admin` | `ReadEnvFile` (`WAME_SERVER`/`WAME_TOKEN` from `./.env`, `~/.config/wame/admin.env`) and `Resolve` (precedence; a file's server needs that file's token) |
| `internal/admin/model.go` | `admin` | the panel's Bubble Tea model: `Options`, tabs, modes, polling, `call` (every server answer is tagged with the client generation in a `genMsg`, so answers from a server the user switched away from are dropped), `applyStatus` (bell on a healthy → unhealthy change), server switching |
| `internal/admin/live.go` | `admin` | the `/events` follower: reconnects with backoff, feeds `liveMsg`/`eventMsg` through a channel closed on stop; keeps time/chat/sender (never text) of the last hour's messages for the counts |
| `internal/admin/chats.go` | `admin` | Chats tab: list, messages, search, sending, and `safe()` for text other people wrote |
| `internal/admin/tokens.go` | `admin` | Tokens tab keys, the create form, rotation (`nextName`, `rotationRequest`, create-then-revoke) and `unused` |
| `internal/admin/health.go` | `admin` | Health tab (`GET /stats`), meters, and `grid`: panels laid out in equal-height rows |
| `internal/admin/view.go` | `admin` | rendering: header (degrades by width), dialogs, the Status grid, tokens and audit tables |
| `Dockerfile`, `compose.yaml` | | static build into distroless `nonroot`; `/data` volume with `WAME_DB=/data/wame.db`, `WAME_ADDR=0.0.0.0:8080` (so a token is required). CI builds it and smoke-tests `version` and the no-token refusal. Docs: `docs/DOCKER.md` |
| `_demo/` | `main` | README screenshot: seeds a scratch DB with fake chats and opens the TUI without connecting; `screenshot.sh` records it with VHS into `docs/wame-tui.png`. Ignored by `./...`, built by CI. Rerun it after visible TUI changes |
| `docs/openapi.yaml` | | OpenAPI 3 spec, embedded into the binary (rebuild after editing) |
| `docs/*.md` | | user docs: CLI, API, TUI, MCP, writing bot logic |
| `docs/security/` | | security reviews (`SCOPED-TOKENS.md`: token system threat model, findings and fixes) |
| `docs/proposed-features/` | | deferred proposals (admin panel, TUI image previews); nothing there is built |
| `.github/workflows/ci.yml` | | CI on every push to `main` and every PR: gofmt, `go vet`, `go test -race` on Ubuntu and macOS, `CGO_ENABLED=0` build |
| `.github/workflows/release.yml`, `.goreleaser.yaml` | | on a `v*` tag: tests, then GoReleaser builds linux/darwin × amd64/arm64 `tar.gz` archives plus `checksums.txt` on the GitHub release, stamping `main.version` via `-ldflags` |

**Dependency direction:** `main` → `api`, `mcpserver`, `auth`, `tui`, `bot`, `store`; `api` → `mcpserver`,
`auth`, `bot`, `store`, `docs`; `mcpserver` → `auth`, `bot`, `store`; `tui` → `bot`, `store`; `auth` →
`store`; `bot` → `store`; `store` → nothing internal. Never import upwards (Go
rejects cycles anyway). `store` knows nothing about whatsmeow's client: it deals in plain strings.

**Where new code goes:**

- SQL, schema, filters, anything that reads or writes `wabot_*` tables: `internal/store`.
- Anything that talks to WhatsApp, enforces limits, audits or publishes events: `internal/bot`,
  in the file that matches (a new action goes in `actions.go`, a new event in `events.go`).
- HTTP concerns (parsing requests, status codes, headers, scopes per route): `internal/api`. Handlers
  stay thin and call `Bot` methods.
- Token issuing and verification: `internal/auth`. MCP tools: `internal/mcpserver`.
- Screen and keys: `internal/tui`.
- Helpers that both `api` and `tui` need belong in the lowest package that makes sense (e.g.
  `store.Message.MediaFileName`), never copied.
- Export only what another package uses; keep the rest unexported. Avoid stutter (`bot.Status`,
  not `bot.BotStatus`).

## Build and check

```sh
go build -o wame .      # or: go install .
go vet ./...
go test -race ./...
gofmt -l .               # must print nothing
```

No cgo: SQLite is `modernc.org/sqlite`, registered as `"sqlite"` and handed to whatsmeow with the `"sqlite3"` dialect.
Don't switch to `mattn/go-sqlite3`.

## Tests

Each package has `_test.go` files in the same package (so they can reach unexported code). They
run offline: scratch databases in `t.TempDir()`, `bot.New` on a fresh DB (no login, no
connection), `events.Message` values built by hand, `httptest` for the API, and the TUI model
driven by feeding it `tea.WindowSizeMsg` / `tea.KeyMsg` and running the returned `Cmd`s
synchronously. Never point a test at a real `wame.db` or connect to WhatsApp. Use only the fake
numbers and names below.

- `internal/store`: v1 → current migration keeps rowids as `seq` and is idempotent, `Filter` SQL vs
  `Match`, `ChatOnly`, edit/delete/import, chats and audit.
- `internal/bot`: `Limiter` (`ReserveN` is all-or-nothing), `ParseRecipient`, `handleProtocol`
  ignoring forged edits and non-admin revokes, `audited`, `cappedBody` / `limitedTransport`, media
  checks, webhook signing and filtering.
- `internal/api`: token auth, token-less `localOnly`, idempotency (replay, 409, panic), error
  mapping, `/messages` cursors, `/events` `Last-Event-ID` replay, scoped tokens and allowlists
  (`TestScopedTokens`, `TestPerTokenRate`, `TestTokenAdminEndpointsAndRevocation`), MCP over HTTP with
  token scopes, and one regression test per security finding in `security_test.go` (`TestF1…`–`TestF10…`).
- `internal/auth`: token issuing/parsing, `TestF2RequiredFailsClosed`.
- `internal/mcpserver`: read-only and restricted callers, cleaning of untrusted text.
- `internal/tui`: 110×24 renders exactly 24 lines (with and without the context bar), autocomplete,
  reaction divider, vim-style keys, half-page scroll, `safe()`.
- `internal/bot` (`settings_test.go`): runtime settings (validation, apply, restart, reset, audit),
  the webhook filter setting vs a late number resolution, webhook test and replay, usage counting.
- `internal/admin`: the client against the real `api.NewHandler` (tokens, audit paging, 401/403
  messages), the http guard, env files and profiles, and the panel: create → copy → revoke →
  filter, rotation, the Chats tab (sanitizing), the live stream, the bell, Health, server switching
  (stale answers dropped) and that Status cards line up at 120 and 180 columns. Tests that build a
  model use `newTestModel`, which stops the live stream on cleanup (otherwise the test server
  can't close).
- `main`: default DB path, `isLoopbackAddr`.
- Also: `internal/store/state_test.go` (read positions, upgrade marks history read, FTS search incl.
  accents/prefix/deleted/edited, group cache) and `internal/bot/state_test.go` (read-self receipts,
  group cache loading); the TUI tests cover the new-messages divider, search jump and badges.

**CI must stay green.** Run `gofmt -l .`, `go vet ./...` and `go test -race ./...` before pushing;
CI runs the same on Linux and macOS.

## Releases

```sh
git tag v0.x.y && git push --tags
```

The `release` workflow runs the tests and then GoReleaser, which publishes the archives and
`checksums.txt` to a GitHub release. The version printed by `wame version` comes from the tag
(without the `v`); local builds print `dev`. Don't edit `version` in `main.go` by hand.

Add a test with every behavior change. To eyeball the TUI, print `ansi.Strip(m.View())` from a
throwaway `zz_*_test.go` (ignored by `.gitignore`) using the helpers in `internal/tui/tui_test.go`.

## Conventions

- **Few comments, more docs.** Code comments only for non-obvious "why". Explanations go in `docs/*.md`.
- **Keep the docs in sync.** A change to an endpoint means updating `internal/api/api.go`, `docs/openapi.yaml` *and*
  `docs/API.md`. A TUI key or command means updating `tuiHelp` in `internal/tui/commands.go`, the
  footer `hints` in `internal/tui/view.go` and `docs/TUI.md` (which quotes `tuiHelp` verbatim). A flag means
  updating `usage` in `main.go`, `docs/CLI.md` and the README table. An admin panel key means
  updating the footer `keys` in `internal/admin/view.go` and `docs/ADMIN.md`. A new runtime setting
  goes in `SettingKeys`, `Settings.get`/`set` and `applySettings` (`internal/bot/settings.go`), the
  `SettingView` key enum and PATCH body in `docs/openapi.yaml`, `docs/API.md` and `docs/CLI.md`. A new TUI command goes in
  `tuiCommands` (autocomplete and hint bar), `tuiHelp` and `docs/TUI.md`.
- Errors from the API are always `{"error": "..."}` via `writeError` (401/403 come from the auth
  middleware), which maps errors to codes:
  `bot.InputError` (build it with `bot.BadInput(...)`) → 400, `store.ErrNotFound` → 404, `errConflict` → 409,
  `bot.RateLimitError` → 429 with `Retry-After`, context cancel/deadline → 504, anything else → 502 (WhatsApp failure). Return
  `BadInput` from `Bot` methods for caller mistakes so the API, TUI and CLI all report them the same way.
- **Everything that sends a message goes through `Bot.deliver`** (or `Bot.sendControl` for edits and
  revokes, which don't create a stored message), so the rate limiter applies. Never call
  `b.Client.SendMessage` directly from new code paths. The limits exist because bursts get numbers banned.
- **Number lookups and presence updates go through `Bot.lookup`** (`Resolve` on a cache miss, `/check`,
  `SetTyping`). Mass `IsOnWhatsApp` calls are another ban trigger.
- Auto-replies run in a goroutine (`go b.autoReply`) and are budgeted by `autoLimit`; never block the
  whatsmeow event handler.
- Presence: default is "unavailable" so the owner's phone keeps notifications. Use `chatPresence` for
  typing indicators; it goes available and back when `-online` is off.
- **Wrap every control action in `Bot.audited(actor, action, target, detail, fn)`.** It runs `fn` and calls
  `Bot.Audit` exactly once with the outcome, so input-validation failures (empty text, not your message,
  edit window) are recorded too. Put the validation inside `fn`, not before it. Call `Bot.Audit` directly
  only for actions that don't return a `Message` (pair, logout, read, token_create, token_revoke,
  settings, webhook_test, webhook_replay). `actor`
  is the caller's token name for API and MCP calls (`principal(r).Name`; `api` for the owner secret and
  token-less mode, `mcp` for stdio without a token), or `tui`, `cli`, `autoreply`, `whatsapp`. Those
  fixed names (plus `owner`, `local`) are reserved so no token can impersonate them. Pass the actor into
  `Bot` methods rather than guessing. Never put message text,
  tokens or secrets in `detail` or in log attributes. New actions go in the tables in
  `docs/proposed-features/ADMIN-PANEL.md` and the `AuditEntry` enum in `docs/openapi.yaml`.
- Logging is `log/slog` via `b.Log`. Errors go in an `err` attribute. Don't log query strings (the token
  can be there).
- Events published today: `store.Message`, `store.MessageUpdate`, `bot.Status`, `store.ChatsChanged`
  (history import), `store.ChatRead` (a chat's read position moved: TUI open, `/read`, read-self
  receipts) and `bot.NamesChanged` (group names refreshed). The TUI reloads the chat list on the
  last two.
- New events for subscribers go through `Bot.publish`. The SSE handler forwards `store.Message` (as `message`,
  read back from the DB so none are lost), `store.MessageUpdate` (as `update`) and `bot.Status` (as `status`).
  `store.ChatsChanged` isn't forwarded but triggers the same DB catch-up as `Message`; other types are dropped.
  A numeric `Last-Event-ID` beats `?since=`, so `EventSource` reconnects resume correctly.
- Messages are only published when `Store.Save` actually inserted them (`Bot.save`). Incoming ones
  (`Bot.handle`) then also go to the webhook; messages wame sends itself don't. `seq` is an explicit
  `INTEGER PRIMARY KEY AUTOINCREMENT` column; never reuse or renumber it.
- Fill `Raw`, `RawChat` and `RawSender` when building a `store.Message` to store: quoting, reactions, deletes,
  read receipts and media download all need the original protobuf and the JIDs as WhatsApp sent them.
  They're tagged `json:"-"` so they never reach API clients or webhooks. `Store.MarkDeleted` clears `raw` on purpose.
- Incoming edits/revokes (`handleProtocol`) are applied only when they come from the original author, or
  are a delete by someone who is actually a group admin (checked against the cached participant list in
  `Bot.group`). Keep that check; otherwise anyone could rewrite stored messages.
- Classify message variants in one place (`classify` in `internal/bot/message.go`).
- Schema changes to `wabot_messages`: new columns only need to be added to `messagesTable` (`internal/store/store.go`)
  (`addMissingColumns` adds them to existing databases on startup, so give them a DEFAULT). Anything else
  needs an upgrade step in `migrate` that keeps the `seq` numbers. Other tables go through
  `migrateState` (`internal/store/state.go`) or `migrateAudit`: create them idempotently and handle the
  first-run backfill there (as `wabot_chat_state` does: existing history counts as read). Upgrades must
  run automatically on startup; there is no separate migrate command.
- Tables: `wabot_messages`, `wabot_messages_fts` (FTS5 over `text`, external content, kept in sync by
  the `wabot_messages_fts_insert/_delete/_update` triggers, so write `text` only through normal
  UPDATE/INSERT), `wabot_chat_state`, `wabot_groups`, `wabot_audit`, `wabot_tokens`, `wabot_settings`. Unread counts and search must
  agree with `Filter.Match` / `textMatches`, which the live paths use.
- The TUI is designed for desktop terminals (~100+ columns). Don't spend effort on narrow layouts.
- **Don't block in `Update`.** Network and DB calls go in a `tea.Cmd` that returns a message. Capture the
  values a `Cmd` needs (chat, message) before returning it instead of reading or changing `m` inside the
  goroutine. Chat loads carry `openSeq` so only the latest one is applied.
- Pass any text that strangers control (message text, push names, file names, group names) through `safe()`
  before rendering it in the TUI.

## WhatsApp gotchas (learned the hard way)

- **LIDs.** WhatsApp may identify a user by a hidden `…@lid` ID instead of a phone JID, especially for
  messages sent from the user's own phone. `toMessage` prefers `SenderAlt` / `RecipientAlt`, `Bot.PhoneJID`
  maps through whatsmeow's `Client.Store.LIDs`, and `store.Store.RelinkLIDs` fixes stored rows once the mapping is learned. Any new code
  that stores or compares chat JIDs must go through the same normalization, or chats split in two.
- **Phone numbers aren't canonical.** Always send via `Bot.Resolve`, which asks WhatsApp (`IsOnWhatsApp`)
  for the real JID. For example, Brazilian numbers may be registered with or without the 9th digit.
- **Before login, `Client.Store.Contacts` and `Client.Store.LIDs` are nil.** Guard every use.
- **One connection per session.** Two processes on the same DB kick each other off. `tui -addr` exists
  so the TUI and the API can share one process.
- whatsmeow logs go to stdout. In `tui` and `send` modes the whatsmeow logger is `waLog.Noop` and the app
  logger writes to `io.Discard` (unless `-log-file` is set) so they don't break the screen.
- Use whatsmeow's current API (context-first signatures: `sqlstore.New(ctx, …)`, `GetFirstDevice(ctx)`,
  `GetQRChannel(ctx)`). Check the module source in `go env GOMODCACHE` rather than relying on old examples.

## Safety and privacy

- **Never commit `wame.db*`.** (The default location is now `~/.config/wame/`, but older checkouts may still have one in the repo directory.) It holds the account's encryption keys and message history. `.gitignore`
  covers it; don't weaken that.
- **No real data in the repo.** Docs and examples use fake numbers (`5511999999999`, `5511988887777`,
  `552187654321`) and fake names (Maria, John, Ana). Before committing, grep the staged diff for real phone
  numbers, names and tokens.
- Don't run `wame` against the user's real `wame.db` while they may have it open. Copy it to a scratch
  directory to inspect it, and treat message contents as private.
- The bot can message real people. Don't send test messages to anyone except the account owner's own
  number, and only when asked.
- **Every new route is wrapped in `need(scope, …)`** in `NewHandler`, and must handle tokens limited to
  chats: check targets with `authorizeChat` / `sendGuard` (sends also get the per-token rate), limit
  queries with `restrictFilter`, or wrap the route in `unrestricted` if it can reveal data beyond the
  allowlist. Add it to the route table in `docs/API.md` and to `docs/openapi.yaml`.
- **Never resolve a restricted caller's input on WhatsApp.** Use `Bot.LocalChat` + `Bot.ChatAllowed`
  (or `ExpandAllowlist`); `Resolve` / `CanonicalChat` are for unrestricted callers only. Error text for
  chats outside the list stays fixed ("this token can't access that chat") so it can't be used as an
  oracle.
- Tokens are required forever once any row exists in `wabot_tokens`, and `Required()` fails closed. Don't
  add any path back to token-less mode. Token-less `localOnly` checks loopback `RemoteAddr`, loopback
  `Host` and same-origin; keep all three. Only `/docs` and `/openapi.yaml` are public.
- Tokens can't mint `admin` tokens over the API; children carry `created_by`, can't outlive the parent and
  are revoked with it. Long-lived handlers (`/events`, `/messages/wait`) must re-check `stillValid` and
  respect the per-token stream cap. `?token=` is accepted only on `GET /events`, `/qr`, `/media/{seq}`.
- **New MCP tools** call `require(req, scope)` (which also re-checks the token is still valid) and, for
  any chat argument, `chat()` (allowlist, no lookup for restricted callers); send-type tools use
  `sendTarget`, which adds the per-token rate (`Bot.ThrottleToken`, shared with the HTTP API). Anything written by third parties (text, names, captions, file names) goes
  under an `untrusted` field and through `clean()`. Mark read-only tools `ReadOnlyHint` and sending ones
  open-world / destructive.
- `wame mcp` speaks the protocol on **stdout**: never print to stdout in that mode (logs go to stderr,
  whatsmeow logging stays off).
- See [docs/security/SCOPED-TOKENS.md](docs/security/SCOPED-TOKENS.md) for the threat model and the
  findings these rules come from; add a regression test for any new auth behaviour.
- `/media/{seq}` serves files received from other people: keep the inline allow-list (`inlineSafe`: images
  except SVG, audio, video, PDF; everything else as an `attachment`), `nosniff`, `CSP: sandbox;
  default-src 'none'` and `Referrer-Policy: no-referrer`. The 100 MB cap counts actual bytes.
- The DB and its `-wal`/`-shm` files are chmod 0600 at startup; files saved from the TUI are 0600 and never
  overwrite existing ones.
- Never log the webhook URL (it may carry a secret) or the request query string.
- `/docs` loads `swagger-ui-dist` from jsDelivr with Subresource Integrity hashes (`docsPage` in `internal/api/api.go`).
  Bumping the version means recomputing both `integrity` attributes, e.g.
  `curl -fsSL URL | openssl dgst -sha384 -binary | openssl base64 -A`, otherwise the page won't load.
- Webhook signing is HMAC-SHA256 over `"<timestamp>.<body>"`. Changing that breaks every receiver; if it
  must change, add a new header version instead.

## Commits

Small, focused commits with a plain summary line. The repo is public under MIT.
