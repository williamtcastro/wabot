# Docker

The image runs `wame serve` on port 8080 as a non-root user (uid 65532) in a distroless image:
no shell, no package manager, just the static binary and CA certificates. The database lives in
`/data/wame.db` (`WAME_DB`), and `/data` is a volume, so the session survives container rebuilds.

```sh
docker build -t wame --build-arg VERSION=$(git describe --tags --always) .
```

Inside the container, the API listens on `0.0.0.0:8080`, so wame refuses to start without a token
(`WAME_TOKEN`, 16+ characters, or a token created with `wame token create`). Publish the port on
`127.0.0.1` only and put HTTPS or an SSH tunnel in front of it (see [ADMIN.md](ADMIN.md#connecting-safely)).

> [!IMPORTANT]
> `/data` holds your WhatsApp session keys: whoever can read it can act as your account. Keep the
> volume or folder private, and keep it out of images, git and shared backups.

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
echo "WAME_TOKEN=$(openssl rand -hex 24)" > .env    # keep .env out of git
docker compose run --rm wame login
docker compose up -d
```

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
```

Commands that connect to WhatsApp (`login`, `send`, `logout`, `mcp`, `tui`) need the session to
themselves: stop the container first (`docker stop wame`), or use the HTTP API or `/mcp` of the
running one. Server flags go after `serve`, e.g. `docker run … wame serve -rate 10 -autoreply=false`,
or as environment variables (`WAME_RATE`, `WAME_WEBHOOK`, …; see [CLI.md](CLI.md)).

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

Stop the container (or accept a backup that may miss the last seconds of writes), then:

```sh
docker run --rm -v wame-data:/data:ro -v "$PWD":/backup alpine tar czf /backup/wame-data.tgz -C /data .
```

For a host folder, back up the folder. Either way the archive holds the session keys: encrypt it.
