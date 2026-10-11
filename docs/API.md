# HTTP API

Base URL: whatever you passed to `-addr` (default `http://127.0.0.1:8080` for `serve`).

> **Interactive docs:** open `http://localhost:8080/docs` while the server runs. It's a
> Swagger UI page generated from [`openapi.yaml`](openapi.yaml), where you can click
> **Authorize**, paste your token and try every endpoint from the browser. The raw spec is at
> `/openapi.yaml`, ready to import into Postman, Insomnia or Bruno. Neither of those two URLs
> needs the token. The page loads Swagger UI from jsDelivr with Subresource Integrity (sha384)
> hashes, so a tampered CDN file won't run.

## Endpoints at a glance

| Method | Path | What it does |
|---|---|---|
| GET | `/status` | connection and login state |
| GET | `/qr` | login QR as a PNG |
| POST | `/pair` | log in with a pairing code |
| POST | `/logout` | unlink the device |
| POST | `/send` | send text, optionally as a reply |
| POST | `/send/file` | send an image or document |
| POST | `/react` | react to a message (or remove your reaction) |
| POST | `/edit` | edit one of your text messages |
| POST | `/delete` | delete a message for everyone |
| POST | `/read` | send read receipts |
| POST | `/typing` | show "typing…" / "recording…" in a chat |
| GET | `/messages` | stored messages, filtered; newest first or cursor-paged |
| GET | `/messages/wait` | long-poll until new matching messages arrive |
| GET | `/events` | live SSE stream with resume |
| GET | `/media/{seq}` | download the photo/video/audio/document of a message |
| GET | `/chats` | conversations by most recent activity |
| GET | `/contacts` | address book |
| GET | `/groups` | your groups |
| POST | `/check` | which numbers have WhatsApp |
| GET | `/audit` | log of control actions (sends, edits, logins…) |
| GET | `/docs`, `/openapi.yaml` | these docs (no token needed) |

## Auth

When a token is set (`-token` / `WABOT_TOKEN`), every request needs one of:

- header `Authorization: Bearer <token>`
- query string `?token=<token>`, handy for `/qr` in a browser and for `EventSource`, which can't
  set headers

If both are present, the query string wins. Wrong or missing token → 401.

**Without a token** the API is locked down to local use:

- `serve` and `tui -addr` refuse to start on a non-loopback address (anything other than
  `127.0.0.1`, `::1` or `localhost`). Set a token to listen on `:8080` or a LAN IP.
- Requests are accepted only when the `Host` header is loopback and the request isn't
  cross-site: an `Origin` header must match `Host`, and `Sec-Fetch-Site` must not be
  `cross-site`. Anything else gets **403**. This stops web pages you visit from calling the
  API through your browser (CSRF, DNS rebinding). `curl localhost:8080/...` works as usual.

`/docs` and `/openapi.yaml` are always public.

Prefer the environment variables (`WABOT_TOKEN`, `WABOT_WEBHOOK_SECRET`) over the flags for
secrets: command-line flags are visible to other users in `ps`.

## Errors

Non-2xx responses always look like this:

```json
{ "error": "not logged in" }
```

