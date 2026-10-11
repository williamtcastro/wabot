# Admin panel (plan)

Status: **not built**. This is a plan for a small web admin panel served by `wabot serve`, and a
description of the logging and audit groundwork that already exists for it.

## Goals

- See at a glance whether the bot is connected, and log in (QR or pairing code) from a browser.
- Read chats and messages, and send a message, without the TUI.
- See who did what: the audit log, filterable.
- See whether the webhook is healthy and whether sends are being throttled.
- See the effective configuration.
- One binary, no build step, no extra services.

## Non-goals

- A full WhatsApp Web clone (no media gallery, no group management, no rich composer).
- Multi-account or multi-tenant support. One process is one WhatsApp session.
- Editing configuration that needs a restart (DB path, listen address) from the browser.
- Mobile layout. Desktop browsers only, like the TUI.
- Exposing it to the internet without a reverse proxy and TLS.

## Pages

| Page | Shows / does | Built on |
|---|---|---|
| **Status & login** | state, JID, push name, connection dot; QR image that refreshes, pairing-code form, logout button | `GET /status`, `GET /qr`, `POST /pair`, `POST /logout`, `status` SSE events |
| **Chats & messages** | chat list with last activity; a chat's messages, live; media links; edited/deleted markers | `GET /chats`, `GET /messages?chat=…`, `GET /events?chat=…`, `GET /media/{seq}` |
| **Send** | recipient, text, optional reply-to and file; shows the result or the 429 wait | `POST /send`, `POST /send/file` (with an `Idempotency-Key` per form submit) |
| **Audit log** | table of actions; filters by actor, action, target, ok/failed, time range; paging | `GET /audit` (needs filters, see below) |
| **Webhook health** | URL (secret masked), filter, queue depth, last success/failure, recent failed deliveries with status and attempt count | needs a delivery log (see below) |
| **Rate limit** | configured `-rate`, `-chat-gap`, `-max-wait`; for the send, lookup (3 × rate) and auto-reply (6/min) limiters: usage in the last minute, current wait for the next slot, recent 429s / dropped auto-replies | needs a limiter snapshot (see below) |
| **Settings** | effective flags and their source (flag / env / default), secrets masked; later, editable runtime settings | needs a config snapshot (see below) |
| **Logs** (optional) | recent app log lines, level filter | needs a log ring buffer (see below) |

## What already exists

- **Read and control endpoints:** `/status`, `/qr`, `/pair`, `/logout`, `/chats`, `/messages`,
  `/messages/wait`, `/events`, `/media/{seq}`, `/send`, `/send/file`, `/react`, `/edit`,
  `/delete`, `/read`. The panel can call these from the browser with no new backend code.
- **Live updates:** `/events` streams `message`, `update` and `status` and resumes with
  `Last-Event-ID`, so the panel can use a plain `EventSource`.
- **Audit:** every control action is stored in `wabot_audit` and served by `GET /audit`.
- **Structured logs:** `log/slog`, text or JSON, one line per HTTP request and per action.

## What's missing

1. **Webhook delivery log.** Today delivery results only go to the log. Add a
   `wabot_webhook_deliveries` table (`delivery`, `event`, `seq`, `attempt`, `status`, `error`,
   `at`, `done`), written by `Webhook.send`, pruned to the last N days, served by
   `GET /admin/webhook` together with queue length (`len(w.queue)`) and capacity.
2. **Limiter snapshot.** A `Limiter.Snapshot()` returning reservations in the last minute, the
   next free slot, and a counter of `RateLimitError`s, for `Bot.Limiter`, `Bot.Lookups` and the
   auto-reply limiter; served by `GET /admin/limits`.
3. **Config snapshot.** Keep the parsed `config` on the `Bot` (or pass it to the panel) and serve
   it with secrets (`token`, `webhookSecret`) replaced by `"set"` / `""`, plus where each value
   came from.
