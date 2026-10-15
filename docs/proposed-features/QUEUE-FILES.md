# Files in the send queue (design record)

> **Built (2026-10-15), with option C.** This is the design record; how to use it is in
> [QUEUE.md](../QUEUE.md#files) and [API.md](../API.md#post-queuefiles-get-queuefiles-delete-queuefilesid).
> The text below is the proposal as written, with the few places the build differs corrected; see
> [Decisions](#decisions-2026-10-15). Before it, the queue took text only. See the
> [list of proposals](README.md).

## Goal

Queue images and documents the way text is queued today: stored, paced by the same limits,
surviving restarts, cancelable, followed through `GET /queue` and `queue` events. The main case is
one file (a flyer, a PDF menu) to many recipients, so a file should be uploaded to wame once and,
where safe, to WhatsApp once.

Non-goals for v1: audio, video, stickers, voice notes; albums; editing a file after queuing;
serving the file back to anyone.

## API shape

Three options:

| Option | For | Against |
|---|---|---|
| **A. Multipart `POST /queue/file`**: the file plus a JSON `messages` field in one request | One call; no orphaned uploads | Every retry re-sends the whole file; the idempotency fingerprint has to hash it; mixes two body formats; a second batch with the same file uploads it again |
| **B. JSON with base64** inside `POST /queue` | One route, one body format | +33 % size; the 16 MB body cap would have to grow; the whole body is decoded in memory by `readJSON`; models and scripts paste huge strings |
| **C. Upload a blob, then reference its id** (`POST /queue/files` → `file_id`, then `POST /queue` with `"file": "f_…"`) | File sent once, used by any number of messages and batches; `POST /queue` stays small JSON with today's idempotency; natural dedupe; MCP can reference an id without moving bytes | Two calls; uploads nobody references need cleanup |

**Recommendation: C.** It matches the bulk case, keeps `POST /queue` unchanged in shape, and the
orphan problem is a timer.

```sh
curl -H "Authorization: Bearer $T" -F file=@menu.pdf localhost:8080/queue/files
# 201 {"id": "f_9c2e…", "sha256": "…", "size": 482113, "mime": "application/pdf",
#      "name": "menu.pdf", "kind": "document", "expires_at": "…"}

curl -H "Authorization: Bearer $T" -X POST localhost:8080/queue -d '{
  "batch": "menu-october", "file": "f_9c2e…", "text": "Our new menu",
  "messages": [{"to": "5511999999999"}, {"to": "5511988887777", "text": "Hi John, the menu"}]}'
```

- `file` (top level or per message) turns `text` into the caption, which may then be empty.
- `GET /queue/files` (own files; admin: all, without names of others), `DELETE /queue/files/{id}`
  (409 while queued items reference it; cancel them first). No route returns the content.
- All under `send`, own files only unless `admin`, like `/queue`.

## Storage

| | Files on disk | BLOB column |
|---|---|---|
| Atomic with the queue row | no (write file, then row; sweep orphans) | yes |
| DB size | unchanged | grows by every file; WAL churn; space only back after `VACUUM` |
| Backups | must copy `wame-queue-files/` too | included |
| Streaming upload to WhatsApp | from the file | whole BLOB in memory |

**Recommendation: disk.** `<db dir>/wame-queue-files/<id>` (so `/data/wame-queue-files/` in Docker), directory 0700,
files 0600, created with `O_EXCL`. The name on disk is the server's random id (`f_` + 16 random
bytes hex), never anything the client sent, so there's no path to traverse. Metadata goes in a new
table:

```sql
CREATE TABLE IF NOT EXISTS wame_outbox_files (
	id          TEXT PRIMARY KEY,
	actor       TEXT NOT NULL,
	token_id    TEXT NOT NULL DEFAULT '',
	sha256      TEXT NOT NULL,
	size        INTEGER NOT NULL,
	mime        TEXT NOT NULL,           -- sniffed, not the client's header
	kind        TEXT NOT NULL,           -- image | document
	name        TEXT NOT NULL DEFAULT '',-- display name only, cleaned
	created_at  INTEGER NOT NULL,
	upload      BLOB,                    -- cached whatsmeow.UploadResponse fields (see below)
	uploaded_at INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER NOT NULL DEFAULT 0 -- set when the bytes are gone; the row stays for the items
);
CREATE UNIQUE INDEX IF NOT EXISTS wame_outbox_files_dedupe ON wame_outbox_files (actor, sha256) WHERE deleted_at = 0;
```

plus `file_id`, `file_kind` and `file_size` (`TEXT`, `TEXT`, `INTEGER`, all defaulting to empty or 0) on `wame_outbox`, added idempotently in `migrateOutbox`.

Limits: 64 MB per file (`maxUpload`, same as `/send/file`; WhatsApp caps images at about 16 MB, so larger ones go as
documents),
at most 50 files and 500 MB per token, 2 GB in all (setting `queue_files_quota`). Over quota → 413
with what's used. Disk-full errors fail the upload, never a send.

**Cleanup**, in `pruneOutbox` (hourly): a file with no open item (queued or sending) is deleted
1 hour after its last item finished; a file never referenced is deleted 24 hours after upload
(`expires_at`). The row stays until its items are pruned (7 days), with `size` and `sha256` only,
so `GET /queue` still shows what was sent. On startup, files in `wame-queue-files/` without a row are removed.

## Upload to WhatsApp once, or per recipient

`Client.Upload` encrypts with a random media key and returns `URL`, `DirectPath`, `MediaKey`,
`FileEncSHA256`, `FileSHA256`, `FileLength`. Those fields are all a recipient needs, and nothing in
them is tied to a recipient: WhatsApp's own forwarding sends the same fields to new chats without
re-uploading. So one upload can serve many sends.

**Recommendation: reuse, with a short lifetime.** The worker uploads on the first due item that
needs the file, stores the response in `upload`/`uploaded_at`, and reuses it for later items while
it's younger than `queue_media_ttl` (default 24 h; WhatsApp doesn't document how long the CDN keeps
an object, and recipients download lazily, so stay well short of the guesses of "weeks"). Older →
upload again. A per-recipient upload remains the fallback (setting off = always upload) if reuse
turns out to misbehave.

