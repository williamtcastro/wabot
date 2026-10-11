# HTTP API

Base URL: whatever you passed to `-addr` (default `http://localhost:8080`).

> **Interactive docs:** open `http://localhost:8080/docs` while the server runs. It's a
> Swagger UI page generated from [`openapi.yaml`](openapi.yaml), where you can click
> **Authorize**, paste your token and try every endpoint from the browser. The raw spec is at
> `/openapi.yaml`, ready to import into Postman, Insomnia or Bruno. Neither of those two URLs
> needs the token.

## Endpoints at a glance

| Method | Path | What it does |
|---|---|---|
| GET | `/status` | connection and login state |
| GET | `/qr` | login QR as a PNG |
| POST | `/pair` | log in with a pairing code |
| POST | `/logout` | unlink the device |
| POST | `/send` | send text |
| POST | `/send/file` | send an image or document |
| GET | `/messages` | stored messages, newest first |
| GET | `/chats` | conversations by most recent activity |
| GET | `/events` | live SSE stream |
| GET | `/contacts` | address book |
| GET | `/groups` | your groups |
| POST | `/check` | which numbers have WhatsApp |
| GET | `/docs`, `/openapi.yaml` | these docs (no token needed) |

## Auth

When a token is set (`-token` / `WABOT_TOKEN`), every request needs one of:

- header `Authorization: Bearer <token>`
- query string `?token=<token>`, handy for opening `/qr` in a browser

Without a token the API is open to anyone who can reach it, so bind to `127.0.0.1`
(`-addr 127.0.0.1:8080`) if you skip it.

## Errors

Non-2xx responses always look like this:

```json
{ "error": "not logged in" }
```

| Code | Meaning |
|---|---|
| 400 | bad input (missing fields, invalid JSON, bad recipient) |
| 401 | missing or wrong token |
| 404 | `/qr` when no QR is pending |
| 502 | WhatsApp rejected or failed the request (also "not logged in") |

## The Message object

Returned by `/send`, `/send/file`, `/messages` and the `message` event:

```json
{
  "id": "3EB0C767D26A1D0F5E2A",
  "chat": "5511999999999@s.whatsapp.net",
  "sender": "5511999999999@s.whatsapp.net",
  "push_name": "Maria",
  "from_me": false,
  "is_group": false,
  "type": "text",
  "text": "oi!",
  "timestamp": "2026-10-10T22:40:00-05:00"
}
```

`type` is one of `text`, `image`, `video`, `document`, `audio`, `sticker`, `reaction`,
`location`, `other`. For media, `text` holds the caption (or the file name for documents).
In groups, `chat` is the group JID and `sender` is the person.

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

Returns the current login QR as a PNG. 404 if you're already logged in.
QR codes rotate about every 20 seconds, so reload if it expires. After all codes time out the
bot starts a new round by itself.

```sh
open "http://localhost:8080/qr?token=$T"
```

## `POST /pair`

Log in with a pairing code instead of a QR. Must be called while not logged in.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/pair -d '{"phone":"+5511988887777"}'
```

```json
{ "code": "ABCD-EFGH" }
```

On the phone: **Linked devices → Link a device → Link with phone number instead**.

## `POST /send`

Send a text message.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/send \
  -d '{"to":"5511999999999","text":"Hello!"}'
```

| Field | Required | |
|---|---|---|
| `to` | yes | phone number or JID (see README → Recipients). Numbers are checked with WhatsApp first, so `5521987654321` and `552187654321` both reach the same person |
| `text` | yes | message body; WhatsApp formatting like `*bold*` and `_italic_` works |

Returns the sent Message.

## `POST /send/file`

Multipart upload. Images (detected from the file content) are sent as photos, everything
else as a document. Max 64 MB.

```sh
curl -H "Authorization: Bearer $T" -X POST localhost:8080/send/file \
  -F to=5511999999999 -F caption="invoice" -F file=@invoice.pdf
```

| Field | Required | |
|---|---|---|
| `to` | yes | recipient |
| `file` | yes | the file |
| `caption` | no | caption text |

Returns the sent Message.

## `GET /messages`

Most recent messages (incoming and sent), newest first. Messages are stored in the
`wabot_messages` table of the session database, so they survive restarts. Messages from
before the bot was linked aren't there, except for whatever WhatsApp sends in its one-time
history sync right after a fresh login.

| Query | Default | |
|---|---|---|
| `limit` | 50 | max items |
| `chat` | all | phone number or JID; matches either the chat or the sender |

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/messages?chat=5511999999999&limit=20"
```

## `GET /events`

[Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream. Two event types:

```
event: message
data: {"id":"…","chat":"…","text":"oi!", …}

event: status
data: {"connected":true,"logged_in":true,"state":"connected", …}
```

A `: ping` comment goes out every 25 s to keep proxies from closing the connection.

```sh
curl -N -H "Authorization: Bearer $T" localhost:8080/events
```

```js
const es = new EventSource("http://localhost:8080/events?token=T");
es.addEventListener("message", e => console.log(JSON.parse(e.data)));
```

## `POST /check`

Check which numbers have WhatsApp.

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

## `GET /chats`

Conversations ordered by last activity, built from stored messages.

| Query | Default | |
|---|---|---|
| `limit` | 50 | max chats |

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/chats?limit=10"
```

```json
[
  {
    "jid": "552187654321@s.whatsapp.net",
    "name": "Maria Silva",
    "is_group": false,
    "last_text": "tudo e vc",
    "last_type": "text",
    "last_from_me": true,
    "last_at": "2026-10-10T20:45:50-03:00"
  }
]
```

`name` is the address-book name, the group subject, or `~ProfileName` when the person isn't
in your contacts.

## `GET /contacts`

Your address-book contacts, synced from the phone a few seconds after the first connect.
Sorted by name. `q` filters by name or part of the number.

```sh
curl -H "Authorization: Bearer $T" "localhost:8080/contacts?q=maria"
```

```json
[ { "jid": "5511999999999@s.whatsapp.net", "phone": "+5511999999999", "name": "Maria Silva", "push_name": "Maria" } ]
```

## `GET /groups`

```json
[ { "jid": "120363012345678901@g.us", "name": "Family", "members": 12 } ]
```

## `POST /logout`

Unlinks the device from your phone and wipes the session from `wabot.db`. You'll need to
log in again afterwards (restart the process to get a new QR).

## Webhook

With `-webhook URL`, every message WhatsApp delivers to the bot is `POST`ed to `URL` as a
Message JSON body (`Content-Type: application/json`, 10 s timeout, no retries). That includes
messages you type on your phone (`from_me: true`), but not ones sent through this API.
Check `from_me` to skip your own messages.
