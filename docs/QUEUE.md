# Send queue

`POST /send` holds the HTTP request open until the message goes out, which is fine for one reply
but not for a bulk send: a thousand messages at 20 per minute take 50 minutes, and a restart in the
middle loses whatever was still waiting. The **send queue** (the outbox) is for that case.
`POST /queue` stores the messages in the database and answers at once; a worker in the server
sends them, one at a time, within the same rate limits as everything else.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/queue -d '{
  "batch": "promo-october",
  "text": "Hi! Our store opens at 9 tomorrow.",
  "messages": [
    {"to": "5511999999999"},
    {"to": "5511988887777", "text": "Hi John! Your order is ready."},
    {"to": "5521987654321", "send_at": "2026-10-13T09:00:00-03:00"}
  ]
}'
```

```json
{ "batch": "promo-october", "queued": 3, "items": [ { "id": 41, "status": "queued", "to": "5511999999999", … }, … ] }
```

## What it guarantees

- **Survives restarts.** Items live in the `wame_outbox` table. One that was being sent when the
  process stopped goes back in the queue on the next start.
- **Same message id on every attempt.** Each item gets its WhatsApp message id before the first
  attempt and keeps it on retries. WhatsApp apps de-duplicate by message id, so if a send did
  reach WhatsApp just before a crash, the resend after the restart isn't shown as a second message.
- **Paced like everything else.** The worker uses the global limiter (`rate`, `chat_gap`) and the
  token's own rate. It only takes a slot that is free right now, so interactive sends
  (`/send`, the TUI, auto-replies) are never pushed back behind a long batch: they go first.
- **Less robotic.** Between queued sends the worker waits an extra random 0 to `queue_jitter`
  (default `2s`; `-queue-jitter`, `WAME_QUEUE_JITTER`, or the setting).
- **Backs off when WhatsApp pushes back.** One failed send is retried later on its own (30 s, 1 min,
  2 min… up to 15 min, 5 attempts in all). Two or more failures in a row pause the whole queue for
  30 s, then 1 min, 2 min… up to 10 min, until a send works again. Sends wait while the server
  is disconnected or logged out, without using up attempts.
- **Token rules still apply.** A token limited to some chats can only queue to those. Items from a
  token that is revoked or expires before they go out fail with that reason instead of being sent.

It doesn't send files, reactions or edits (use the direct routes). It takes at most 1000 messages
per request (repeat the request with the same `batch` for more), and at most 10,000 waiting per
token, so a misbehaving client can't fill the disk.

## States

```
queued ──▶ sending ──▶ sent
  ▲  │          │
  │  │          ├──▶ failed      (bad number, not on WhatsApp, 5 attempts used, token revoked)
  │  ▼          │
  │ canceled    └──▶ queued      (retry later)
```

Finished items (sent, failed, canceled) are deleted after 7 days. The sent message itself stays
in the message history like any other.

## API

All routes need the `send` scope. Without `admin`, a token sees and cancels only the items it
queued itself; with `admin`, every item (`?actor=` narrows it).

| Route | |
|---|---|
| `POST /queue` | queue up to 1000 messages. Body: `messages` (each `to`, `text`, optional `reply_to` and `send_at`), optional `batch` (letters, digits, `.`, `_`, `-`; generated when left out), and top-level `text` / `send_at` used by messages that leave theirs out. Answers 202 with the items (without text) |
| `GET /queue` | items, newest first, plus `counts` by state. Query: `status`, `batch`, `actor` (admin), `before` (id, to page), `limit` (≤ 500) |
| `GET /queue/{id}` | one item, with its text |
| `DELETE /queue/{id}` | cancel a queued item (409 when it's already sending or finished) |
| `DELETE /queue?batch=NAME` | cancel every queued item of a batch |

A request is safe to retry with an `Idempotency-Key` header (or `idempotency_key` in the body):
the same key from the same token returns the items it already queued instead of adding them again.
The same key with different messages is refused with 409, like on `/send`. A body over 16 MB gets
413.

**Following progress.** `GET /queue?batch=promo-october` shows the counts. Or listen:
`/events?events=queue` streams a `queue` event each time an item changes state, and with
`-webhook` a `queue` delivery (`X-Wame-Event: queue`) goes out when an item is sent or fails.
Neither includes the message text.

```json
{ "id": 41, "batch": "promo-october", "actor": "promo-bot", "to": "5511999999999", "status": "sent",
  "attempts": 1, "chat": "5511999999999@s.whatsapp.net", "message_id": "3EB0C4…", "due": "…", "created_at": "…", "updated_at": "…" }
```

**Pausing.** The `queue_paused` [setting](API.md#get-settings-patch-settings) holds everything
(`PATCH /settings {"queue_paused": true}`, or `P` on the admin panel's Queue tab). Like other
settings it's kept over restarts.

**Who sends.** The worker runs inside `wame serve` and `wame tui`. `wame mcp` and `wame send` don't
drain the queue. Every send is in the audit log as `send` under the token that queued it, with
`queue #id` in the detail; queuing and canceling are audited as `queue` and `queue_cancel`.

## Why not Redis

We measured it before deciding. On an M1 laptop, SQLite (WAL mode, the same file wame already
uses) stores about **10,000 queued messages per second** one at a time and about **32,000 per
second** in batches of 100. The worker's read, claim and finish of one item take about 0.3 ms
(`go test ./internal/store -run '^$' -bench .`).

The queue's speed is set by WhatsApp, not by storage: at the default 20 sends per minute the worker
needs one row every 3 seconds, about 30,000 times less than SQLite gives. Redis wouldn't send
anything sooner, and it would add:

- another service to run, secure and back up, next to a binary that's self-contained today;
- a second copy of the message text, in memory, which also needs persistence (AOF) configured
  or a Redis restart loses the queue;
- a network hop and a second source of truth to keep in step with the database.

What Redis is good at (many workers sharing one queue across machines) doesn't apply: one wame
process owns one WhatsApp session, and two processes sending for the same account would fight
over it. If you run several numbers, run one wame per number, each with its own database.
