# Docker

The image runs `wame serve` on port 8080 as a non-root user (uid 65532) in a distroless image:
no shell, no package manager, just the static binary and CA certificates. The database lives in
`/data/wame.db` (`WAME_DB`), and `/data` is a volume, so the session survives container rebuilds.

```sh
docker build -t wame --build-arg VERSION=$(git describe --tags --always) .
```

Inside the container, the API listens on `0.0.0.0:8080`, so wame refuses to start without a token:
an owner secret in `WAME_TOKEN`, or a token created with `wame token create` (see [Tokens](#tokens)).
Publish the port on `127.0.0.1` only and put HTTPS or an SSH tunnel in front of it (see [ADMIN.md](ADMIN.md#connecting-safely)).

> [!IMPORTANT]
> `/data` holds your WhatsApp session keys: whoever can read it can act as your account. Keep the
> volume or folder private, and keep it out of images, git and shared backups.

## Tokens

There are two kinds, and both work for the API, `/mcp` and the [admin panel](ADMIN.md).

### Owner secret

A random string of 16+ characters that **you** make up; wame doesn't generate it. The same value
goes to the server and to whoever connects to it:

```sh
openssl rand -hex 24                                    # prints e.g. 3f9c…e1a7
cp .env.example .env                                    # then set WAME_TOKEN=<that value> in it
docker run … -e WAME_TOKEN=<that value> … wame          # or pass it to docker run
```

It has full access and changes only when you change the server's setting, so keep it private.
Anyone who can run `docker inspect` on the host can read it from the container's environment.

### Named tokens

wame generates these. They can be revoked on their own, expire, and carry narrower scopes. Create
them on the running container (the command only touches the database, so it's safe while serving):

```sh
docker exec wame wame token create my-laptop -scopes admin -expires 720h
docker exec wame wame token list
docker exec wame wame token revoke my-laptop
```

The token (`wame_…`) is printed **once**; copy it right away. Then, on your laptop:

```sh
export WAME_TOKEN=wame_…
wame admin -server https://your-server       # or through an SSH tunnel, see below
```

From the panel's Tokens tab (`n`) you can then create tokens for bots and agents, like `read` or
`send` limited to some chats. Only the owner secret can create `admin` tokens from the panel.

You can also skip the owner secret: create a named token before the first start, and the server
starts without `WAME_TOKEN` because a token already exists.

```sh
docker run --rm -v wame-data:/data wame token create my-laptop -scopes admin -expires 720h
docker run -d --name wame --restart unless-stopped -p 127.0.0.1:8080:8080 -v wame-data:/data wame
```

With Compose, remove the `WAME_TOKEN` line from `compose.yaml` for that (it's required there), and
run `docker compose run --rm wame token create …` first.

## Choose where the database lives

| | Docker volume (internal) | Host folder (external) |
|---|---|---|
| Mount | `-v wame-data:/data` | `-v "$PWD/data":/data` |
| Where | managed by Docker (`docker volume inspect wame-data`) | a folder you choose |
| Permissions | set up by the image | the folder must be writable by the container user (below) |
| Good for | a server you manage with Docker only | backing up with your usual tools, inspecting with `sqlite3`, moving an existing DB in |

### Docker volume

```sh
export WAME_TOKEN=$(openssl rand -hex 24)

docker run -it --rm -v wame-data:/data wame login        # scan the QR, then Ctrl+C
docker run -d --name wame --restart unless-stopped \
  -e WAME_TOKEN -p 127.0.0.1:8080:8080 -v wame-data:/data wame
```

A new volume is created owned by the image's user, so there's nothing to set up.

### Host folder

The container runs as uid 65532, which can't write to a folder owned by you. Either run it as
yourself:

```sh
mkdir -p data && chmod 700 data
docker run -it --rm --user "$(id -u):$(id -g)" -v "$PWD/data":/data wame login
docker run -d --name wame --restart unless-stopped --user "$(id -u):$(id -g)" \
  -e WAME_TOKEN -p 127.0.0.1:8080:8080 -v "$PWD/data":/data wame
```

or hand the folder to the image's user: `sudo chown 65532:65532 data` and drop `--user`. (Docker
Desktop on macOS maps ownership for you; both work there.) Don't use `--user` with a Docker volume:
the volume belongs to 65532 and your uid can't write to it.

## Docker Compose

[`compose.yaml`](../compose.yaml) builds the image and uses a Docker volume; the comment in it shows
the one-line switch to a host folder (add `user: "1000:1000"` with your uid and gid, or `chown` the
folder as above).

```sh
cp .env.example .env    # set WAME_TOKEN (openssl rand -hex 24); .env is git-ignored
docker compose run --rm wame login
docker compose up -d
```

[`.env.example`](../.env.example) lists the main keys: `WAME_TOKEN` (required), and the optional
webhook, rate and log settings. Compose passes those to the container, and also the queue's
(`WAME_QUEUE_JITTER`, `WAME_QUEUE_QUIET_HOURS`, `WAME_QUEUE_TIMEZONE`, `TZ`) and the alerts' if
set. `WAME_SERVER` is the URL `wame admin` connects to; the server ignores it. Run `wame admin` from the folder with the `.env`
and it reads `WAME_SERVER` and `WAME_TOKEN` from it; or save them in `~/.config/wame/admin.env` to
use from anywhere ([details](ADMIN.md#saving-the-server-and-token)).

## Logging in without a terminal on the server

Start the container without logging in first. It waits for a login and prints the QR in its logs
(`docker logs -f wame`, scannable as is), or use the [admin panel](ADMIN.md) from your own machine, which draws the
QR and can ask for a pairing code instead:

```sh
ssh -N -L 8080:127.0.0.1:8080 you@server &
WAME_TOKEN=… wame admin
```

## Everyday commands

```sh
docker logs -f wame                                                   # app logs
docker exec wame wame token create claude -scopes read -expires 720h  # safe while serving
docker exec wame wame token list
docker exec wame wame backup /data/backups/                           # safe while serving
```

Commands that connect to WhatsApp (`login`, `send`, `logout`, `mcp`, `tui`) need the session to
themselves: stop the container first (`docker stop wame`), or use the HTTP API or `/mcp` of the
running one. Server flags go after `serve`, e.g. `docker run … wame serve -rate 10 -autoreply=false`,
or as environment variables (`WAME_RATE`, `WAME_WEBHOOK`, …; see [CLI.md](CLI.md)).

The rate limits, auto-replies and webhook filter can also be changed without recreating the
container, from the [admin panel](ADMIN.md#changing-settings)'s Settings tab or
`PATCH /settings`. Those changes are stored in `/data` and **win over the container's environment
variables** on every restart until they're reset there, so check the Settings tab if a changed
`WAME_RATE` seems to be ignored.

The container's clock is UTC unless you set `TZ` (like `TZ=America/Sao_Paulo`; the time zone data
is built into wame, so this works in the distroless image). That matters for the send queue's quiet
hours: `WAME_QUEUE_QUIET_HOURS=21:00-08:00` is read on the clock of `WAME_QUEUE_TIMEZONE`, or of
`TZ` when that's unset, so without either a window meant for São Paulo starts three hours early.
Compose passes all three through from `.env`; see [QUEUE.md](QUEUE.md).

`docker stop` shuts wame down cleanly: it stops taking requests, ends open event streams, gives
in-flight requests up to 5 seconds and queued webhook deliveries one last attempt, then closes the
database. Messages in the [send queue](QUEUE.md) stay in `/data` and continue after the restart.
Docker's default 10-second stop timeout is enough.

## Alerts and monitoring

### Gotify

To get a push notification when WhatsApp disconnects or logs out, the send queue backs off or the
webhook loses deliveries, set these in `.env` (Compose passes them through; with `docker run`, use
`-e`):

```sh
WAME_ALERT_GOTIFY=http://10.0.0.5:8070     # your Gotify server, without ?token=
WAME_ALERT_GOTIFY_TOKEN=…                  # an app token created in Gotify
WAME_ALERT_DELAY=2m                        # optional: how long a disconnection lasts before it alerts
```

Check it with `curl -H "Authorization: Bearer $WAME_TOKEN" -X POST localhost:8080/alerts/test`.
What each alert means: [API.md → Alerts](API.md#alerts).

### Prometheus

`GET /metrics` serves Prometheus metrics ([list](API.md#get-metrics)) and requires an admin token.
Create one for Prometheus and keep it in a file it can read:

```sh
docker exec wame wame token create prometheus -scopes admin   # prints the token once
echo -n 'wame_…' > /etc/prometheus/wame-token && chmod 600 /etc/prometheus/wame-token   # paste it here
```

```yaml
scrape_configs:
  - job_name: wame
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/wame-token
    static_configs:
      - targets: ["10.0.0.5:8080"]
```

The port is published on `127.0.0.1` by default; scrape from the same host, or publish it on an
address Prometheus can reach (a token is required either way). Useful alert rules:
`wame_connected == 0` for a few minutes, `increase(wame_webhook_failed_total[15m]) > 0`,
`wame_queue_cooldown == 1`.

## Queue files

Files uploaded for the [send queue](QUEUE.md#files) are kept in `/data/wame-queue-files/` (directory `0700`,
files `0600`, named by random ids), next to the database, until they are sent or pruned. They sit
there unencrypted, so treat `/data` accordingly. `wame backup` copies the database only; the files
aren't in a backup, and after a restore the queued items that need one fail with "file is gone".
`wame logout` removes the whole directory.

## Moving an existing database in

Stop whatever uses it, and copy `wame.db` with its `-wal` and `-shm` files if they exist.

```sh
# into a host folder
cp ~/.config/wame/wame.db* data/

# into a Docker volume
docker run --rm -v wame-data:/data -v ~/.config/wame:/src:ro alpine \
  sh -c 'cp /src/wame.db* /data/ && chown -R 65532:65532 /data && chmod 700 /data'
```

Don't run the old copy afterwards: two processes using one WhatsApp session log each other out.

## Backups

Use [`wame backup`](CLI.md#wame-backup--keep-n-path) inside the running container. It copies the
database through a read-only connection with SQLite's `VACUUM INTO`, so there's no need to stop
anything, and it works in the distroless image (no shell or `sqlite3` needed):

```sh
docker exec wame wame backup /data/backups/            # → /data/backups/wame-YYYYMMDD-HHMMSS.db
docker exec wame wame backup -keep 7 /data/backups/    # and keep only the 7 newest there
docker cp wame:/data/backups/wame-20261013-093005.db .  # take a copy off the volume
```

`docker exec` runs as the container's user (uid 65532, or your `--user`), which is the one that owns
`/data`, so the backup folder can live there; it's created `0700` on the first run and each copy is
`0600`. `WAME_DB` is already set in the image, so there's no `-db` to pass. The backup copies the database only, not `/data/wame-queue-files/` (see [Queue files](#queue-files)). Copying `wame.db` with
`cp` or `tar` while the server runs can catch it halfway through a write; `wame backup` can't.

For a nightly copy, run the same `docker exec` line from the host's cron or a systemd timer, with
`-keep` so old ones go away. With a host folder the copies land in it, ready for your usual backup
tool. Either way they hold the session keys: encrypt anything that leaves the machine.

To restore, stop the container, replace `wame.db` with the copy and delete `wame.db-wal` and
`wame.db-shm`, as in [Moving an existing database in](#moving-an-existing-database-in).
