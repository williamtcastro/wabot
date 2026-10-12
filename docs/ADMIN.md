# Admin panel (`wame admin`)

`wame admin` is a terminal panel for a wame server that runs somewhere else: a VPS, a home server,
a container. It talks to that server only through the [HTTP API](API.md), so it doesn't need the
database file, doesn't open a WhatsApp connection and can run alongside `serve` or `tui`.

```sh
export WAME_TOKEN=…                          # the server's owner secret, or a token with the admin scope
wame admin -server https://wame.example.com
```

| Tab | Shows | Keys |
|---|---|---|
| **1 Status** | connection state, linked account, a QR code to scan while the server isn't linked, the pairing code after `p` | `p` pair with a phone number, `L` log out (asks first) |
| **2 Tokens** | every token: scopes, chat allowlist, rate, expiry, last use, who created it; revoked and expired ones dimmed | `j`/`k` move, `n` new token, `x` revoke (asks first) |
| **3 Audit** | the audit log, newest first; failed actions in red with their error | `j`/`k`, `Ctrl+D`/`Ctrl+U`, `g`/`G` move, `/` filter, `Esc` clear the filter |

Everywhere: `1`–`3` or `Tab` / `Shift+Tab` switch tabs, `r` refreshes, `q` or `Ctrl+C` quits.
Status refreshes every 3 seconds, and so does the open Tokens or Audit tab (Audit only while
you're at the top and not filtering). Moving past the last audit row loads older entries.

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
`720h`; empty = never). The token is shown **once**, in a box; press `c` to copy it (system
clipboard, or the terminal's OSC 52 clipboard over SSH). Any other key closes it for good.

The server's rules apply as in [Tokens and scopes](API.md#tokens-and-scopes): with an admin token
(not the owner secret), the new token can't have the `admin` scope, can't outlive yours, and is
revoked along with it.

## Connecting safely

| Flag | Env | Default | |
|---|---|---|---|
| `-server URL` | `WAME_SERVER` | `http://127.0.0.1:8080` | the server's base URL; a path prefix behind a reverse proxy works (`https://example.com/wame`). Without a scheme, `https://` is assumed |
| `-token TOKEN` | `WAME_TOKEN` | | the owner secret or an admin token. Prefer the env variable: flags show up in `ps` |
| `-insecure` | | `false` | allow plain `http://` to a non-loopback address |

The token goes in every request, so the panel refuses plain `http://` to anything but
`localhost`/`127.0.0.1`/`::1`. Two good setups:

- **SSH tunnel** (nothing exposed): keep the server on `127.0.0.1:8080` and run
  `ssh -N -L 8080:127.0.0.1:8080 you@server`, then `wame admin` with the default `-server`.
- **HTTPS reverse proxy** (Caddy, nginx, Traefik) in front of `wame serve -addr 127.0.0.1:8080`,
  then `wame admin -server https://wame.example.com`.

Use `-insecure` only on a network you trust (a VPN, Tailscale): the token would otherwise be
readable by anyone on the path.

A token without the admin scope can still open the panel and see the Status tab (without the QR),
but the Tokens and Audit tabs say it needs the admin scope. Prefer a dedicated admin token with an
expiry over the owner secret, so it can be revoked on its own:

```sh
wame token create ops-laptop -scopes admin -expires 720h   # on the server
```

## What it doesn't do (yet)

No chats or messages (use `wame tui` on the server, or the API), no webhook or rate-limit health,
no editing server settings. The [web admin panel proposal](proposed-features/ADMIN-PANEL.md)
lists the server-side pieces those need; once they exist they can become tabs here too.
