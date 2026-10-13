# Drafts that need approval (proposal)

> **Deferred proposal.** Not built. Today any client with the token can send right away through
> `POST /send`. See the [list of proposals](README.md).

## Goal

Let an agent propose a reply instead of sending it. A human (or a token allowed to approve) looks
at it in the TUI or over the API and approves, edits, or rejects it. Only an approved draft is
sent, through the normal send path, so rate limits, audit and idempotency all still apply.

Non-goals: multi-step approval chains, scheduled sends, drafts for files in v1 (text only; files
can follow with the same table and a blob column), a web UI (the
[admin panel](ADMIN-PANEL.md) can add one later).

## Assumptions about tokens

Scoped per-agent tokens exist (see [API.md → Tokens and scopes](../API.md#tokens-and-scopes)). Each
token has a **name**, which is also the audit actor (e.g. `support-bot`), **scopes** and an optional
chat allowlist. The scopes that exist today are `read`, `send` and `admin`. This proposal would add
two more:

| Scope | Allows |
|---|---|
| `drafts` | create drafts, list and read its own drafts, withdraw its own pending drafts |
| `approve` | list all drafts, approve, edit-and-approve, reject |

`send` stays as it is. Policy (below) decides whether a token with `send` must go through drafts
anyway.

## Flow

```
agent ── POST /drafts ──► pending ──approve──► sending ──► sent (message seq)
                            │   └──edit+approve──┘    └──► failed (error)
                            ├──reject──► rejected
                            ├──withdraw (author)──► withdrawn
                            └──expires_at passes──► expired
```

## Policy

Two layers; the stricter one wins.

**Per chat** (`wame_draft_policy`, set by an `approve`-scope token or the TUI):

| Mode | Effect on `POST /send` from a token | Effect on `POST /drafts` |
|---|---|---|
| `auto` (default) | sends as today | draft is created and sent immediately, recorded as approved by `policy` |
| `drafts` | 403 `{"error":"this chat requires drafts"}` | draft waits for approval |
| `blocked` | 403 | 403 |

**Per token** (part of the token config): `send_mode: direct | drafts`. A `drafts` token gets
the same 403 on `/send` everywhere and its drafts always wait, even in `auto` chats.

The TUI, CLI and auto-replies (`actor` `tui`, `cli`, `autoreply`) aren't tokens and keep
sending directly; `blocked` applies to them too (it's the "never touch this chat" switch).

## API

### `POST /drafts` (scope `drafts`)

```json
{
  "to": "5511999999999",
  "text": "Hi Maria, your order ships tomorrow.",
  "reply_to": "3EB0C767D26A1D0F5E2A",
  "note": "customer asked about shipping",
  "expires_in": "2h",
  "idempotency_key": "ticket-8812-reply"
}
```

`to` is resolved and checked when the draft is created (`Bot.Resolve`, so a number not on WhatsApp
is a 400 now, not at approval time). `reply_to` must exist in that chat. `note` is shown to the
approver and never sent. `expires_in` defaults to 24 h, max 7 days.

Returns 201 with the Draft:

```json
{
  "id": "d_01J9Z6Q4W8",
  "chat": "5511999999999@s.whatsapp.net",
  "chat_name": "Maria",
  "text": "Hi Maria, your order ships tomorrow.",
  "reply_to": "3EB0C767D26A1D0F5E2A",
  "note": "customer asked about shipping",
  "author": "support-bot",
  "status": "pending",
  "based_on_seq": 1051,
  "created_at": "2026-10-11T10:02:00-03:00",
  "expires_at": "2026-10-11T12:02:00-03:00"
}
```

`based_on_seq` is the chat's latest `seq` when the draft was made; it's how staleness is detected.

### Other endpoints

| Method | Path | Scope | |
|---|---|---|---|
| GET | `/drafts?status=pending&chat=&limit=&before=` | `approve` (all) or `drafts` (own only) | newest first |
| GET | `/drafts/{id}` | same | |
| POST | `/drafts/{id}/approve` | `approve` | body optional: `{"text":"edited text","force":false}` |
| POST | `/drafts/{id}/reject` | `approve` | `{"reason":"tone"}` (reason is shown to the author, not audited as text) |
| DELETE | `/drafts/{id}` | `drafts` (own, pending only) | withdraw |
| PUT | `/chats/{jid}/draft-policy` | `approve` | `{"mode":"drafts"}` |
| GET | `/chats/{jid}/draft-policy` | `read` | |

Approve returns the Draft with `status: "sent"` and `message` (the sent Message), or the error.
A rate-limited approve returns 429 with `Retry-After` and the draft stays `pending`, so the
approver can retry.

Chat allowlists apply everywhere: a token never sees drafts for chats outside its list (404).

### Events

- SSE: new event types on `/events`, opt-in with `events=draft`: `draft` with the Draft object on
  every status change. Filtered by `chat`. Delivered to tokens with `approve`, or `drafts` for
  their own.
- Webhook: `X-Wame-Event: draft`, same body, signed like messages. Off unless
  `-webhook-events` includes `draft` (so existing receivers don't get a new body shape).
- In-process: `bot.DraftChanged{Draft}` through `Bot.publish`, which the TUI listens to.

## TUI

- A **badge** in the sidebar chat list: `✎2` next to a chat with pending drafts, and a total in
  the footer status (`✎ 3 drafts`).
- `/drafts` opens a drafts pane in place of the conversation (like `/search` results): one entry per
  draft with chat name, author, age, note, and the text. `Ctrl+R` jumps there from anywhere.
- Keys in the pane:

| Key | |
|---|---|
| `j` `k` / `↑` `↓` | move |
| `a` | approve and send |
| `e` | edit: loads the text into the input; `Enter` approves the edited text, `Esc` cancels |
| `x` | reject (asks for an optional reason in the input) |
| `o` | open the chat at the draft's `based_on_seq` to see the context |
| `Esc` / `q` | close |

- In a chat with pending drafts, they render at the bottom of the conversation, dimmed, with
  `✎ draft by support-bot · a approve · e edit · x reject` when selected.
- Approving in the TUI uses actor `tui` as approver. Update `tuiHelp`, the footer hints,
  `tuiCommands` and `docs/TUI.md` together, per the conventions.

## Sending

Approve calls `Bot.SendText(ctx, actor, draft.Chat, text, SendOptions{ReplyTo: draft.ReplyTo})`
inside `Bot.ApproveDraft`. Nothing new talks to WhatsApp, so `Bot.deliver`, the limiter and
`-typing` apply unchanged. The `actor` passed to `SendText` is the **author**
(`support-bot`), so the `send` audit row says who wrote it; the approval is a separate row.

Status moves `pending → sending` in one `UPDATE … WHERE status = 'pending'` (checked with
`RowsAffected`) before calling `SendText`, so two approvers clicking at once send once. On
error it goes back to `pending` (429, timeouts) or to `failed` (400: e.g. `reply_to` deleted
meanwhile).

## Failure cases

| Case | Behaviour |
|---|---|
| New messages arrived in the chat after `based_on_seq` | draft is **stale**. API: approve returns 409 `{"error":"chat changed since the draft was made","new_messages":3}` unless `force: true`. TUI: shows `⚠ 3 new messages` and needs `a` twice |
| The `reply_to` message was deleted | approve fails with 400, draft → `failed` |
| Expired | a sweep every minute (and a check on approve) sets `expired`; approve → 410 |
| Chat policy changed to `blocked` after drafting | approve → 403, draft stays pending until it expires or is rejected |
| Logged out / disconnected | 502 as today; draft stays `pending` |
| Same idempotency key, same body | returns the existing draft (in any status), no new draft |
| Same key, different body | 409, like `/send` |
| Author token deleted | its pending drafts are rejected with reason `author removed` |

## Security

- **No self-approval.** `approve` on a draft whose `author` equals the caller's token name is 403,
  even if that token has both scopes. Enforced in `Bot.ApproveDraft`, not only in the handler, so
  every entry point gets it.
- A token with `drafts` can't read other tokens' drafts (they may contain context about other
  chats).
- Approve requires a token, or the TUI. Token-less local mode allows everything it allows today;
  document that drafts are meaningful only with tokens.
- Edited-then-approved text is what's sent; the audit row records `edited` so it's visible that
  the human changed it.
- `note` and `reason` are free text from agents and humans; render through `safe()` in the TUI.

## Storage

New tables via a `migrateDrafts` in `internal/store/drafts.go`:

```sql
CREATE TABLE IF NOT EXISTS wame_drafts (
	id           TEXT PRIMARY KEY,           -- "d_" + random
	chat         TEXT NOT NULL,
	text         TEXT NOT NULL,
	reply_to     TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT '',
	author       TEXT NOT NULL,              -- token name
	status       TEXT NOT NULL,              -- pending|sending|sent|failed|rejected|withdrawn|expired
	based_on_seq INTEGER NOT NULL,
	idem_key     TEXT NOT NULL DEFAULT '',
	idem_fp      TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	decided_at   INTEGER NOT NULL DEFAULT 0,
	decided_by   TEXT NOT NULL DEFAULT '',
	reason       TEXT NOT NULL DEFAULT '',
	sent_seq     INTEGER NOT NULL DEFAULT 0,
	error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS wame_drafts_status ON wame_drafts (status, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS wame_drafts_idem ON wame_drafts (author, idem_key) WHERE idem_key <> '';

CREATE TABLE IF NOT EXISTS wame_draft_policy (
	chat TEXT PRIMARY KEY,
	mode TEXT NOT NULL                       -- auto|drafts|blocked
);
```

- Idempotency for drafts lives in the table (unique per author and key, fingerprint in `idem_fp`),
  so it survives restarts, unlike the in-memory `/send` keys.
- Draft text is stored in the DB like message text already is. Sent, rejected and expired drafts
  are pruned after 30 days.
- `chat` goes through `PhoneJID` on insert. `RelinkLIDs` should also move `@lid` chats in both
  tables (same pattern as `wame_chat_state`).

## Audit

| Action | Actor | Target | `detail` |
|---|---|---|---|
| `draft` | author token | chat | `<draft id>, N chars` |
| `draft_approve` | approver (the token name, or `tui`) | chat | `<draft id>` or `<draft id>, edited` |
| `draft_reject` | approver | chat | `<draft id>` |
| `draft_withdraw` | author | chat | `<draft id>` |
| `draft_expire` | `drafts` | chat | `<draft id>` |
| `draft_policy` | setter | chat | `auto`, `drafts` or `blocked` |
| `send` | author token | chat | as today: `text, N chars` |

Never the draft text, note or reason. Add these to the tables in [ADMIN-PANEL.md](ADMIN-PANEL.md)
and the `AuditEntry` enum in `docs/openapi.yaml`. `draft_approve` is written with `Bot.Audit`
(it doesn't return a Message on its own) right before the `send` row from `SendText`.

## Where the code goes

- `internal/store/drafts.go`: tables, `CreateDraft`, `Draft`, `Drafts(filter)`, `ClaimDraft`
  (pending → sending), `FinishDraft`, `ExpireDrafts`, policy get/set.
- `internal/bot/drafts.go`: `CreateDraft`, `ApproveDraft`, `RejectDraft`, `WithdrawDraft`,
  policy checks, the expiry sweep goroutine started in `Start`, `DraftChanged` publish.
- `internal/bot/send.go`: none. The `/send` policy check goes in the API handler (it knows the
  token), with the "is this chat blocked" lookup in `bot` so the TUI can share it.
- `internal/api`: handlers in a new `drafts.go`; `/events` forwards `bot.DraftChanged` as `draft`.
- `internal/tui`: drafts pane, badge, keys.

## Effort and risks

- API + store + bot with tests: 3–4 days. TUI pane and badge: 2 days. Webhook/SSE events: half a day.
- Risks: approvers rubber-stamping (staleness warning helps; consider showing the last 3 messages in
  the pane); drafts piling up unseen when nobody runs the TUI (webhook event + expiry); policy
  confusion between per-chat and per-token rules (one table in the docs, stricter wins).

## Tests

- store: claim is exclusive (two goroutines, one wins), idempotency per author, expiry sweep,
  LID relink of drafts.
- bot: self-approval refused; stale draft → conflict unless forced; approve uses `SendText` (limiter
  reservation observed, audit rows `draft_approve` then `send` with author as actor); 429 leaves it
  pending; deleted `reply_to` → failed.
- api: scopes (`drafts` can't approve, can't list others'), allowlist → 404, `/send` 403 under
  `drafts` policy, `blocked` chat, SSE `draft` event.
- tui: badge renders, `a`/`e`/`x` flows, 110×24 layout still exactly 24 lines with the pane open.

## Open questions

- Should `auto` chats skip storing a draft row at all, and just send?
- Should rejecting feed the reason back to the agent via webhook only, or also a long-poll
  `GET /drafts/wait`?
- Allow file drafts in v1 (needs storing the upload until approval)?
- Is one approver enough, or do some chats need two?
- Should drafts for a chat be invalidated automatically when the human replies in that chat from
  the phone?