| Code | Meaning |
|---|---|
| 400 | bad input: missing fields, invalid JSON, bad recipient, a number that isn't on WhatsApp, unknown or deleted `reply_to`, editing someone else's message or one older than 20 minutes, idempotency key over 200 characters, media over the 100 MB download cap |
| 401 | missing or wrong token |
| 403 | no token is configured and the request isn't a same-origin request to localhost |
| 404 | not found: no QR pending, unknown message `id` in that chat, unknown `seq`, a message without media, or a deleted message's media |
| 409 | an idempotency key was reused for a different request |
| 410 | media expired on WhatsApp's servers (`/media`) |
| 429 | rate limited (sends or number lookups); see the `Retry-After` header (seconds) and [Rate limiting](#rate-limiting) |
| 502 | WhatsApp rejected or failed the request (also "not logged in") |
| 504 | the request timed out or was cancelled while waiting |

## The Message object

Returned by `/send`, `/send/file`, `/react`, `/edit`, `/delete`, `/messages`, `/messages/wait`,
the `message` and `update` SSE events and the webhook:

```json
{
  "seq": 1042,
  "id": "3EB0C767D26A1D0F5E2A",
  "chat": "5511999999999@s.whatsapp.net",
  "sender": "5511999999999@s.whatsapp.net",
  "push_name": "Maria",
  "from_me": false,
  "is_group": false,
  "type": "image",
  "text": "look at this",
  "reply_to": "3EB0A1B2C3D4E5F60718",
  "media": {
    "mimetype": "image/jpeg",
    "size": 183204,
    "path": "/media/1042"
  },
  "timestamp": "2026-10-10T22:40:00-03:00"
}
```

| Field | |
|---|---|
| `seq` | monotonic number assigned when wabot stores the message. Use it as a cursor (`since`) |
| `id` | WhatsApp message ID. Unique within a chat; use it with `reply_to`, `/react`, `/edit`, `/delete`, `/read` |
| `chat` | the conversation: the person's JID in a 1:1 chat, the group JID in a group |
| `sender` | who wrote it. In groups that's the person, not the group |
| `push_name` | the sender's WhatsApp profile name |
| `type` | `text`, `image`, `video` (including round video notes), `document`, `audio`, `sticker`, `reaction`, `location` |
| `text` | body, caption, file name for documents without a caption, the emoji for reactions, `lat,long` for locations. Empty after a delete |
| `reply_to` | omitted unless set: the `id` of the quoted message, or for reactions the message reacted to |
| `media` | omitted for non-media types. `path` is the download URL relative to the API base |
| `edited`, `deleted` | omitted unless `true`. A deleted message keeps its `media` metadata, but the file can't be downloaded any more |

Messages are stored in the `wabot_messages` table of the session database, so they survive
restarts. Messages from before the bot was linked aren't there, except for whatever WhatsApp
sends in its one-time history sync right after a fresh login. Those imported messages also get
new `seq` values (with old timestamps), so a cursor reader started before the import will see them.
Edits found in the history sync are applied to the stored message instead of being stored twice.

`seq` comes from an `AUTOINCREMENT` column, so it only grows and is never reused, even if rows are
deleted.

Edits and deletes made by other people update the stored message only when they come from the
message's original author, or (for deletes) from a group admin removing someone's message.
Anything else is ignored.

Reactions are stored as their own messages (`type: "reaction"`, `reply_to` = the target).
An empty `text` means the reaction was removed.

## Filters

`/messages`, `/messages/wait` and `/events` accept the same query parameters. The webhook
filter (`-webhook-filter`) uses the same syntax.

| Query | |
|---|---|
| `chat` | phone number or JID. Matches the chat **or** the sender, so a person's number also matches what they write in groups |
| `type` | comma-separated list, e.g. `text,image` |
| `from_me` | `true` or `false` |
| `is_group` | `true` or `false` |
| `since` | a `seq`; only messages with a higher `seq` match |

Unknown types or non-boolean values are a 400.

---

## `GET /status`

```sh
curl -H "Authorization: Bearer $T" localhost:8080/status
```

```json
{
  "connected": true,
  "logged_in": true,
  "jid": "5511988887777@s.whatsapp.net",
  "push_name": "Maria",
  "state": "connected"
}
```

`state` is one of `starting`, `connecting`, `waiting_for_qr_scan`, `paired`, `connected`,
`disconnected`, `qr_timeout`, `logged_out`, `login_error: …`. While waiting for a scan, `qr`
holds the raw QR string.

## `GET /qr`

Returns the current login QR as a PNG. 404 if you're already logged in or no code is pending.
QR codes rotate about every 20 seconds, so reload if it expires. After all codes time out the
bot starts a new round by itself.

```sh
open "http://localhost:8080/qr?token=$T"
```

## `POST /pair`

Log in with a pairing code instead of a QR. Must be called while not logged in (400 otherwise).

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/pair -d '{"phone":"+5511988887777"}'
```

```json
{ "code": "ABCD-EFGH" }
```

On the phone: **Linked devices → Link a device → Link with phone number instead**.

## `POST /logout`

Unlinks the device from your phone and wipes the session from `wabot.db`. You'll need to
log in again afterwards (restart the process to get a new QR). Returns `{"ok": true}`.

## `POST /send`

Send a text message.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/send \
  -d '{"to":"5511999999999","text":"Hello!"}'

# reply to (quote) a message, safe to retry
curl -H "Authorization: Bearer $T" -H "Idempotency-Key: order-1234-shipped" -X POST localhost:8080/send \
  -d '{"to":"5511999999999","text":"Shipped ✅","reply_to":"3EB0C767D26A1D0F5E2A"}'
```

| Field | Required | |
|---|---|---|
| `to` | yes | phone number or JID (see README → Recipients). Numbers are checked with WhatsApp first, so `5521987654321` and `552187654321` both reach the same person. A number that isn't on WhatsApp is a 400 |
| `text` | yes | message body; WhatsApp formatting like `*bold*` and `_italic_` works |
| `reply_to` | no | `id` of a stored message in the same chat to quote. Unknown or deleted → 400. If the message was edited, the quote shows its current text |
| `idempotency_key` | no | up to 200 characters, see below. The `Idempotency-Key` header does the same and takes precedence |

Returns the sent Message.

**Idempotency.** If you send the same key again within 24 hours, you get the first result back
and nothing is sent again. A request that arrives while the first one is still in flight waits
for it. Failed sends aren't remembered, so you can retry them with the same key. Keys are kept
in memory (lost on restart) and are separate for `/send` and `/send/file`.

A key is bound to the request it was first used with: `to`, `text` and `reply_to` for `/send`;
`to`, file name, `caption`, `reply_to` and the file's SHA-256 for `/send/file`. Reusing a key for
a different request within 24 hours is a **409**:

```json
{ "error": "idempotency key was already used for a different request" }
```

## `POST /send/file`

Multipart upload. Images (detected from the file content) are sent as photos, everything
else (including videos and audio) as a document. Max 64 MB.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/send/file \
  -F to=5511999999999 -F caption="invoice" -F file=@invoice.pdf
```

| Field | Required | |
|---|---|---|
| `to` | yes | recipient |
| `file` | yes | the file |
| `caption` | no | caption text |
| `reply_to` | no | `id` of a message in the same chat to quote |
| `idempotency_key` | no | same as `/send`; the `Idempotency-Key` header also works |

Returns the sent Message.

## `POST /react`

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/react \
  -d '{"chat":"5511999999999","id":"3EB0C767D26A1D0F5E2A","emoji":"👍"}'
```

| Field | Required | |
|---|---|---|
| `chat` | yes | the chat the message is in |
| `id` | yes | the message to react to (must be stored; 404 otherwise) |
| `emoji` | no | the reaction. Empty removes your reaction |

Returns the reaction as a Message (`type: "reaction"`). Counts against the rate limit.

## `POST /edit`

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/edit \
  -d '{"chat":"5511999999999","id":"3EB0C767D26A1D0F5E2A","text":"fixed typo"}'
```

Only your own **text** messages can be edited, and only within 20 minutes of sending (WhatsApp's
edit window, `whatsmeow.EditWindow`); otherwise 400. Returns the updated Message with
`edited: true`. Counts against the rate limit.

## `POST /delete`

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/delete \
  -d '{"chat":"5511999999999","id":"3EB0C767D26A1D0F5E2A"}'
```

Deletes the message for everyone. You can delete your own messages, and other people's
messages only in groups where you're an admin (wabot doesn't check admin rights; WhatsApp
does). Deleting someone else's message in a 1:1 chat is a 400. Returns the updated Message
with `deleted: true` and empty `text`. wabot also drops its stored copy of the original message,
so its media can't be downloaded afterwards (404) and it can't be quoted. Counts against the
rate limit.