Risks:

- **Expired media.** A recipient who opens the chat late may get "download failed" and ask the
  sender to re-upload (a media retry receipt). wame doesn't answer those today, for `/send/file`
  either. Open question below; the TTL keeps the window small.
- **Bulk signal.** The same encrypted object to hundreds of unknown numbers is an obvious broadcast
  pattern. Pacing helps; per-recipient uploads wouldn't hide it (`FileSHA256` is the plaintext hash
  either way).
- **Key exposure.** The cached media key decrypts the CDN object. It lives in the DB next to the
  session keys, which are worse to lose; it's never returned by any route.

## Dedupe

`sha256` of the bytes, per actor: uploading the same file twice returns the existing id (200
instead of 201). Not across actors: answering "already have it" for another token's file would be
an oracle for what others sent. The WhatsApp upload cache is per file row, so two actors sending
the same flyer upload it twice; acceptable.

## Pacing

- A file message is one send for the global limiter, the per-chat gap and the token's rate, the
  same slot a text item takes. No discount for reuse.
- The upload runs **before** the worker takes a slot, so a slow upload doesn't hold a slot
  interactive sends need. The worker is single-threaded, so at most one upload runs at a time.
- Optionally a byte budget (`queue_upload_mb_per_min`) so a 64 MB file every few seconds can't
  saturate a home uplink; the worker waits for the budget like it waits for a slot.

## Retries

- Upload error → the item goes back to queued with the normal backoff (counts as an attempt, like a
  send error), and counts toward the whole-queue cooldown.
- Send error after a reused upload → drop the cached upload (`uploaded_at = 0`) before the retry,
  so the next attempt re-uploads. Cheap insurance against an expired object.