4. **Log ring buffer.** A `slog.Handler` that tees the last ~1000 records into memory, served by
   `GET /admin/logs?level=&after=`. Attributes are already safe to show (see conventions below).
5. **Audit filters.** `GET /audit` only has `limit` and `before`. Add `actor`, `action`, `target`,
   `ok`, `from`, `to` query parameters, and an index on `(action, at)` if it gets slow.
6. **Admin auth, separate from the API token.** The API token is meant for scripts and is
   accepted in the query string. The panel should instead use:
   - a login form (password from `-admin-password` / `WABOT_ADMIN_PASSWORD`, stored hashed if
     persisted), creating a server-side session with an `HttpOnly`, `SameSite=Strict`,
     `Secure`-when-TLS cookie;
   - a CSRF token on every state-changing request (double-submit or per-session token);
   - session expiry and a logout button;
   - the API token keeps working for `/` routes; panel routes live under `/admin/` and accept
     only the session cookie.
7. **Read-only role.** A second password (or user list) for viewers who can see status, chats and
   audit but can't send, delete, pair or log out. Enforced server-side per route, and recorded as
   the audit `actor` (e.g. `admin:maria`, `viewer:john`).
8. **Settings persistence.** If runtime-editable settings are added (rate limit, chat gap,
   auto-reply on/off, webhook filter), store them in a `wabot_settings` key/value table that
   overrides flags on startup, and audit every change (`action=settings`, `detail` = key names,
   never values of secrets).

## Logging today

### App log (`b.Log`, `*slog.Logger`)

Configured by `-log-format text|json`, `-log-file PATH` (stderr otherwise) and `-debug`
(debug level, else info). Discarded in `tui` and `send` modes unless `-log-file` is set.

| Message | Level | Attributes |
|---|---|---|
| `http` | info; debug for `/status`, `/messages/wait`, `/events`; warn for 4xx; error for 5xx | `method`, `path`, `status`, `duration_ms`, `remote` (no query string: it may hold the token) |
| `action` | info | `actor`, `action`, `target`, `detail` |
| `action failed` | warn | same, plus `err` |
| `audit write failed` | error | same, plus `err` |
| `connection state` | info | `state` |
| `login failed` | error | `event`, `err` |
| `save message` | error | `chat`, `id`, `err` |
| `auto-reply failed` | warn | `chat`, `err` |
| `auto-reply dropped` | debug | `chat`, `err` (over the auto-reply budget) |
| `set presence failed` | warn | `err` |
| `delivery failed` | warn | `component=webhook`, `delivery`, `attempt`, `status`, `err`, `will_retry` (the URL is never logged) |
| `delivered after retry` | info | `component=webhook`, `delivery`, `attempt` |
| `giving up on delivery` | error | `component=webhook`, `delivery`, `attempts` |
| `queue full, dropping delivery` | error | `component=webhook`, `delivery` |
| `webhook filter chat resolved` | info | `chat`, `jid` |
| `webhook filter: could not resolve chat, keeping it as typed` | warn | `chat`, `err` |
| `API has no token: only local, same-origin requests are accepted` | warn | `addr` |
| `HTTP API listening` | info | `addr`, `docs` |
| `HTTP API stopped` | error | `err` (`tui -addr` only) |

whatsmeow's own logs are separate (stdout, `waLog`), off in `tui` and `send`.

### Audit table (`wabot_audit`)

| Column | |
|---|---|
| `id` | autoincrement; cursor for `before` |
| `at` | Unix milliseconds |
| `actor` | who: `api`, `tui`, `cli`, `autoreply`, `whatsapp` |
| `action` | what (below) |
| `target` | chat JID, when there is one. For failed attempts, the recipient or chat as given (`5511999999999`) |
| `detail` | short, non-sensitive description |
| `ok` | 1 or 0 |
| `error` | error text when `ok` is 0 |

