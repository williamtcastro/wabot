# Backlog: smaller ideas

Ideas that came up while building the send queue, alerts and admin panel, too small for a full
proposal each. None of this exists yet. Each entry says what it would do, where it would live, and
what to watch for. Pick one up, write the plan into its own proposal if it grows, and delete the
entry when it's built.

## Operations

### Scheduled backups inside wame

`-backup-every 24h -backup-keep 7` (and env/setting equivalents) runs the same read-only
`VACUUM INTO` as `wame backup` on a timer, into `<db dir>/backups/` (`/data/backups/` in Docker).
A failure alerts through Gotify. No cron or host job needed.

- Lives in `internal/bot` (a `RunBackups` worker started through `b.Go` in `serve`/`tui`), reusing
  `store.Backup` and `store.PruneBackups`.
- Show the last backup (time, size, result) in `/stats` and on the Health tab; `/metrics` gets
  `wame_backup_last_success_timestamp`.
- Watch for: disk space (refuse when free space is below the DB size × 2), backups landing on the
  same disk as the DB (document copying them off-box), the 0600 mode.

### Docker health check

`wame health` exits 0 when the server answers and WhatsApp is connected (or is waiting for a QR),
non-zero otherwise. The distroless image has no shell or curl, so the check must be in the binary.
`HEALTHCHECK CMD ["/wame", "health"]` in the Dockerfile; Unraid then shows healthy/unhealthy.

- Talks to `127.0.0.1:8080/status`. Needs a token: either a read token in an env var the check can
  use, or a token-less `/healthz` that reveals only "up / connected / logged in" (no JID, no QR).
  The second is simpler; it must stay loopback-only.
- Watch for: a check that flaps during the reconnects WhatsApp does on its own (allow a grace period).

### Memory that survives restarts

Webhook failures, per-token usage (24 h buckets) and alert history are in memory and reset on every
restart, so the Health tab and `/stats` look empty after a deploy.

- Persist small rolling tables (`wame_usage_hourly`, `wame_webhook_failures` capped at 100,
  `wame_alert_log` capped at 100) and load them on start.
- Watch for: write volume (batch usage writes, e.g. once a minute), never storing message text or
  the webhook URL, pruning.

## Send queue

### Retry failed items

Re-queue failed items from the Queue tab (`r` on one, `R` on its batch) or the API
(`POST /queue/{id}/retry`, `POST /queue/retry?batch=`). Resets attempts, keeps the message id (so a
copy that did arrive isn't shown twice), audited as `queue_retry`.

- Skip items that failed because the recipient opted out, isn't on WhatsApp, or the token was revoked:
  retrying those can't succeed. Say how many were skipped and why.

### Batch view

A per-batch summary on the Queue tab: one row per batch with queued / sent / delivered / read /
failed / opted out, started and last activity, and who queued it. `Enter` opens the batch's items.
`GET /queue/batches` returns the same. Mostly a `GROUP BY batch` over `wame_outbox`.

### Files in the queue

See [QUEUE-FILES.md](QUEUE-FILES.md).

## Media

### Keep incoming media (auto-download)

Today an incoming photo, video, audio or document is stored as metadata plus its encrypted media
key; `GET /media/{seq}` downloads and decrypts it from WhatsApp **on every request**. WhatsApp only
keeps media for a limited time (weeks), after which `/media` answers 410 and the file is gone.

- Optional `-media-keep` (off by default): download incoming media in the background (through `b.Go`,
  never in the event handler) into `<db dir>/media/<seq>`, 0600, then serve `/media/{seq}` from disk
  when present and fall back to WhatsApp otherwise.
- Limits: per-file cap (the existing 100 MB), types to keep (e.g. images and voice notes only), a
  total quota with oldest-first pruning, and a retention period. Downloads count against a small
  budget so a history sync doesn't saturate the link.
- Delete the file when the message is deleted for everyone (`MarkDeleted`), and everything on logout.
- Watch for: disk use on small servers, privacy (media at rest; same protections as the DB),
  backups (`wame backup` covers the DB only; document the media folder).

## Contacts

See [CONTACTS.md](CONTACTS.md): wame's own contact list (names, tags, notes) and a profile per
person, plus tags, `{name}` templates and dedupe in the queue.

## Integrations

### Starter n8n workflow

A ready-to-import n8n workflow JSON in `docs/` with a Webhook node at path `wame` (POST), a Code node
that checks `X-Wame-Signature` (HMAC-SHA256 of `timestamp.body` with the shared secret) and rejects
old timestamps, and a placeholder branch per `X-Wame-Event` (`message`, `update`, `queue`, `test`).
Turning the webhook on becomes: import, set the secret, activate, set `WAME_WEBHOOK`, press `t` on
the Health tab.

### ntfy as a second alert backend

`-alert-ntfy URL` (+ optional token) next to Gotify, through the existing `Notifier` interface.
Same rules: no message text, no contacts' numbers, the token only in a header, errors cleaned of the
address.

### Token expiry warnings

An alert and a badge on the Tokens tab a few days before a token expires (default 3 days, setting),
so an integration doesn't stop silently. Once per token per day at most.
