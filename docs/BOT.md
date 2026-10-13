# Writing bot logic

There are three ways to make the bot do things:

1. a **webhook**: wame POSTs messages to your service, which replies through the API
2. **polling**: your service pulls new messages with `GET /messages/wait` or `GET /events`
3. **Go code** in `internal/bot/events.go`

Whichever you pick, keep the [rate limits](API.md#rate-limiting) on. Bots that send a lot, fast,
or to people who never wrote to them are what get numbers banned.

## Option 1: webhook (any language)

Run wame with a webhook and a signing secret, handle messages in your own service, and reply
through the API.

Secrets go in environment variables rather than flags, so they don't show up in `ps`:

```sh
export WAME_TOKEN=change-me-to-a-long-secret WAME_WEBHOOK_SECRET=another-long-webhook-secret
./wame serve -autoreply=false -webhook http://localhost:9000/hook -webhook-filter "type=text&from_me=false"
```

Give the bot its own token rather than the owner secret, so it can be limited and revoked on its
own (and its sends show up under its name in the audit log):

```sh
./wame token create echo-bot -scopes send -expires 720h   # add -chats 5511999999999 to limit it
export WAME_BOT_TOKEN=wame_…                             # use this in the bot below instead of WAME_TOKEN
```

Python example (standard library only). It checks the signature, skips duplicate deliveries,
and answers `echo <text>` by quoting the original message:

```python
# echo_bot.py
import hashlib, hmac, json, os, time, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

API = "http://localhost:8080"
TOKEN = os.environ["WAME_TOKEN"]
SECRET = os.environ["WAME_WEBHOOK_SECRET"].encode()
seen = set()  # delivery IDs; use a real store with expiry in production

def verify(headers, body):
    ts = headers.get("X-Wame-Timestamp", "")
    sig = headers.get("X-Wame-Signature", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > 300:
        return False
    want = "sha256=" + hmac.new(SECRET, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, sig)

def send(to, text, reply_to=None):
    body = {"to": to, "text": text}
    if reply_to:
        body["reply_to"] = reply_to
    req = urllib.request.Request(f"{API}/send", method="POST", data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=60)

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        if not verify(self.headers, body):
            self.send_response(401); self.end_headers()
            return
        self.send_response(204); self.end_headers()

        delivery = self.headers["X-Wame-Delivery"]
        if self.headers["X-Wame-Event"] != "message" or delivery in seen:
            return
        seen.add(delivery)
        msg = json.loads(body)
        if msg["from_me"] or msg["type"] != "text":
            return
        if msg["text"].lower().startswith("echo "):
            send(msg["chat"], msg["text"][5:], reply_to=msg["id"])

HTTPServer(("127.0.0.1", 9000), Hook).serve_forever()
```

Always reply to `msg["chat"]`, not `msg["sender"]`. That way group messages get answered in
the group. Answer the webhook quickly (2xx) and do slow work afterwards: wame waits at most
10 s per attempt and retries on timeouts, which you'd see as duplicates.

### Verifying webhook signatures

With `-webhook-secret`, every request carries:

```
X-Wame-Timestamp: 1791680400
X-Wame-Signature: sha256=5d1c…
```

The signature is `hex(HMAC-SHA256(secret, timestamp + "." + raw_body))`. To verify:

1. Use the **raw** request body, before any JSON parsing.
2. Recompute the HMAC and compare with a **constant-time** function.
3. Reject timestamps more than a few minutes old, so a captured request can't be replayed later.
4. Dedupe on `X-Wame-Delivery`, since delivery is at-least-once.

Python:

```python
import hashlib, hmac, time

def verify(secret: bytes, timestamp: str, signature: str, body: bytes, max_age=300) -> bool:
    if not timestamp.isdigit() or abs(time.time() - int(timestamp)) > max_age:
        return False
    mac = hmac.new(secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest("sha256=" + mac, signature)
```

Node (no dependencies):

```js
const crypto = require("node:crypto");

function verify(secret, timestamp, signature, rawBody, maxAge = 300) {
  const ts = Number(timestamp);
  if (!Number.isInteger(ts) || Math.abs(Date.now() / 1000 - ts) > maxAge) return false;
  const want = "sha256=" + crypto.createHmac("sha256", secret)
    .update(`${timestamp}.`).update(rawBody).digest("hex");
  const a = Buffer.from(want), b = Buffer.from(signature || "");
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}

// usage with node:http
require("node:http").createServer((req, res) => {
  const chunks = [];
  req.on("data", c => chunks.push(c)).on("end", () => {
    const body = Buffer.concat(chunks);
    const ok = verify(process.env.WAME_WEBHOOK_SECRET, req.headers["x-wame-timestamp"],
                      req.headers["x-wame-signature"], body);
    res.writeHead(ok ? 204 : 401).end();
    if (ok) console.log(req.headers["x-wame-event"], JSON.parse(body));
  });
}).listen(9000, "127.0.0.1");
```

**Testing your receiver.** `POST /webhook/test` (or `t` on the [admin panel](ADMIN.md)'s Health
tab) sends a signed delivery with `X-Wame-Event: test` and no message, and reports what your
endpoint answered. Ignore event types you don't handle, as the examples above do. If deliveries
failed while your bot was down, `GET /stats` lists the last 20 failures and `POST /webhook/replay`
sends one again with the same `X-Wame-Delivery`, so de-duplicating by that header stays correct.

**Sending many messages.** If your bot answers one message at a time, `POST /send` is fine. For a
broadcast or anything in bulk, use the [send queue](QUEUE.md) instead of a loop over `/send`: it
returns at once, paces the sends, retries and backs off on its own, and keeps going after a
restart. With `-webhook`, each queued message that goes out or fails arrives as
`X-Wame-Event: queue` (the item without its text), which your receiver can use to track progress.

## Option 2: polling (no open port)

If you can't expose a port, pull messages instead. `GET /messages/wait` is a long-poll with a
cursor, so a restart of your bot doesn't lose messages: store `next_since` somewhere and start
from it.

```python
import json, urllib.request

API, TOKEN = "http://localhost:8080", "change-me-to-a-long-secret"
since = 0  # load the last value you saved

while True:
    url = f"{API}/messages/wait?since={since}&timeout=60&type=text&from_me=false"
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {TOKEN}"})
    page = json.load(urllib.request.urlopen(req, timeout=70))
    for msg in page["messages"]:
        print(msg["chat"], msg["text"])
    since = page["next_since"]  # save it
```

`GET /events` (SSE) works too and resumes by itself with `EventSource`. See
[API.md](API.md#get-events).

## Option 3: edit `internal/bot/events.go`

`autoReply()` in `internal/bot/events.go` is a plain `switch` on the message text. It answers in any chat,
including your own "Message yourself" chat, and quotes the command. It runs in its own goroutine
and has a small budget (6 replies per minute, one per 10 s per chat); replies over the budget are
dropped. Add a case:

```go
case "!dice":
    reply = fmt.Sprint(rand.IntN(6) + 1)
```

For more than a text reply, use the methods on `Bot`: `SendText`, `SendFile`, `React`, `Edit`,
`Delete`, `MarkRead`, `SetTyping`, `DownloadMedia`. They go through the rate limiters and write
to the audit log, so pass an actor name (`"autoreply"` or your own). The raw whatsmeow client is
at `b.Client`; see the [whatsmeow docs](https://pkg.go.dev/go.mau.fi/whatsmeow). If you send
with `b.Client` directly you skip the limiter and the audit log, so prefer the `Bot` methods.

## Running it as a service

The bot reconnects on its own after network drops. To keep it running across reboots,
use systemd, launchd or Docker and point `-db` at a persistent path, e.g.:

```ini
# /etc/systemd/system/wame.service
[Service]
ExecStart=/opt/wame/wame serve -db /var/lib/wame/wame.db -addr 127.0.0.1:8080 -log-format json
Environment=WAME_TOKEN=change-me-to-a-long-secret
Environment=WAME_WEBHOOK=http://127.0.0.1:9000/hook
Environment=WAME_WEBHOOK_SECRET=another-long-webhook-secret
Restart=always

[Install]
WantedBy=multi-user.target
```

App logs go to stderr (journald picks them up); `-log-file` writes them to a file instead.