| Action | Written by | `detail` |
|---|---|---|
| `send` | `Bot.SendText` | `text, N chars` |
| `send_file` | `Bot.SendFile` | `<file name>, N bytes` |
| `react` | `Bot.React` | `<emoji> on <id>` |
| `edit` | `Bot.Edit` | message id |
| `delete` | `Bot.Delete` | message id |
| `read` | `Bot.MarkRead` | `N messages` (written when something was marked, or on any failure) |
| `pair` | `Bot.PairPhone`: `POST /pair`, TUI `/pair`, `wabot login -phone` | |
| `logout` | `POST /logout`, `wabot logout` | |
| `logged_out` | WhatsApp event (actor `whatsapp`) | reason code |

`send`, `send_file`, `react`, `edit` and `delete` run through `Bot.audited`, which records every
attempt exactly once, whatever the outcome. Failures (`ok` = 0) include input errors such as empty
text or file, a recipient that isn't on WhatsApp, an unknown or deleted `reply_to`, a message id
that doesn't exist, editing someone else's message or one past the 20-minute window, as well as
429s and WhatsApp rejecting the send.

Each `Bot.Audit` call also writes the `action` / `action failed` log line above, so the log and
the table agree.

## Conventions for new controls

- Every control action (anything that changes state on WhatsApp or in wabot) is wrapped in
  `Bot.audited(actor, action, target, detail, fn)`, so it's recorded once with the real error,
  including validation failures. Use `Bot.Audit` directly only where the action doesn't fit that
  shape (pair, logout, read).
  Add the new `action` name to the tables in this file and to the `AuditEntry` enum in
  `openapi.yaml`.
- `actor` is who asked: `api`, `tui`, `cli`, `autoreply`, `whatsapp`, and later
  `admin:<user>`. Pass it into `Bot` methods; don't infer it.
- `target` is the chat JID. `detail` is short and safe: ids, counts, sizes, file names.
- **Never log or audit** message text, captions, tokens, webhook secrets, passwords, session
  cookies or QR/pairing codes.
- Errors go in an `err` attribute, not formatted into the message string.
- Anything that sends a message goes through `Bot.deliver` (or `Bot.sendControl` for edits and
  revokes), so the rate limiter applies to the panel too. Number lookups go through `Bot.lookup`.
- Bad input returns `badInput(...)` (400); WhatsApp failures are returned as-is (502).

## Implementation phases

**Phase 1: read-only panel.** `admin/` folder with static HTML, a little CSS and
[htmx](https://htmx.org) (vendored, not from a CDN), embedded with `//go:embed admin/*` and
served at `/admin/`. Pages: status & QR, chats & messages (live via `EventSource`), audit log.
Auth: the existing API token, entered once and kept in `sessionStorage`; without a token the
panel works only from a browser on the same machine, because the API already rejects non-local
and cross-site requests (403). Serve the panel from the same origin as the API so those checks
pass. No new tables.

**Phase 2: admin auth and actions.** Session login, CSRF, read-only role. Send form,
react/delete buttons, pair/logout. Panel actions audited as `admin:<user>`.

**Phase 3: operational views.** Webhook delivery table and page, limiter snapshot, config
snapshot, audit filters, optional log ring buffer.

**Phase 4: runtime settings.** `wabot_settings` table, edit form for limiter, auto-reply and
webhook filter, applied without a restart, each change audited.

No JavaScript build step at any phase. Server-rendered fragments for htmx can use
`html/template`.

## Open questions

- Is a separate admin password worth it, or is "API token + bind to localhost + reverse proxy
  auth" enough for most users?
- Should the message viewer be in the panel at all, given it exposes message content to anyone
  with viewer access? Maybe off by default for the viewer role.
- How long to keep audit and webhook delivery rows? Add a `-retention` flag or prune by count?
- Retry a failed webhook delivery from the panel? That needs the body stored, which means
  storing message content in a second place.
- Should the panel reuse `/events` directly, or get its own stream with log and delivery events?
- Serve the panel on a separate `-admin-addr` so the API port can stay private to scripts?
