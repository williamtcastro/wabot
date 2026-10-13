# Admin panel (`wame admin`)

`wame admin` is a terminal panel for a wame server that runs somewhere else: a VPS, a home server,
a container. It talks to that server only through the [HTTP API](API.md), so it doesn't need the
database file, doesn't open a WhatsApp connection and can run alongside `serve` or `tui`.

```sh
export WAME_TOKEN=…                          # the server's owner secret, or a token with the admin scope
wame admin -server https://wame.example.com
```

## Tabs

| Tab | Shows | Keys |
|---|---|---|
| **1 Status** | cards for the WhatsApp session, the server (URL, version and uptime, latency, live or polling), tokens (active, expiring within 7 days, unused for 30+ days, revoked), the last 24 h of activity, messages in the last hour (received, sent, busiest chats) and health (send rate, 429s, webhook). While the server isn't linked, the QR to scan instead, or the pairing code after `p` | `p` pair with a phone number, `L` log out (asks first) |
| **2 Chats** | the server's chats with unread counts, and the selected chat's latest messages; or results of a search across every chat | `j`/`k`, `g`/`G` pick a chat, `i` send a message, `/` search, `Esc` clear the search |
| **3 Tokens** | every token with colored scopes (`read`, `send`, `admin`), chats, rate, expiry (amber within 7 days), last use (amber ⚠ when unused for 30+ days), its requests and sends in the last 24 h (failed sends in red), open live connections, and creator; revoked and expired ones struck through. A card below shows the selected token in full, including when and from which IP it was last seen | `j`/`k` move, `n` new, `R` rotate, `x` revoke (both ask first) |
| **4 Audit** | the audit log, newest first, with ✓ / ✗ and actions colored by kind; a card below shows the selected entry with its full time and error | `j`/`k`, `Ctrl+D`/`Ctrl+U`, `g`/`G` move, `/` filter, `Esc` clear the filter |
| **5 Health** | the server's version and uptime; for sends, number lookups and auto-replies, a meter of the last minute's use against the limit, the per-chat gap and 429s (or dropped auto-replies) since startup; the webhook's queue, delivered / retried / replayed / given up / dropped counts and its latest failures | `j`/`k` pick a failure, `R` replay it, `t` send a test delivery |
| **6 Settings** | the settings that change without a restart (send rate, chat gap, max wait, auto-replies, webhook filter): the value in effect, the server's flag, and who changed it and when | `j`/`k` move, `Enter` change (toggles auto-replies), `x` back to the flag |