- The item keeps its message id across attempts as today; a retry with a fresh upload is the same
  message to WhatsApp's dedupe.
- The file missing from disk (deleted by hand) → the item fails with "file is gone", no retry.

## Security

- **MIME from the bytes**, `http.DetectContentType` (as `sendFile` does), never the client's
  `Content-Type`. v1 allow-list: JPEG, PNG, WebP as images; PDF, plain text, ZIP-based office
  formats, CSV as documents; everything else 415. Image dimensions checked with `image.DecodeConfig`
  only (no full decode) and capped.
- **Names**: the client's file name is display metadata only: base name, invisible and control
  characters dropped (the MCP `clean()` rules), 255 bytes max, never used as a path.
- **Never served back.** No content route. `GET /queue/files` shows other actors' rows to admin
  without `name`; file names count as text under the existing rule (events, webhook `queue`
  deliveries, audit details and the admin panel never carry them).
- **Allowlists**: the file isn't tied to a chat; each message's `to` is checked as today
  (`allowedChat`, no lookups for restricted tokens). A token can only reference its own files.
- **Revocation**: items from a revoked or expired token fail as today, and its files are deleted at
  the next prune.

## Privacy

Files are deleted as soon as nothing open needs them (above). `wame logout` removes `wame-queue-files/`
entirely. The message stored in history after a send keeps only what any sent file
keeps today (type, mime, size, name), not the bytes. Docs must say plainly that queued files sit on
the server's disk until sent.

## Admin panel and MCP

- **Admin panel** Queue tab: a type column (`text`, `image`, `document`) and size; never the name or
  a preview. Health: files count and bytes used against the quota.
