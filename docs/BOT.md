# Writing bot logic

There are two ways to make the bot do things.

## Option 1: webhook (any language)

Run wabot with a webhook, handle messages in your own service, and reply through the API.

```sh
WABOT_TOKEN=change-me ./wabot serve -autoreply=false -webhook http://localhost:9000/hook
```

Python example (standard library only):

```python
# echo_bot.py
import json, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

API, TOKEN = "http://localhost:8080", "change-me"

def send(to, text):
    req = urllib.request.Request(f"{API}/send", method="POST",
        data=json.dumps({"to": to, "text": text}).encode(),
        headers={"Authorization": f"Bearer {TOKEN}"})
    urllib.request.urlopen(req)

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.send_response(204); self.end_headers()
        if msg["from_me"] or msg["type"] != "text":
            return
        if msg["text"].lower().startswith("echo "):
            send(msg["chat"], msg["text"][5:])

HTTPServer(("127.0.0.1", 9000), Hook).serve_forever()
```

Always reply to `msg["chat"]`, not `msg["sender"]`. That way group messages get answered in
the group.

If you can't expose a port, consume `GET /events` (SSE) instead of a webhook.

## Option 2: edit `bot.go`

`autoReply()` in `bot.go` is a plain `switch` on the message text. Add a case:

```go
case "!dice":
    reply = fmt.Sprint(rand.IntN(6) + 1)
```

For more than a text reply, use the methods on `Bot` (`SendText`, `SendFile`) or the raw
whatsmeow client at `b.Client`. See the
[whatsmeow docs](https://pkg.go.dev/go.mau.fi/whatsmeow) for everything it can do:
reactions, read receipts, presence, group management, and more.

## Running it as a service

The bot reconnects on its own after network drops. To keep it running across reboots,
use systemd, launchd or Docker and point `-db` at a persistent path, e.g.:

```ini
# /etc/systemd/system/wabot.service
[Service]
ExecStart=/opt/wabot/wabot serve -db /var/lib/wabot/wabot.db -addr 127.0.0.1:8080
Environment=WABOT_TOKEN=change-me
Restart=always

[Install]
WantedBy=multi-user.target
```