Everywhere: `1`–`6` or `Tab` / `Shift+Tab` switch tabs, `S` switches server (see
[Several servers](#several-servers)), `r` refreshes, `q` or `Ctrl+C` quits. The header shows unread
chats, active tokens, failed actions in the last 24 h (red), a `!` on Health after a 429 or
webhook failure in the last hour and a `•` on Settings while any is changed from its flag, plus the connection state, the server, live or polling, and the
latency. Confirmations, forms and the new token open as dialogs over the current tab.

## Live updates

The panel follows the server's [event stream](API.md#get-events) (`⚡live` in the header): a
status change shows at once, and new messages update the Chats tab and the Status tab's
message counts as they arrive. If the stream drops, it reconnects with backoff (2 s up to 30 s) and
the panel polls every 3 seconds meanwhile; while live, it polls every 15 s for the rest.

When a healthy server loses WhatsApp, gets logged out or stops answering, the terminal **bell**
rings once and the reason shows in red at the bottom.

The message counts keep only the time, chat and sender name of each message (never its text), in
memory, for an hour.

## Chats

The Chats tab reads the same data as `GET /chats` and `GET /messages`: what the server has stored,
newest 60 messages per chat. It's a quick look, not a second client. Viewing a chat **doesn't** mark
it as read on the phone. `i` sends a text to the open chat as the account, through the server's
rate limits, and is audited under the token's name. `/` searches every chat with the server's
full-text search.

Text written by other people (names, messages, file names) is stripped of control characters and
bidi overrides before it's drawn, so it can't redraw or recolor the screen.

## Rotating a token

`R` on a token creates a replacement with the same scopes, chat allowlist and rate, and the same
lifetime it was created with (counted from now). It's named after the old one with a number
(`support-bot` → `support-bot-2` → `support-bot-3`; revoked names stay reserved). The new token is
shown once; the old one, and every token it created, is revoked right after. If creating fails,
nothing is revoked; if revoking fails, the new token still works and the panel says so.

Tokens nobody has used for 30 days (counting from creation if never used) are flagged in amber:
candidates to revoke.

## Changing settings

The Settings tab uses [`PATCH /settings`](API.md#get-settings-patch-settings): a change applies on
the server at once (the next send already uses the new rate), and is saved there, so it **also wins
over the server's flags after a restart** until you reset it with `x`. The FLAG column shows what
the server was started with. Bad values are refused with the reason and change nothing. Every
change is in the audit log as `settings`.

## Webhook test and replay

On the Health tab, `t` sends a signed test delivery (`X-Wame-Event: test`, no message) and shows
how the receiver answered, which is handy after changing its code or the secret. `R` sends the
marked failed delivery again, once, with the same delivery id; it's marked `replayed` when the
receiver accepts it. Only deliveries in the list can be replayed. See
[`POST /webhook/test`, `POST /webhook/replay`](API.md#post-webhooktest-post-webhookreplay).

## Logging in a server remotely

When the server isn't linked yet, the Status tab draws the QR the server is offering, so you can
scan it from your phone without shell access to the box. The QR rotates every ~20 seconds and the
panel follows. Or press `p`, type the phone number with its country code, and enter the 8-character
code it shows under WhatsApp → Linked devices → Link a device → Link with phone number.

`L` logs out: it unlinks the device and deletes the session on the server, which then needs a new
login. Both are recorded in the audit log under the token's name (`api` for the owner secret).

## Creating tokens

`n` opens a form: name, scopes (`read`, `send`, `admin`), an optional chat allowlist (numbers or
JIDs), the per-token send rate (default 10/min, `0` = only the global limit) and an expiry (default
`720h`; empty = never). The token is shown **once**; press `c` to copy it (system clipboard, or the
terminal's OSC 52 clipboard over SSH). Any other key closes it for good.

The server's rules apply as in [Tokens and scopes](API.md#tokens-and-scopes): with an admin token
(not the owner secret), the new token can't have the `admin` scope, can't outlive yours, and is
revoked along with it. The same goes for rotating.

## Connecting

| Flag | Env | Default | |
|---|---|---|---|
| `-server URL` | `WAME_SERVER` | `http://127.0.0.1:8080` | the server's base URL; a path prefix behind a reverse proxy works (`https://example.com/wame`). Without a scheme, `https://` is assumed |
| `-token TOKEN` | `WAME_TOKEN` | | the owner secret or an admin token. Prefer the env variable: flags show up in `ps` |
| `-profile NAME` | | | use the saved server `NAME` (below) |
| `-insecure` | `WAME_INSECURE` (in a file) | `false` | allow plain `http://` to a non-loopback address |

### Saving the server and token

So you don't have to export them each time, `wame admin` also reads `WAME_SERVER`, `WAME_TOKEN` and
`WAME_INSECURE` from env files (`KEY=value` lines; other keys are ignored). It takes each value from
the first place that has it:

1. the `-server` / `-token` flags;
2. the environment;
3. `.env` in the current directory (the same file Docker Compose uses; see
   [`.env.example`](../.env.example));
4. `~/.config/wame/admin.env` (or `$XDG_CONFIG_HOME/wame/admin.env`), for use from any folder.

```sh
mkdir -p ~/.config/wame
printf 'WAME_SERVER=https://wame.example.com\nWAME_TOKEN=wame_…\n' > ~/.config/wame/admin.env
chmod 600 ~/.config/wame/admin.env
wame admin
```

A `WAME_SERVER` (and `WAME_INSECURE`) from a file is only used with a `WAME_TOKEN` from **that same
file**. If the token comes from somewhere else (say, `WAME_TOKEN` exported in your shell) and a
`.env` in the folder you're in names a server, wame refuses rather than send your token to it. Fix
it by putting both in one file, or by passing `-server`. Keep these files private (`chmod 600`):
they hold the token.

### Several servers

Save each server as its own file in `~/.config/wame/servers/`, named after it:

```sh
mkdir -p ~/.config/wame/servers && chmod 700 ~/.config/wame/servers
printf 'WAME_SERVER=https://wame.example.com\nWAME_TOKEN=wame_…\n' > ~/.config/wame/servers/vps.env
printf 'WAME_SERVER=http://10.0.0.5:8080\nWAME_TOKEN=wame_…\nWAME_INSECURE=true\n' > ~/.config/wame/servers/home.env
chmod 600 ~/.config/wame/servers/*.env
```

Each file needs both `WAME_SERVER` and `WAME_TOKEN`; files missing either are skipped.
`wame admin -profile vps` opens one directly. Without `-profile`, and with no token found in the
places above, the panel starts on a list of them. Inside the panel, `S` opens the list and switches
without restarting; everything loaded from the previous server is dropped, including answers still
on their way.

### Keeping the token safe

The token goes in every request, so the panel refuses plain `http://` to anything but
`localhost`/`127.0.0.1`/`::1`. Two good setups:

- **SSH tunnel** (nothing exposed): keep the server on `127.0.0.1:8080` and run
  `ssh -N -L 8080:127.0.0.1:8080 you@server`, then `wame admin` with the default `-server`.
- **HTTPS reverse proxy** (Caddy, nginx, Traefik) in front of `wame serve -addr 127.0.0.1:8080`,
  then `wame admin -server https://wame.example.com`.

Use `-insecure` (or `WAME_INSECURE=true` in a server's file) only on a network you trust (a VPN,
Tailscale): the token would otherwise be readable by anyone on the path.

A token without the admin scope can still open the panel and see the Status and Chats tabs (without
the QR), but the Tokens, Audit and Health tabs say it needs the admin scope. Prefer a dedicated
admin token with an expiry over the owner secret, so it can be revoked (or rotated) on its own:

```sh
wame token create ops-laptop -scopes admin -expires 720h   # on the server
```

The Health tab, the usage columns, webhook test and replay, and the Settings tab need a recent
server (`GET /stats`, `/settings`, `/webhook/*`); an older one says so instead of failing.

## What it doesn't do (yet)

No sending files, reactions or replies (use `wame tui` on the server, or the API), no changing the
webhook URL or secret, and no history of webhook deliveries or usage beyond what the server keeps
in memory since it started (the last 20 failures, 24 hours of usage). See the [web admin panel proposal](proposed-features/ADMIN-PANEL.md) for the
server-side pieces those need.