- **MCP**: no upload tool (models shouldn't push megabytes of base64 through a context window).
  `schedule_message` gains an optional `file` id that the user uploaded through the API or the
  admin panel; the same scopes, allowlist and own-files rules apply. `list_scheduled` shows `kind`
  and size.

## Migration

`migrateOutbox` creates `wame_outbox_files` and adds `wame_outbox.file_id`, `file_kind` and `file_size` idempotently; existing
rows get `''` (text). The `wame-queue-files/` directory is created on first upload with 0700. Older servers
answer 404 on `/queue/files`; the admin panel shows "update the server". `docs/openapi.yaml`,
`docs/API.md`, `docs/QUEUE.md` and the audit tables (`queue_file`, `queue_file_delete`: target the
file id, detail `kind, N bytes`, never the name) are updated with the code.

## Where the code goes

- `internal/store/outbox_files.go`: the table, `InsertOutboxFile` (dedupe), `OutboxFile`,
  `ListOutboxFiles`, `SetOutboxUpload`, `OrphanOutboxFiles`.
- `internal/bot/outbox_files.go`: `AddQueueFile` (sniff, quota, write 0600, row), the upload cache,
  pruning; `sendQueued` branches to a `sendFile`-like path that takes a cached upload instead of
  bytes (split today's `sendFile` into upload + build-message so both paths share it).
- `internal/api/queue.go`: `/queue/files` routes and `file` in `enqueue`.

## Test plan

- store: dedupe per actor (same hash, two actors → two rows), orphan listing, migration on an
  existing outbox.
- bot: MIME sniffing ignores the client type; HTML/SVG/executables refused; quota per token and in
  all; the disk name never contains client input (`../../x`); files 0600 and dir 0700; reuse within
  TTL (one `Upload` for three items, via an upload func the test swaps in), re-upload after TTL and
  after a send error; upload error backs off and counts an attempt; file deleted from disk fails the
  item; prune deletes finished and never-referenced files; revoked token's files go.
- api: scopes; restricted token can't reference another token's file (404, not 403, so ids don't
  leak); 413 over size or quota; 415; no route returns content; `GET /queue` never shows file names
  to other actors; idempotent `POST /queue` with `file`.
- admin: Queue tab type column, no names.

## Open questions

- Answer media retry receipts (re-upload on request) in v1, or rely on the TTL?
- Is 24 h the right default reuse lifetime? Measure with a test number how long a CDN object stays
  downloadable before raising it.
- Should a batch be able to reference a file another token uploaded (shared assets for a team), or
  is admin-only enough?
- Audio and video: same table, but WhatsApp wants specific codecs (OGG/Opus voice notes, MP4/H.264)
  and thumbnails; worth a separate step.
- Keep files when the queue is paused for days, or count paused time toward the orphan timer?

## Decisions (2026-10-15)

The open questions above, answered:

- **Media retry receipts**: not answered in v1. A recipient who asks for expired media to be sent
  again gets nothing; the reuse TTL keeps that window small.
- **Default reuse lifetime**: 24 h kept (`queue_media_ttl`, up to 168 h, `off` = upload for every
  message). Not measured against the CDN yet.
- **Sharing between tokens**: no. Files are own-actor only; admin sees every actor's files (without
  the others' names) and can delete any, but a batch can only reference the caller's own files, admin
  included. No team sharing.
- **Audio and video**: still a later step.
- **Paused queue**: paused time counts toward the 24 h orphan timer only for files no item ever
  used. A file that queued items need stays while those items are open, paused or not.

What was decided while building:

- The dedupe unique index is partial (`WHERE deleted_at = 0`), so a deleted file can be uploaded
  again; the row of a deleted file keeps only id, actor, size, sha256, mime and kind (`deleted_at`
  set, name and cached upload cleared).
- Kind and size are copied onto each `wame_outbox` row (`file_kind`, `file_size`), so lists, events
  and webhook deliveries need no join, and the file's name never reaches them.
- A trigger (`wame_outbox_file_live`) refuses an insert into `wame_outbox` that names a deleted or
  another actor's file. It closes the race between the check and a concurrent delete or prune;
  replays of an existing idempotency key are exempt. The idempotency fingerprint includes the file id.
- Quota is checked inside the `INSERT`, so parallel uploads can't both pass. Limits: 50 files and
  500 MB per actor (token name), and `queue_files_quota` in all (default 2 GB, `0` refuses uploads).
- The optional `queue_upload_mb_per_min` byte budget was not built.
- Upload errors have their URLs cut to the host before they are stored: the upload URL carries
  WhatsApp's auth token.
- Cleanup also deletes files of a revoked or expired token (their queued items fail with "file is
  gone"), sweeps files in `wame-queue-files/` without a row older than 15 minutes (instead of only at startup), and
  runs once when the worker starts as well as hourly. Logout, `POST /logout` and WhatsApp logging
  the device out delete every file and remove `wame-queue-files/` (audited as `queue_files_wipe`).
- Backups (`wame backup`) copy the database only, not `wame-queue-files/`.
- Files that are queued are audited as `send_file` with `kind, N bytes, queue #id` (no name); uploads
  and deletes as `queue_file` and `queue_file_delete`.
- Types are sniffed from the bytes: JPEG, PNG, WebP; PDF, plain text, CSV, and the Office ZIP
  formats (`.docx .xlsx .pptx .odt .ods .odp`, checked for `[Content_Types].xml` or `mimetype`);
  text starting with `<` is refused; the name's extension must match the content.
- The upload is made right before the send, one at a time, only when the item's slot is about to be
  free; it is kept in memory for the item it was made for for a short while. A fresh upload ends
  the worker's step, so pause, quiet hours and the token are checked again before the send.
- An upload error while the connection is down gives the attempt back (as a text send does); a
  local read error backs off without the whole-queue cooldown.
- From the adversarial review: per-actor quotas are checked before anything is written to disk; at
  most two uploads per token and eight in all run at once (429); Office archives are checked by
  walking at most 10,000 central-directory headers (not `zip.OpenReader`, which allocates one per
  entry); text must be UTF-8 and may not start with `<` after any whitespace; errors never quote the
  client's extension or a server path; the total quota error doesn't show other tokens' usage; an
  upload of the same bytes restores a stored copy that went missing on disk.

For usage see [QUEUE.md](../QUEUE.md#files).