## `POST /read`

Sends read receipts (blue ticks) for incoming messages.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/read -d '{"chat":"5511999999999"}'
curl -H "Authorization: Bearer $T" -X POST localhost:8080/read \
  -d '{"chat":"5511999999999","ids":["3EB0C767D26A1D0F5E2A"]}'
```

| Field | Required | |
|---|---|---|
| `chat` | yes | the chat |
| `id` / `ids` | no | one id or a list. Without either, the latest 50 incoming messages in the chat are marked |

Your own messages, reactions and unknown ids are skipped. Returns `{"marked": 3}`.

## `POST /typing`

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/typing \
  -d '{"chat":"5511999999999","state":"typing"}'
```

`state` is `typing` (the default), `recording` (voice message) or `paused`. WhatsApp clears
the indicator by itself after a while. Returns `{"ok": true}`. Each call counts against the
number-lookup limit (see [Rate limiting](#rate-limiting)).

WhatsApp only relays typing indicators while the account is "available". Unless wabot runs with
`-online`, it switches to available while any chat shows an indicator and back to unavailable
once every indicator is `paused` or 25 seconds old, so a client that never sends `paused` can't
leave you online.

## `GET /messages`

Stored messages (incoming and sent), filtered with the [shared filters](#filters).

- **Without `since`:** newest first by timestamp. Good for "show me the last 20".
- **With `since`:** oldest first by `seq`, starting after `since`. Good for paging through
  everything or polling for new messages: pass the last `seq` you got as the next `since`.

| Query | Default | |
|---|---|---|
| `limit` | 50 | max items (up to 1000) |
| `chat`, `type`, `from_me`, `is_group`, `since` | | see [Filters](#filters) |

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/messages?chat=5511999999999&limit=20"

# page through every incoming text, oldest first
curl -H "Authorization: Bearer $T" "localhost:8080/messages?since=0&type=text&from_me=false&limit=500"
```

## `GET /messages/wait`

Long-poll: returns as soon as at least one message matching the filters exists after `since`,
or after `timeout` seconds with an empty list.

| Query | Default | |
|---|---|---|
| `since` | the newest stored `seq` | start after this `seq`. Leave it out to wait only for messages that arrive after the request |
| `timeout` | 30 | seconds, max 120 |
| `limit` | 100 | max items (up to 1000) |
| `chat`, `type`, `from_me`, `is_group` | | see [Filters](#filters) |

```json
{ "messages": [ { "seq": 1043, "...": "..." } ], "next_since": 1043 }
```

Pass `next_since` as `since` on the next call and you never miss or repeat a message, even
across reconnects. Set your HTTP client timeout above `timeout`.

```sh
since=0
while true; do
  r=$(curl -s -m 70 -H "Authorization: Bearer $T" \
      "localhost:8080/messages/wait?since=$since&timeout=60&from_me=false")
  echo "$r" | jq -c '.messages[]'
  since=$(echo "$r" | jq '.next_since')
done
```

## `GET /events`

[Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream. Three event types:

```
id: 1043
event: message
data: {"seq":1043,"id":"…","chat":"…","text":"oi!", …}

event: update
data: {"seq":1040,"id":"…","text":"fixed typo","edited":true, …}

event: status
data: {"connected":true,"logged_in":true,"state":"connected", …}
```

| Event | When | Notes |
|---|---|---|
| `message` | a new message is stored (incoming, sent from your phone, or sent through wabot) | carries `id: <seq>`; filtered |
| `update` | a stored message is edited or deleted, by you or by someone else | the whole updated Message; filtered (except `since`); no `id`, not replayed |
| `status` | connection or login state changes | a Status object; not filtered |

| Query | |
|---|---|
| `events` | comma-separated subset of `message,update,status`. Default: all three |
| `chat`, `type`, `from_me`, `is_group`, `since` | see [Filters](#filters) |

**Resuming.** The stream first replays every stored matching message after a starting `seq`,
then continues live. The starting point is, in order:

1. the `Last-Event-ID` header, if it's a number. Browsers' `EventSource` sends it automatically
   when it reconnects, so a dropped connection picks up where it left off, even if the URL has
   `since`. A non-numeric value is ignored.
2. `?since=`
3. otherwise "now": only messages stored after you connect.

Messages imported by a history sync while you're connected are streamed as well.

`message` events are read from the database, so a slow client doesn't lose them. `update` and
`status` events can be dropped if the client falls far behind.

A `: ping` comment goes out every 25 s to keep proxies from closing the connection.

```sh
curl -N -H "Authorization: Bearer $T" "localhost:8080/events?from_me=false&events=message"
```

```js
const es = new EventSource("http://localhost:8080/events?token=T&type=text");
es.addEventListener("message", e => console.log(e.lastEventId, JSON.parse(e.data)));
es.addEventListener("update", e => console.log("edited/deleted", JSON.parse(e.data)));
```

## `GET /media/{seq}`

Downloads and decrypts the media of a stored message (photo, video, voice message, sticker,
document). `seq` comes from the Message; `media.path` already has the full path.

```sh
curl -H "Authorization: Bearer $T" -OJ localhost:8080/media/1042
```

Images (except SVG), audio, video and PDF are served `inline` with their original
`Content-Type`. Everything else is served as `application/octet-stream` with
`Content-Disposition: attachment`, so a browser downloads it instead of rendering it. The file
name comes from the message, or is `wabot-<type>-<seq>.<ext>` when there isn't one. Every
response also has `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox;
default-src 'none'` and `Referrer-Policy: no-referrer`.

| Code | |
|---|---|
| 400 | file is over the 100 MB download cap. The cap applies to the bytes actually downloaded, not just the size the sender declared |
| 404 | unknown `seq`, the message has no media, or it was deleted |
| 410 | WhatsApp no longer has the file (it answered 403, 404 or 410; media expires on their servers after a while) |

Media is fetched from WhatsApp on each request; wabot doesn't keep a copy.

## `GET /chats`

Conversations ordered by last activity, built from stored messages. Reactions don't count
as activity.

| Query | Default | |
|---|---|---|
| `limit` | 50 | max chats (up to 1000) |

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/chats?limit=10"
```

```json
[
  {
    "jid": "552187654321@s.whatsapp.net",
    "name": "Maria",
    "is_group": false,
    "last_text": "tudo e vc",
    "last_type": "text",
    "last_from_me": true,
    "last_at": "2026-10-10T20:45:50-03:00"
  }
]
```

`name` is the address-book name, the group subject, `You` for your own chat, or `~ProfileName`
when the person isn't in your contacts.

## `GET /contacts`

Your address-book contacts, synced from the phone a few seconds after the first connect.
Sorted by name. `q` filters by name or part of the number.

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/contacts?q=maria"
```

```json
[ { "jid": "5511999999999@s.whatsapp.net", "phone": "+5511999999999", "name": "Maria", "push_name": "Maria" } ]
```

## `GET /groups`

```json
[ { "jid": "120363012345678901@g.us", "name": "Family", "members": 12 } ]
```

## `POST /check`

Check which numbers have WhatsApp (1 to 50 per call). Each number counts against the
number-lookup limit, so a large batch can get a 429.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/check \
  -d '{"phones":["+5511999999999","15551234567"]}'
```

```json
[
  { "query": "+5511999999999", "on_whatsapp": true, "jid": "5511999999999@s.whatsapp.net" },
  { "query": "+15551234567", "on_whatsapp": false, "jid": "" }
]
```

## `GET /audit`

Control actions taken through wabot, newest first: sends, files, reactions, edits, deletes,
read receipts, pairing and logout, plus WhatsApp logging the device out. Every send, file,
reaction, edit and delete attempt is recorded exactly once, including ones rejected for bad
input (`ok: false` with the error). Message text is never stored here.

| Query | Default | |
|---|---|---|
| `limit` | 100 | max entries (up to 1000) |
| `before` | latest | only entries with an `id` lower than this; pass the last `id` you got to page back |

```json
[
  {
    "id": 57,
    "at": "2026-10-10T22:41:03-03:00",
    "actor": "api",
    "action": "send",
    "target": "5511999999999@s.whatsapp.net",
    "detail": "text, 6 chars",
    "ok": true
  },
  {
    "id": 56,
    "at": "2026-10-10T22:40:12-03:00",
    "actor": "tui",
    "action": "edit",
    "target": "5511988887777@s.whatsapp.net",
    "detail": "3EB0C767D26A1D0F5E2A",
    "ok": false,
    "error": "…"
  }
]
```

`actor` is `api`, `tui`, `cli`, `autoreply` or `whatsapp`. `action` is `send`, `send_file`,
`react`, `edit`, `delete`, `read`, `pair`, `logout` or `logged_out`. See
[ADMIN-PANEL.md](ADMIN-PANEL.md#logging-today) for what each one records.

---

## Rate limiting

Sending many messages quickly is the most common way automated WhatsApp numbers get banned.
wabot spaces out everything it sends: texts, files, reactions, edits, deletes and auto-replies.

- `-rate N` (default 20, env `WABOT_RATE`): at most N messages in any rolling minute. `0` turns
  this cap off.
- `-chat-gap D` (default `1s`): at least this long between two messages to the same chat.
  `0` turns it off.
- `-max-wait D` (default `30s`): a send waits in line for its slot. If the slot is further away
  than this, the API answers **429** with `Retry-After: <seconds>` instead of waiting. `0` means
  always wait.
- `-typing`: show "typing…" for a moment (scaled to the text length, 0.8–4 s) before each text.

The limiter is shared by every client in the process (API, TUI, CLI, auto-replies). `/read`
isn't limited.

**Number lookups** have a separate limit of 3 × `-rate` per minute (60 by default; unlimited
when `-rate 0`), with the same `-max-wait` and 429 behaviour. A lookup is: resolving a phone
number that isn't cached yet (any `to` / `chat` given as a number), each number in `/check`,
and each `/typing` call. Checking many numbers that never wrote to you is another pattern that
gets accounts flagged.

**Auto-replies** have their own small budget on top of that: at most 6 per minute and one per
10 s per chat. Replies over that budget are dropped, not queued, so a flood of `!ping` can't
fill the send queue. They run in the background and don't hold up incoming messages.

**Presence.** By default wabot tells WhatsApp the account is "unavailable", so your phone keeps
showing notifications. `-online` marks it "available" while connected instead.

## Webhook

With `-webhook URL`, wabot `POST`s each newly stored message to `URL` as a Message JSON body,
and each edit or delete of a stored message as an update.

What gets delivered:

- incoming messages, and messages you type on your phone (`from_me: true`)
- **not** messages sent through wabot itself (API, TUI, CLI, auto-replies)
- **not** duplicates WhatsApp sends again, protocol messages, or history-sync imports
- edits and deletes, as `X-Wabot-Event: update` with the updated Message

Headers on every request:

| Header | |
|---|---|
| `Content-Type` | `application/json` |
| `X-Wabot-Event` | `message` or `update` |
| `X-Wabot-Delivery` | unique ID for this delivery, the same on every retry. Use it to dedupe |
| `X-Wabot-Timestamp` | Unix seconds when this attempt was sent |
| `X-Wabot-Signature` | `sha256=<hex>`, only with `-webhook-secret` |

**Signature.** With `-webhook-secret S`, the signature is
`hex(HMAC-SHA256(S, "<X-Wabot-Timestamp>.<raw body>"))`. Verify it with a constant-time
comparison and reject old timestamps. [BOT.md](BOT.md#verifying-webhook-signatures) has
Python and Node code.

**Delivery.** Any 2xx counts as delivered. Network errors, timeouts (10 s), 429 and 5xx are
retried with exponential backoff and jitter (about 1, 2, 4, 8, 16 s), 6 attempts in total.
Other statuses aren't retried. Redirects aren't followed: a 3xx counts as a failed delivery,
so point `-webhook` at the final URL. Failures are logged with the delivery ID, status and
error, but not the URL. Four workers send in parallel from an in-memory queue of 1000;
when the queue is full, new deliveries are dropped and logged. So delivery is **at-least-once
and unordered**, and anything still queued is lost when the process stops. If you can't afford
gaps, use `/messages/wait` or `/events` with a cursor instead, or as a backfill.

**Filter.** `-webhook-filter` takes the [filter](#filters) syntax as a query string:

```sh
./wabot serve -webhook https://example.com/hook -webhook-secret "$SECRET" \
  -webhook-filter "type=text,image&from_me=false"
```

`since` is ignored there. A `chat` given as a phone number is looked up on WhatsApp once the bot
is logged in, the same way `/send` resolves recipients (so Brazil's optional 9th digit and hidden
`@lid` IDs are handled). Until then, or if the lookup fails, it's matched as typed and a warning
is logged. A JID (`…@s.whatsapp.net`, `…@g.us`) is used as is.
