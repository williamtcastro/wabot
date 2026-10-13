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
{ "batch": "promo-october", "queued": 3, "replayed": 0, "items": [ { "id": 41, "status": "queued", "to": "5511999999999", … }, … ] }
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
- **Quiet at night, if you want.** With [quiet hours](#quiet-hours) set, nothing queued goes out
  in that window; it waits for the morning.
- **Backs off when WhatsApp pushes back.** One failed send is retried later on its own (30 s, 1 min,
  2 min… up to 15 min, 5 attempts in all). Two or more failures in a row pause the whole queue for
  30 s, then 1 min, 2 min… up to 10 min, until a send works again. Sends wait while the server
  is disconnected or logged out, without using up attempts.
- **Token rules still apply.** A token limited to some chats can only queue to those. Items from a
  token that is revoked or expires before they go out fail with that reason instead of being sent.

It doesn't send files, reactions or edits (use the direct routes). It takes at most 1000 messages
per request (repeat the request with the same `batch` for more), and at most 10,000 waiting per
token, so a misbehaving client can't fill the disk.

## CSV import

A spreadsheet export works too. `POST /queue` takes the same messages as a CSV file when sent with
`Content-Type: text/csv` (or `application/csv`), and `wame queue import` sends a file of any length from the command line.

```csv
to,text,send_at,reply_to
5511999999999,"Hi Maria, your order is ready.",,
5511988887777,"Hi John!
See you at 9.",2026-10-13T09:00:00-03:00,
552187654321,,,
```

- **The first line names the columns**, in any order and any case: `to` (required), `text`,
  `send_at` (RFC 3339) and `reply_to`. Any other column is refused with its name, so a typo like
  `txet` can't silently drop the text. Leave out the columns you don't need.
- **One message per line.** Fields with commas, quotes or line breaks go in double quotes (`""` for
  a quote inside), as every spreadsheet writes them. A UTF-8 BOM, Windows line ends, spaces around
  fields, blank lines, rows shorter than the header and empty columns without a name (the trailing
  `,,` some spreadsheets add) are all fine. A value outside the named columns is refused: it's
  usually text with a comma that wasn't quoted.
- **Commas or semicolons.** Spreadsheets set to a language with a decimal comma (Portuguese, for
  one) export with `;` between fields. When the header line has `;` and no `,`, the file is read
  with `;`.
- A row without `text` (like the last row above) gets the default text; without `send_at`, the
  default time or as soon as the limits allow.
- **Errors name the line** of the file, like `line 7: invalid recipient "12"`, and nothing in the
  request is queued.
- A number a spreadsheet turned into scientific notation (`5.511999E+12`) has lost digits, and one
  written with decimals (`5511999999999.0`, as pandas and some exports do) isn't the number you
  meant: both are refused. Format the `to` column as text before exporting.

```sh
curl -H "Authorization: Bearer $T" -H "Content-Type: text/csv" -H "Idempotency-Key: promo-oct-1" \
  --data-binary @promo.csv "localhost:8080/queue?batch=promo-october"
```

The query takes `batch`, `send_at` and `text` (the defaults, like the JSON body's top-level fields);
the limits are the JSON form's: 1000 rows and 16 MB per request, the token's allowlist checked on
every row, and `Idempotency-Key` to make a retry safe. Use `--data-binary`, not `-d`, which drops
the line breaks. Put the message text in the file's `text` column rather than in `?text=`: query
strings end up in proxy logs, shell history and browser history. `?text=` still works, for short
defaults that aren't private.

**From the command line**, `wame queue import` reads the file, checks every row locally (columns,
numbers, times, text length) and sends it to the server in requests of 1000 rows, all under one
batch. What only the server knows (the token's allowlist, its limit of 10,000 waiting messages) is
checked per request, after the earlier requests were queued, so an import can stop halfway; see
[running it again](#running-an-import-again).

```sh
wame queue import -batch promo-october -text "Hi! Our store opens at 9." promo.csv
# 1/3: lines 2-1001: queued 1000
# 2/3: lines 1002-2001: queued 1000
# 3/3: lines 2002-2341: queued 340
# queued 2340 messages, batch promo-october (follow it with: wame queue status -batch promo-october)
wame queue status -batch promo-october
# batch promo-october: queued 2210, sending 0, sent 130 (delivered 120, read 41), failed 0, canceled 0
```

It talks to the server over the API, like [`wame admin`](ADMIN.md) (same `-profile`, `-server`,
`-token`, `WAME_SERVER` / `WAME_TOKEN`, `./.env` and `admin.env`), so it works with a server in
Docker or on another machine; the token needs the `send` scope. `-dry-run` checks the file and
shows how it would be split, without sending anything.

### Running an import again

Each request carries an idempotency key: `-key` plus the request's number (`promo-oct.1`,
`promo-oct.2`, …) or, without `-key`, one taken from the file's full path (`csv-…`). Without
`-batch`, the batch name comes from that key too, so a rerun lands in the same batch. A request
whose key was used before returns what it queued instead of queuing it again; the CLI prints the
key in its error when an import stops.

- **It stopped halfway** (the network dropped, a row the token may not send to, the 10,000 waiting
  limit): fix what the error names, in that request's lines or later ones, and run the same
  command again (the error message spells it out, `-key` included). Requests whose rows didn't
  change are recognized and not queued twice; the rest are sent. Don't edit, add or remove lines
  before the request that failed: those were queued already.
- **Rows that were already queued can't be changed** by rerunning: a request whose rows (text,
  number, `reply_to`, `send_at`, batch) differ from what was queued under its key is refused with
  409, and nothing of it is queued. Cancel the batch and import again under a new `-key`
  instead. Rows only added at the end of the last request go through, so a file that grew can be
  sent again.
- **A new import from the same path** (next month's `promo.csv`) needs a new `-key`, or it's
  refused with 409; the same file sent again unchanged queues nothing new. Moving the
  file changes the derived key, so pass the `-key` from the error when resuming a moved file.
- `-text` and `-send-at` are filled in before sending, so changing them is a change of the rows.
- The server remembers the keys as long as it keeps the items, until 7 days after they finish.

## States

```
queued ──▶ sending ──▶ sent
  ▲  │          │
  │  │          ├──▶ failed      (bad number, not on WhatsApp, 5 attempts used, token revoked, opted out)
  │  ▼          │
  │ canceled    └──▶ queued      (retry later)
```

Finished items (sent, failed, canceled) are deleted after 7 days. The sent message itself stays
in the message history like any other.

**Delivered and read.** A sent item gains `delivered_at` when WhatsApp reports it reached the
phone, and `read_at` when the person opened it (only if they share read receipts; a read also
fills `delivered_at` when that receipt never came). They aren't states: the item stays `sent`.
`counts.delivered` and `counts.read` count them among the sent ones, the admin panel shows
✓ sent, ✓✓ delivered and a blue ✓✓ read, and each receipt sends one `queue` event (repeats don't).
Receipts are matched by the item's message id, so they work after a restart too. A receipt on an
item that isn't `sent` (one that failed although an attempt did reach the phone) is recorded but
sends no event, so a failure is reported once.

```json
{ "id": 41, "status": "sent", "message_id": "3EB0C4…", "delivered_at": "2026-10-12T21:41:05-03:00", "read_at": "2026-10-12T21:52:40-03:00", … }
```

## Quiet hours

`queue_quiet_hours` (`-queue-quiet-hours`, `WAME_QUEUE_QUIET_HOURS`, or the
[setting](API.md#get-settings-patch-settings)) is a daily window like `21:00-08:00`, read on the
clock of `queue_timezone` (`-queue-timezone`, `WAME_QUEUE_TIMEZONE`; an IANA name like
`America/Sao_Paulo`, default `Local`: the server's zone, which in Docker is UTC unless `TZ` is
set; settings and `GET /stats` show which one it is, like `Local (UTC)`). The window may cross
midnight; empty turns it off. Inside it the worker sends nothing and checks again every minute
(so a clock change or a suspended host can't oversleep it) until the window ends, then works
through what's due, at the usual pace. Changing the
setting wakes it at once. Items whose `send_at` falls inside the window go out when it ends.
`GET /stats` shows `quiet_until` while it holds, and the admin panel's Queue tab says "quiet hours
until 08:00". Only the queue keeps quiet: `/send`, the TUI, the CLI and auto-replies don't.

**Switching them off and on.** `queue_quiet` (`true` by default) turns the window off without
forgetting it: `PATCH /settings {"queue_quiet": false}` lets the queue send at any hour, and `true`
(or `null`) brings the same window back. `Q` on the admin panel's Queue tab does the same (or
`Enter` on "Quiet hours on" in the Settings tab). While off, `GET /stats` has `quiet_off: true` and
the tab shows the window marked "off"; while on, the panel's header shows `☾ quiet 21:00-08:00`,
or `☾ quiet until 08:00` in amber while it's holding the queue.

## Opt-outs

When someone answers a 1:1 chat with nothing but `STOP`, `SAIR`, `PARAR`, `CANCELAR`,
`UNSUBSCRIBE` or `DESCADASTRAR` (any case, accents ignored), wame puts the chat on an opt-out list
(`wame_optouts`) without replying, and audits it as `optout_add` by `whatsapp`. The list is
checked when an item's turn comes, after its number is resolved to the chat WhatsApp uses: items
to a listed chat fail with `recipient opted out` and don't take a send slot. Queuing to them is
still accepted (the list can change before the item's turn, and one check at send time keeps
every path consistent), so watch for that error. Such items don't count toward the "queued messages
failed" alert. Direct sends aren't affected.

WhatsApp may show a person under a hidden ID (`…@lid`) before it says which number that is; an
opt-out received that way is stored under that ID, matched against both the number and its ID, and
moved onto the number once WhatsApp links them. A number added to the list by hand while logged out
is stored as typed and also matched as typed, in case WhatsApp resolves it to a different JID (a
Brazilian mobile with or without the extra 9). Removing a number removes every form it is stored
under. If the list can't be read, the item waits and is retried without using an attempt. Admins manage the
list with [`/optouts`](API.md#get-optouts-post-optouts-delete-optoutschat).

## API

All routes need the `send` scope. Without `admin`, a token sees and cancels only the items it
queued itself; with `admin`, every item (`?actor=` narrows it).

| Route | |
|---|---|
| `POST /queue` | queue up to 1000 messages. Body: `messages` (each `to`, `text`, optional `reply_to` and `send_at`), optional `batch` (letters, digits, `.`, `_`, `-`; generated when left out), and top-level `text` / `send_at` used by messages that leave theirs out. Or a [CSV file](#csv-import) as `text/csv` or `application/csv`. Answers 202 with the items (without text), `queued` (how many items) and `replayed` (how many of them were already queued under the idempotency key) |
| `GET /queue` | items, newest first, plus `counts` by state. Query: `status`, `batch`, `actor` (admin), `before` (id, to page), `limit` (≤ 500) |
| `GET /queue/{id}` | one item, with its text |
| `DELETE /queue/{id}` | cancel a queued item (409 when it's already sending or finished) |
| `DELETE /queue?batch=NAME` | cancel every queued item of a batch |

A request is safe to retry with an `Idempotency-Key` header (or `idempotency_key` in the body):
the same key from the same token returns the items it already queued instead of adding them again
(counted in `replayed`). The same key with different messages (any of `to`, `text`, `reply_to`,
`send_at`, or a named `batch`) is refused with 409, like on `/send`, and nothing of that request is
queued. Replayed items don't count toward the 10,000 waiting limit. A body over 16 MB gets
413.

**Following progress.** `GET /queue?batch=promo-october` shows the counts. Or listen:
`/events?events=queue` streams a `queue` event each time an item changes state or a sent one is
delivered or read, and with `-webhook` a `queue` delivery (`X-Wame-Event: queue`) goes out when an
item is sent, fails, or is delivered or read.
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

**From an AI agent.** The MCP tools `schedule_message`, `list_scheduled` and `cancel_scheduled` use
the same queue with the same rules (send scope, allowlist, own items only); see [MCP.md](MCP.md#tools).

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
