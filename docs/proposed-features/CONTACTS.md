# Contacts: your own list and a profile per person (proposal)

> **Phases 1 and 2 are built** (each phase ends with an "As built" list of where the code differs
> from this plan); phases 3 and 4 are still proposals. See the [list of proposals](README.md). This
> replaced the two contact entries that were in [BACKLOG.md](BACKLOG.md).

## Goals

- A small list wame owns: **your own name, tags, notes, language and time zone** per person, edited
  over the API, the admin panel, the TUI, or a CSV import.
- **One display name everywhere** (chats, TUI, admin, MCP, queue templates), from one function.
- **A profile per person** (`GET /contacts/{number}`): names from every source, what WhatsApp says
  (about, verified business name, business profile, photo id), and what wame already knows (first
  and last message, counts, unread, groups in common, queue results, opt-out, which tokens can see
  the chat).
- **Queue features built on it**: `to=tag:clients`, `{name}` templates, and the two dedupe options
  (`-dedupe`, `-skip-sent`).

## Non-goals

- **Never writing back to WhatsApp** or the phone's address book. whatsmeow's `ContactStore` has
  `PutContactName` / `PutPushName`; wame must not call them. Your names live in `wame_contacts` only.
- Not a CRM: no pipelines, deals, activity logs, custom fields, reminders or per-contact files.
- No automatic contact creation from people who write to you. The unified name already falls back
  to the push name; a row exists only when someone sets a field.
- No bulk WhatsApp profile refreshes (a ban risk). Lookups are one person at a time, asked for by an
  admin.
- No photo storage by default (see [Privacy](#privacy-and-security)).

## What exists today (verified)

| Piece | Where | Notes |
|---|---|---|
| Address book list | `internal/bot/contacts.go: Bot.Contacts` | `GetAllContacts`, phone JIDs only. `contactName` is `cmp.Or(FullName, FirstName, BusinessName, PushName)`, so push-name-only entries **are** listed (BACKLOG said otherwise) and show the push name without `~` |
| Display name | `internal/bot/contacts.go: Bot.ChatName` | group subject, `Me`, `contactName`, `~push`, `+number`. Used by `Bot.Chats` (callback into `store.Chats`) and `internal/tui/model.go` (open chat) |
| Lookups | `Bot.lookup` → `Bot.Lookups` (3 × rate per minute) | used by `Resolve` (cache miss), `Check`, `SetTyping` |
| Group cache | `internal/store/state.go: wame_groups` | **name and admins only, no members**. `refreshGroups` has `j.Participants` but stores only admins, so "groups in common" needs a new column |
| Allowlists | `internal/bot/allow.go` | `LocalChat` (no network), `ChatAllowed`, `ExpandAllowlist` (resolves the list's own entries) |
| LID relink | `internal/store/store.go: relinkLIDs` | one tx; moves `wame_messages`, `wame_chat_state`, `wame_optouts` rows from `…@lid` to the phone JID |
| A small keyed table | `internal/store/optout.go` | the model for `wame_contacts`: `migrateOptOuts`, `ON CONFLICT`, `relinkLater`, list with `relinkLIDs()` first |
| Routes | `internal/api/api.go` | `GET /contacts` is `need(read, unrestricted(...))`; `POST /check` is `need(send, unrestricted(...))` |
| MCP | `internal/mcpserver/server.go: findContact` | read scope, refuses restricted callers, names under `untrusted` through `clean()`, 50 results |

whatsmeow (`go.mau.fi/whatsmeow@v0.0.0-20261007111105-c386243a72ba`):

| API | File: function | Use |
|---|---|---|
| `ContactStore.GetContact(ctx, JID) (types.ContactInfo, error)`, `GetAllContacts(ctx) (map[JID]ContactInfo, error)` | `store/store.go: ContactStore` | address-book names (`FirstName`, `FullName`), `PushName`, `BusinessName`, `RedactedPhone` |
| `LIDStore.GetPNForLID`, `GetLIDForPN`, `GetManyLIDsForPNs` | `store/store.go: LIDStore` | LID ↔ phone (already behind `Bot.PhoneJID`, `optOutKeys`) |
| `Client.GetUserInfo(ctx, []JID) (map[JID]types.UserInfo, error)` | `user.go: GetUserInfo` | `Status` (about), `PictureID`, `VerifiedName`, `LID` (it also stores LID mappings). Batched usync |
| `Client.GetProfilePictureInfo(ctx, JID, *GetProfilePictureParams) (*types.ProfilePictureInfo, error)` | `user.go: GetProfilePictureInfo` | params `Preview`, `ExistingID` (returns `nil, nil` when unchanged), `IsCommunity`, `CommonGID`, `InviteCode`, `PersonaID`. Errors `ErrProfilePictureUnauthorized` (hidden), `ErrProfilePictureNotSet` (`errors.go`) |
| `Client.GetBusinessProfile(ctx, JID) (*types.BusinessProfile, error)` | `user.go: GetBusinessProfile` | `Address`, `Email`, `Categories`, `ProfileOptions`, `BusinessHoursTimeZone`, `BusinessHours` |
| `Client.IsOnWhatsApp(ctx, []string) ([]types.IsOnWhatsAppResponse, error)` | `user.go: IsOnWhatsApp` | `IsIn`, canonical `JID`, `VerifiedName` |
| `events.Picture{JID, Author, Timestamp, Remove, PictureID}` | `types/events/events.go`, sent from `notification.go` | picture changed or removed |
| `events.UserAbout{JID, Status, Timestamp}` | `types/events/events.go`, `notification.go` | about changed |
| `events.PushName{JID, JIDAlt, Message, OldPushName, NewPushName}`, `events.BusinessName{…}` | `types/events/appstate.go`, `user.go` | whatsmeow already stored the new value |
| `events.Contact{JID, Timestamp, Action, FromFullSync}` | `types/events/appstate.go`, `appstate.go` | phone address-book edit; whatsmeow already called `PutContactName` |

## Data model

Two tables, created in a new `migrateContacts` (`internal/store/contacts.go`) called from `Open`
after `migrateOptOuts`. Idempotent `CREATE … IF NOT EXISTS`; later columns through `addColumns`.

```sql
CREATE TABLE IF NOT EXISTS wame_contacts (
	jid        TEXT PRIMARY KEY,           -- phone JID (5511999999999@s.whatsapp.net), or …@lid until relinked
	name       TEXT NOT NULL DEFAULT '',   -- your name for them, ≤ 100 runes, one line
	tags       TEXT NOT NULL DEFAULT '',   -- ",clients,vip," (delimited, sorted, lowercase)
	notes      TEXT NOT NULL DEFAULT '',   -- ≤ 2000 runes
	language   TEXT NOT NULL DEFAULT '',   -- BCP 47 like pt-BR, ≤ 16 chars
	time_zone  TEXT NOT NULL DEFAULT '',   -- IANA name, checked with bot.ParseTimezone
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,              -- actor, like wame_optouts.by
	updated_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS wame_contact_cache (
	jid            TEXT PRIMARY KEY,
	on_whatsapp    INTEGER NOT NULL DEFAULT -1,  -- -1 unknown, 0, 1
	about          TEXT NOT NULL DEFAULT '',     -- ≤ 700 runes (WhatsApp's limit is 139; cap anyway)
	about_at       INTEGER NOT NULL DEFAULT 0,   -- when they set it (event timestamp), 0 unknown
	verified_name  TEXT NOT NULL DEFAULT '',
	verified_level TEXT NOT NULL DEFAULT '',
	business       TEXT NOT NULL DEFAULT '',     -- JSON of the fields we keep, ≤ 8 KB
	picture_id     TEXT NOT NULL DEFAULT '',     -- '' none, '-' hidden from us
	picture_url    TEXT NOT NULL DEFAULT '',     -- signed CDN URL; expires; admin only
	info_at        INTEGER NOT NULL DEFAULT 0,   -- last GetUserInfo / IsOnWhatsApp
	picture_at     INTEGER NOT NULL DEFAULT 0,
	business_at    INTEGER NOT NULL DEFAULT 0,
	error          TEXT NOT NULL DEFAULT '',     -- last refresh error, cleaned, ≤ 200 chars
	error_at       INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS wame_contact_cache_updated ON wame_contact_cache (updated_at);
```

- **Tags as a delimited column, not a join table.** Tags match `^[a-z0-9][a-z0-9_-]{0,31}$`, at
  most 20 per contact, stored sorted as `,a,b,` so a filter is `instr(tags, ',' || ? || ',') > 0`
  (not `LIKE`: `_` is a wildcard there). No index; a scan of 10,000 rows is a few ms. One row per
  person keeps the relink and the delete simple. A join table becomes worth it past ~50k contacts.
- **No index on `name`.** The list is sorted in Go after merging with the address book anyway.
- **Keys.** Always the phone JID from `Bot.LocalChat` / `Bot.PhoneJID`, never as typed. A person only
  known by `…@lid` (a chat that came in that way) is stored under it and relinked.
- **Brazil's 9th digit.** A number typed with the 9 may be registered without it. Reads look up both
  variants locally (`phoneVariants(jid) []string` in `internal/bot/names.go`: for `55` + 2-digit
  area + `9` + 8 digits starting with 6 to 9, also the form without the 9, and the reverse; a
  landline's 8 digits start with 2 to 5, so a mobile never maps onto one). `PUT` by an admin while
  logged in resolves the number once through `CanonicalChat` (one lookup, not cached when the number
  isn't on WhatsApp, which is then stored as typed like `PATCH`); a CSV
  import never does (1000 lookups would be a ban risk): it updates an existing row under either
  variant, otherwise stores the number as typed.
- **Relink (built).** `relinkContacts` in `internal/store/contacts.go`, called by `relinkLIDs` in
  the same transaction, merges in Go: the phone row's name, language and time zone win when set;
  notes are both kept (phone row's first, joined by `\n`) when they differ, which may go past 2000
  characters; tags are united, phone row's first, capped at `MaxContactTags` at merge time so a
  later `PATCH` isn't refused; `updated_by` = `relink`, `created_*` the older row's. When it moved
  rows it bumps `Store.ContactsGen`, as every successful contacts write does. The sketch below was
  the first plan:

```sql
-- merge a @lid row into the phone row: phone row's non-empty fields win, tags are united
INSERT INTO wame_contacts (jid, name, tags, notes, language, time_zone, created_at, updated_at, created_by, updated_by)
	SELECT <pn>, name, tags, notes, language, time_zone, created_at, updated_at, created_by, updated_by
	FROM wame_contacts WHERE jid <mapped>
	ON CONFLICT (jid) DO UPDATE SET
		name = CASE WHEN wame_contacts.name = '' THEN excluded.name ELSE wame_contacts.name END,
		notes = CASE WHEN wame_contacts.notes = '' THEN excluded.notes ELSE wame_contacts.notes END,
		language = CASE WHEN wame_contacts.language = '' THEN excluded.language ELSE wame_contacts.language END,
		time_zone = CASE WHEN wame_contacts.time_zone = '' THEN excluded.time_zone ELSE wame_contacts.time_zone END,
		tags = wame_contacts.tags || excluded.tags,  -- normalized (dedupe/sort) by the next write; readers split and dedupe
		updated_at = MAX(wame_contacts.updated_at, excluded.updated_at);
DELETE FROM wame_contacts WHERE jid <mapped>;
UPDATE OR IGNORE wame_contact_cache SET jid = <pn> WHERE jid <mapped>;
DELETE FROM wame_contact_cache WHERE jid <mapped>;   -- a cache copy, losing it is fine
```

  (`<pn>` and `<mapped>` are the existing `pn` / `mapped` fragments, with `chat` replaced by `jid`.)
  `SetContact` calls `relinkLater` for `@lid` keys, like `AddOptOut`.
- **Retention.** `wame_contacts` rows live until deleted or until logout. `wame_contact_cache` rows
  not refreshed in 90 days are pruned (`Store.PruneContactCache(before)`, called from the existing
  daily `pruneOutbox` step). Logout (`wame logout`, `POST /logout`, or the phone unlinking the
  device) deletes own contacts, the cache and group members in one transaction (see Decision 2).
  Both tables are in `wame backup` like everything else.
- **Groups in common** need members: add `members TEXT NOT NULL DEFAULT '[]'` to `wame_groups`
  (through `addColumns` in `migrateState`), filled by `refreshGroups` with the phone JIDs of all
  participants (`PhoneJID` of `JID`, `PhoneNumber`, `LID`, like admins today).

## One name function

`internal/bot/names.go` (new), replacing the body of `ChatName`:

```go
type Names struct {
	Display  string `json:"name"`             // what every view shows
	Source   string `json:"name_source"`      // own | book | business | push | number | group | me
	Own      string `json:"own_name,omitempty"`
	Book     string `json:"book_name,omitempty"`
	Business string `json:"business_name,omitempty"`
	Push     string `json:"push_name,omitempty"`
}

func (b *Bot) NamesOf(ctx context.Context, chat, pushName string) Names
func (b *Bot) ChatName(ctx context.Context, chat, pushName string) string { return b.NamesOf(ctx, chat, pushName).Display }
```

Order: **own > phone address book (`FullName`, then `FirstName`) > business (`ContactInfo.BusinessName`,
then the cached `verified_name`) > push name (shown as `~Name`) > `+number`**. Groups keep the
subject (`group <id>` until fetched), your own chat stays `Me`. No network, ever: own names come from
an in-memory map, the rest from whatsmeow's store and the cache table.

- **Own-name map.** `Bot.own` (a `map[string]ownContact` under `b.mu`, loaded lazily from
  `Store.Contacts()` on first use and replaced on every write through a `Bot` method) so `/chats`
  with 1000 chats doesn't run 1000 queries. One process owns the DB, so there's no outside writer.
- **Behaviour change:** push-name-only entries now show as `~Name` everywhere (today `contactName`
  returns the push name unmarked in some paths and marked in others). Mention in the changelog.
- **Callers:** `Bot.Chats` (unchanged signature), `Bot.Contacts`, the TUI open-chat path,
  `findContact`, the profile, `{name}` templates (with the push-name exception below), the
  admin panel (through `/chats` and `/contacts`). The webhook and events never carry names they
  don't carry today.
- **Reloads:** on `*events.Contact`, `*events.PushName` and `*events.BusinessName`, and after any own
  write, publish `NamesChanged{}` (the TUI already reloads its chat list on it). Debounce: at most
  one per second (history sync fires many `PushName`s).

## Profile

`Bot.Profile(ctx, chat string, opt ProfileOptions) (Profile, error)` in `internal/bot/profile.go`
(new). `chat` is already a local key (the API resolves it; see the rules below).

```go
type ProfileOptions struct {
	Full    bool   // admin: notes, stats, tokens, business, picture URL
	Refresh bool   // admin only: ask WhatsApp now (lookups)
	Actor   string // for the audit entry of a refresh
}
```

### From WhatsApp: on request, cached

| Field | Call | Lookups | TTL (then `stale: true`) | Also updated by |
|---|---|---|---|---|
| `on_whatsapp`, canonical JID | `IsOnWhatsApp` | 1 | 7 days | a successful send (`Resolve` cache) |
| `about`, `verified_name`, `picture_id`, LID | `GetUserInfo([]JID{jid})` | 1 | 24 h | `events.UserAbout` (about), `events.Picture` (id; URL cleared), `events.BusinessName` |
| `picture_url` | `GetProfilePictureInfo(jid, &{Preview: true, ExistingID: cached})` | 1 | when the id changes; URL older than 6 h is dropped from answers | `events.Picture` |
| `business` | `GetBusinessProfile` | 1, only when a verified name exists | 7 days | |

- **Only `refresh=1` (admin) calls WhatsApp.** Opening a profile never does, so read tokens,
  restricted tokens, MCP and a polling panel can't spend the budget. The answer says `as_of` per
  part and `stale` when past its TTL.
- **Budget.** Each call is preceded by `b.lookup(ctx, 1)`; a refresh is 2 to 4 lookups. A 429 from
  the limiter comes back as `RateLimitError` with `Retry-After`. One refresh per person per 5 minutes:
  a second one inside that returns the cache with `refreshed: false` and `next_refresh_at`.
- **Batching.** `GetUserInfo` takes many JIDs; a refresh of one person uses one. A future
  "refresh this tag" (not in this plan) would batch 50 per call and charge 50.
- **Events are free.** `events.Picture`, `events.UserAbout`, `events.BusinessName` update an
  **existing** cache row only (`UPDATE … WHERE jid = ?`, after `PhoneJID`), synchronously (one small
  write, like messages), so the table can't grow from strangers' notifications. Group pictures are
  ignored.
- **Errors.** `ErrProfilePictureNotSet` → `picture_id = ''`, `ErrProfilePictureUnauthorized` → `'-'`
  (hidden from you); both are answers, not errors. Other errors are stored cleaned of URLs and
  capped, never logged with the number.

### From wame's own data

Computed per profile, never in the list. Keys = `optOutKeys(jid)` (phone JID + its LID) plus the
9th-digit variant.

```sql
-- messages: index wame_messages_ts (chat, ts); O(messages in the chat), ~10 ms for 50k
SELECT MIN(ts), MAX(ts), SUM(from_me = 1), SUM(from_me = 0)
FROM wame_messages WHERE chat IN (?, ?) AND type <> 'reaction' AND deleted = 0;

-- unread: same expression as store.Chats; index wame_messages_chat_seq
SELECT COUNT(*) FROM wame_messages WHERE chat = ? AND from_me = 0 AND type <> 'reaction' AND deleted = 0
	AND seq > COALESCE((SELECT read_seq FROM wame_chat_state WHERE chat = ?), 0);

-- groups in common: O(total members), a few hundred groups × members, < 5 ms
SELECT g.jid, g.name FROM wame_groups g, json_each(g.members) m WHERE m.value IN (?, ?) ORDER BY g.name;

-- queue (last 7 days only: finished items are pruned); wame_outbox is small, a scan is fine
SELECT COUNT(*), SUM(status = 'queued' OR status = 'sending'), SUM(status = 'sent'),
	SUM(delivered_at > 0), SUM(read_at > 0), SUM(status = 'failed'), MAX(updated_at)
FROM wame_outbox WHERE chat IN (?, ?) OR recipient IN (?, ?, ?);
```

- **Opt-out:** `Store.OptedOut(keys...)`.
- **Tokens that can see the chat:** `Store.Tokens()`, usable ones with an allowlist, `ChatAllowed`
  (cached; it may resolve the allowlist's own entries, which is already allowed). Names only.
- Messages sent in groups by this person are not counted (no index on `sender`; adding one costs
  every insert). Open question below.
- All of this is `Store.ContactStats(keys []string) (ContactStats, error)` in
  `internal/store/contacts.go`, one read transaction, `relinkLIDs()` first.

### Your own fields

`name`, `tags`, `notes`, `language`, `time_zone`, `created_at/by`, `updated_at/by` from
`wame_contacts`.

## HTTP API

New handlers in `internal/api/contacts.go` (new file); routes added to the table in `api.go`.

| Route | Scope | Restricted tokens | Lookups |
|---|---|---|---|
| `GET /contacts` | read | **only allowlisted chats** (was 403) | never |
| `GET /contacts/tags` | read | 403 (`unrestricted`): tag names and counts span every contact | never |
| `GET /contacts/{number}` | read: basic; admin: full | allowlisted chats only (fixed 403 text otherwise) | never |
| `GET /contacts/{number}?refresh=1` | admin | n/a | 2-4 |
| `PUT /contacts/{number}`, `PATCH /contacts/{number}` | admin | n/a | ≤ 1 (admin `PUT` while logged in, to canonicalize) |
| `DELETE /contacts/{number}` | admin | n/a | never |
| `POST /contacts/import` (`text/csv`) | admin | n/a | never |
| `GET /contacts/export` (`text/csv`) | admin | n/a | never |

**Why these scopes.** Names and tags are what bots need to route and greet; `read` tokens can
already see names through `/chats`. Notes, language/time zone, the WhatsApp profile, stats and
token lists are personal data or reveal how wame is used, so the full profile is `admin`. Writes are
`admin` because the list is shared: a name feeds every token's `{name}` templates and a tag decides
who another token's `tag:` batch reaches. (A separate `contacts` scope is an open question.)

**Restricted tokens.** `{number}` goes through `allowedChat` (`LocalChat` + `ChatAllowed`, no
lookup); the list is filtered with `ExpandAllowlist`. `tag=` on a restricted list filters inside
the allowlist only. Changing `GET /contacts` from 403 to filtered is a token-model change: update
`docs/security/SCOPED-TOKENS.md` and add a regression test in `security_test.go`.

**Resolving `{number}` for unrestricted callers:** digits, `+…`, or a URL-escaped JID. Parsed with
`LocalChat` + `phoneVariants`; never `Resolve` (a read token must not trigger lookups). A group JID
is 400 `contacts are people, not groups`.

### Shapes

`GET /contacts?q=&tag=clients&tag=vip&known=1&limit=` (tags AND-ed; `known=1` drops people you only
know by push name; `limit` default 1000, max 5000). Existing fields stay, new ones are added:

```json
[ { "jid": "5511999999999@s.whatsapp.net", "phone": "+5511999999999", "name": "Maria",
    "name_source": "own", "own_name": "Maria", "book_name": "Maria", "push_name": "Maria",
    "tags": ["clients"] } ]
```

The list merges whatsmeow's address book with `wame_contacts` (a person in either), works before
login (own rows only), and never includes notes.

`GET /contacts/tags` → `[ { "tag": "clients", "count": 12 } ]`.

`GET /contacts/5511988887777` (admin):

```json
{ "jid": "5511988887777@s.whatsapp.net", "phone": "+5511988887777",
  "names": { "name": "John", "name_source": "own", "own_name": "John", "book_name": "John", "push_name": "John" },
  "own": { "tags": ["clients"], "notes": "prefers mornings", "language": "en", "time_zone": "America/New_York",
           "updated_at": "2026-10-13T09:12:00-03:00", "updated_by": "admin-panel" },
  "whatsapp": { "on_whatsapp": true, "about": "Hey there", "verified_name": "",
                "picture": { "id": "1712345678", "url": "https://…", "hidden": false },
                "business": null, "as_of": "2026-10-12T21:00:00-03:00", "stale": false, "error": "" },
  "activity": { "first_at": "…", "last_at": "…", "sent": 42, "received": 57, "unread": 2,
                "groups_in_common": [ { "jid": "120363012345678901@g.us", "name": "Family" } ] },
  "queue": { "window": "7d", "total": 3, "open": 0, "sent": 3, "delivered": 3, "read": 2, "failed": 0, "last_at": "…" },
  "opted_out": false,
  "tokens": ["support-bot"] }
```

A `read` token gets `jid`, `phone`, `names`, `own.tags` and `whatsapp.on_whatsapp` only. A person
never seen still answers 200 with empty parts (so the panel can tell a missing route, 404, from an
unknown person). `refresh=1` adds `"refreshed": true|false` and `"next_refresh_at"`.

`PUT` replaces all own fields (absent = empty). `PATCH` merges: absent = unchanged, `null` or `""`
clears; `add_tags` / `remove_tags` arrays edit tags without a read-modify-write race. Both answer
200 with the profile as the caller may see it. A body whose fields are all empty after a `PUT` or
`PATCH` deletes the row. `DELETE` removes the own row (404 if none); the cache stays.

`POST /contacts/import?add_tags=clients&dry_run=1` with a CSV:

```csv
number,name,tags,notes
5511999999999,Maria,clients vip,
5511988887777,John,clients,"prefers mornings"
552187654321,,,
```

- Columns in any order and case: `number` (required; `to` and `phone` accepted as aliases), `name`,
  `tags` (separated by spaces, `;` or `|`), `notes`, `language`, `time_zone`. Unknown columns are
  refused by name. A column present overwrites that field (an empty cell clears it); absent columns
  are left alone. Same CSV rules as the queue (BOM, CRLF, `;` files, scientific notation refused):
  extract the header logic of `ReadQueueCSV` into `readCSVTable(r, columns, required, limit)` in
  `internal/bot/queuecsv.go` and use it for both.
- ≤ 5000 rows and 4 MB. One transaction: any bad line → 400 naming it, nothing stored. Idempotent
  by nature (an upsert), so no key needed.
- Answer `{"added": 2, "updated": 1, "unchanged": 0}`; `dry_run=1` answers the same without storing.
- `GET /contacts/export` writes the same columns back (own rows only), so a round trip is lossless.

**Errors:** the usual `writeError` mapping. 400 for bad numbers, groups, tags, sizes, time zones; 403
fixed text for restricted callers; 404 for `DELETE` of a person without a row; 413 over size; 429
on a refresh over the lookup budget (with `Retry-After`); 502 on a refresh while not logged in.

**Audit** (`Bot.Audit`, add to the table in `ADMIN-PANEL.md` and the `AuditEntry` enum):

| Action | Target | `detail` (never values) |
|---|---|---|
| `contact_set` | chat | changed field names: `name, tags` |
| `contact_delete` | chat | |
| `contact_import` | `csv` | `N rows: A added, U updated` |
| `contact_export` | `csv` | `N contacts` |
| `contact_refresh` | chat | `N lookups` or the error |

Docs: `docs/API.md` (route table, scopes table, the 403 note, a `## /contacts` section), and
`docs/openapi.yaml` (`Contact`, `ContactProfile`, `ContactTag`, the import, the audit enum).

## MCP

- **`find_contact`** uses `Bot.Contacts` with the unified name: `name` and `name_source`, plus
  `tags`. Names, push names and tags go under `untrusted` through `clean()` (an imported CSV is
  third-party text too). Restricted callers get their allowlisted contacts instead of an error, the
  same rule as HTTP, implemented once in `Bot.Contacts(ctx, ContactQuery{Q, Tags, Known, Allow})`.
- **`contact_profile`** (new, `internal/mcpserver/contacts.go`, `ReadOnlyHint`): read scope,
  `chat` through `LocalChat` (+ `allowedChat` for restricted), **never** `CanonicalChat`, never a
  refresh. Returns: names, tags, language, time zone, on WhatsApp, about and verified name, first and
  last message time, counts, unread, names of groups in common. Everything from other people
  (names, about, group names, verified name) under `untrusted`, cleaned.
- **Never reveals:** notes, the picture URL, the business profile, queue stats, opt-out status
  (kept quiet on purpose), which tokens can see the chat, error text from WhatsApp. Even for an
  admin connection: an agent doesn't need them and a prompt injection could exfiltrate them.
- Docs: `docs/MCP.md`, the server instructions in `server.go` (one line about `contact_profile`).

## Admin panel

**Recommendation: a Contacts tab** (key `9`, after Logs), with the profile as its right pane, plus
`p` on the Chats tab to jump to the selected chat's profile there. A card on Chats would have no
room for a tag filter, the list of people you never chatted with, or an import.

- Left: the list from `GET /contacts` (name, `+number`, tags dimmed); right: the profile of the
  selected person (`GET /contacts/{number}`, loaded on selection with a request sequence like
  `optOutsReq`).
- Keys: `j/k` etc. (`ListMove`), `/` filter (name, number, tag, shared `filterKey`), `t` tag filter
  (prompt with completion from `GET /contacts/tags`; empty clears), `e` edit name, `T` edit tags
  (`+vip -old` or a full list), `n` edit notes, `L` language / time zone, `W` refresh from
  WhatsApp (`r` stays the global reload; the footer says "2-4 lookups", and a refused refresh shows
  `next_refresh_at`), `d` delete own fields
  (confirm, capturing the chat like the opt-out remove), `I` import a local CSV file (path prompt,
  `dry_run` first, then confirm), `enter` on a person opens their chat on the Chats tab.
- Everything other people wrote (push names, book names from the phone, about, business fields,
  group names) and your own notes go through `safe()`; notes are shown wrapped, with newlines folded.
- New modes: `modeEditContact` (one prompt, field chosen by the key), `modeConfirmDeleteContact`,
  `modeImportContacts`. Edits use `PATCH` so two panels don't overwrite each other's other fields.
- **Older servers.** Probe with `GET /contacts/tags`: 404 → the tab shows "this server doesn't have
  contacts yet: update wame on it" (like `optOutsRefusal`), 403 → "this token can't list contacts"
  (no read scope, or limited to chats). A 404 on
  `GET /contacts/{number}` from a new server can't happen (it answers 200 for unknown people), so a
  404 there also means an older server.
- Polling: none. `NamesChanged` isn't streamed over SSE, so the tab loads when opened and reloads
  after each of its own writes and on `r` (reload).
- Docs: footer `keys` in `view.go`, `docs/ADMIN.md`.

## TUI

- Own names show up automatically (`ChatName`).
- `/name <text>` sets your name for the open chat (`/name` alone clears); `/tag clients vip` sets
  tags, `/tag +vip -old` edits them. Both call `Bot.PatchContact(ctx, "tui", chat, …)` (audited),
  refuse groups, and work offline (no lookup: the open chat is already a resolved key).
- `/contacts` lists tags next to names; `/to` completion includes own names.
- The context bar shows tags dimmed after the name.
- Docs: `tuiCommands`, `tuiHelp`, footer hints, `docs/TUI.md`; rerun `_demo/screenshot.sh` if the
  chat list changes visibly.

## Queue integration

### `to=tag:NAME`

- A `to` of `tag:clients` (JSON, CSV, MCP `schedule_message`) expands **at enqueue time** into one
  item per person with that tag, in `Bot.QueueMessages`, before the existing per-row checks. A
  snapshot: people tagged later aren't added.
- Allowlist-aware: for a restricted token, members outside its allowlist are dropped **silently and
  uncounted** (the count would reveal the tag's size). Expanding needs `read` as well as `send`
  (it reveals membership through the queued items).
- Members of one `tag:` row are distinct; a person in two tagged rows gets two items unless
  `dedupe` is on.
- Limits: the expanded total counts against `MaxQueueBatch` (1000 per request) and the per-actor
  open cap. An empty tag is 400 `tag clients has no contacts` (for restricted tokens: `…no contacts
  this token can reach`).
- Idempotency: an expanded item's key is `KEY#i:<chat>` instead of `KEY#i`, so a retry after a tag
  change replays the members already queued and adds only new ones.
- `CheckQueueRequest` / `ParseRecipient` must accept the `tag:` form only where expansion runs; the
  CLI's local check accepts it and `-dry-run` prints `tag:clients (expanded by the server)`.

### `{name}` templates

- Opt-in: `"template": true` / `?template=1` / `wame queue import -template`. Without it, braces are
  plain text (a message may legitimately contain `{name}`).
- Placeholders: `{name}`, `{first_name}` (first word), `{name|fallback}` (fallback when there's no
  name); `{{` and `}}` are literal braces. Any other `{…}` is a 400 naming the line, like an unknown
  CSV column.
- Name source: the unified name **without the push-name step** (a push name is whatever a stranger
  typed, and would go out under your account): own > address book > verified business name. An
  optional `name` column in the queue CSV wins over all of them. No name and no fallback → 400
  listing the first lines affected, so nothing goes out as "Hi ,".
- The name is cleaned before it's inserted: control, format (bidi, zero-width) and
  variation-selector runes removed, newlines folded, capped at 60 runes. `bot` doesn't import
  `termui` (it pulls in Bubble Tea and lipgloss), so this is a small `cleanName` in
  `internal/bot/names.go` with the same rule as `mcpserver.invisible`; the same helper cleans names
  on write in Phase 1.
- Rendered **at enqueue**, so `GET /queue/{id}` shows exactly what goes out and a later rename
  doesn't change queued text. Text length is checked after rendering.
- Replays: add `source_hash TEXT NOT NULL DEFAULT ''` to `wame_outbox` (via `outboxReceiptColumns`'
  `addColumns`), the sha256 of the unrendered text; `sameRequest` compares it instead of `text` when
  set, so resuming an import after someone edited a name replays instead of failing with 409.
- Lives in `internal/bot/template.go` (new): `renderTemplate(text string, names func() (string, bool)) (string, error)`.

### Per-recipient time zone for quiet hours (later)

The column is there from Phase 1; using it is a Phase 4: `outboxStep` reads the contact's zone from
the own-name map and, inside that recipient's window, moves the item's `due` to the window's end
(`Store.DeferOutbox(id, due)`, no attempt used) and goes on with the next item, so one sleeping
recipient doesn't hold the queue. Global quiet hours still apply on top.

### Dedupe options

Both are request options, applied inside `Bot.QueueMessages` **after** per-row keys are assigned
(keys keep the original row index, so skipping rows never shifts another row's key) and inside the
`Store.Enqueue` transaction (two imports at once can't both pass).

| | `dedupe` | `skip_sent` |
|---|---|---|
| Skips | a row whose (recipient, rendered text) already appeared earlier in the same import | a recipient that already has a queue item from this actor (admin: any actor) that is queued, sending or sent, either updated within `DURATION` or in batch `NAME` |
| API | `"dedupe": true` / `?dedupe=1` | `"skip_sent": "48h"` or `"batch:promo-october"` / `?skip_sent=` |
| CLI | `wame queue import -dedupe` (across the **whole file**, before chunking, so chunk keys stay deterministic) | `-skip-sent 48h` or `-skip-sent batch:promo-october` |
| Limits | | duration ≤ 7 days (`outboxKeep`: older items are pruned, so a longer window would lie) |

- **Recipient identity.** Add `recipient_key TEXT NOT NULL DEFAULT ''` to `wame_outbox`, set at
  enqueue to `LocalChat(to)` (no lookup), with an index:

```sql
CREATE INDEX IF NOT EXISTS wame_outbox_recipient ON wame_outbox (recipient_key, updated_at) WHERE recipient_key != '';

-- skip_sent=DURATION, per new row (inside the Enqueue transaction); ? keys = recipient_key + 9th-digit variant + its LID
SELECT 1 FROM wame_outbox
WHERE (recipient_key IN (?, ?, ?) OR chat IN (?, ?, ?))
	AND status IN ('queued', 'sending', 'sent') AND updated_at >= ?
	AND actor = ?                      -- omitted for admin
	AND NOT (actor = ? AND key LIKE ? ESCAPE '\')   -- this request's own key prefix: a replay is a replay, not a skip
LIMIT 1;

-- skip_sent=batch:NAME: same, with "batch = ?" instead of the time condition
```

  Old rows have `recipient_key = ''` and are matched by `chat` (set once resolved). Failed and
  canceled items don't count: the person didn't get the message.
- **Order per row:** replay by key first (the stored item is returned), then `skip_sent`, then
  `dedupe`, then insert. A replay of a request whose rows were skipped skips them again (unless the
  window has passed: then they're queued, which is what a fresh run would do).
- **Reporting.** The 202 answer gains `"skipped": {"duplicate": 4, "recently_sent": 8}` and
  `"skipped_rows": [{"line": 7, "to": "5511999999999", "reason": "recently_sent"}]` (first 100). The
  CLI prints `queued 2328, skipped 12 (8 sent in the last 48h, 4 duplicates)`. Audit `queue` detail
  becomes `N messages, M skipped`. Skips are not failures: no items, no events, no alert counts.
- **Privacy.** Without admin, only the caller's own items count, so a token can't learn whether
  another token messaged someone (the `OutboxFilter.For` rule, reused).
- An all-skipped request is 202 with `queued: 0`, not an error (a rerun of a finished campaign).

## Privacy and security

- **Scopes:** basic fields at `read`, everything else `admin` (above); restricted tokens see only
  allowlisted chats and never cause a lookup; `GET /contacts/tags` and every write are unrestricted
  only.
- **Never** in logs, events, the webhook, alerts, `/stats`, `/metrics` or audit details: names,
  notes, tags, about, business fields, picture ids or URLs. Log errors with `err` only, not the
  number. `NamesChanged` carries nothing.
- **Photos:** only the id and the signed URL (admin answers only, dropped after 6 h). No bytes are
  stored. A later opt-in (`-contacts-keep-photos`) could store previews 0600 next to the DB, under
  the IMAGE-PREVIEWS proposal.
- **Terminals:** `safe()` in the TUI and admin for every name, about, business field, tag and note.
  MCP: `clean()` under `untrusted`.
- **Size caps:** name 100 runes, notes 2000, 20 tags × 32 chars, language 16, time zone 64, about
  700, business JSON 8 KB, error 200; bodies 64 KB for `PUT`/`PATCH`, 4 MB / 5000 rows for import
  (413 above). At most 100,000 own rows (400 beyond), so the table can't be filled by a script.
- **Logout wipes contact data:** `Bot.Logout` (CLI and API) and `events.LoggedOut` both call
  `Store.ClearContactState`, which deletes your own contacts, the WhatsApp profile cache and
  `wame_group_members` in one transaction, and Bot drops its in-memory names, refresh slots and
  resolved numbers. Messages, group names and admin lists (`wame_groups`), opt-outs and the audit
  log stay. A disconnect or restart while still logged in keeps everything. A profile refresh or
  group refresh still in flight when the wipe runs writes nothing (a wipe counter checked under
  the contacts lock).
  Audited as `contacts_wipe` (`N contacts`, never names). Run `wame backup` first to keep your notes.
- **Export:** `wame backup` covers both tables; `GET /contacts/export` gives a CSV to admins.
- **Templates** never use push names; tag expansion doesn't reveal sizes to restricted tokens;
  `skip_sent` doesn't reveal other tokens' sends.

## Phases

Each phase lists disjoint file sets so agents can run in parallel (one row per agent). Fix the Go
signatures in the first commit of a phase (store types, `Bot` method stubs) so the others build.

### Phase 1: own fields, unified name, API, admin edit (≈ 1,500 lines with tests)

| Agent | Files | Work |
|---|---|---|
| store | `internal/store/contacts.go` (new), `contacts_test.go` (new), `store.go` (`Open`, `relinkLIDs` statements) | `migrateContacts`, `OwnContact`, `SetContact(c, fields)`, `DeleteContact(keys...)`, `Contacts(ContactFilter)`, `ImportContacts(rows) (ImportResult, error)` (one tx), `ContactTags()` |
| bot | `internal/bot/names.go` (new), `contactbook.go` (new), `contacts.go`, `events.go`, `queuecsv.go` (`readCSVTable` extraction), tests | `NamesOf`, `ChatName` delegating, own-name map, `phoneVariants`, `Contacts(ctx, ContactQuery)`, `SetContact`/`PatchContact`/`DeleteContact`/`ImportContacts` (validation, audit), `NamesChanged` on contact events |
| api + mcp | `internal/api/contacts.go` (new), `api.go` (routes only), `contacts_test.go` (new), `security_test.go` (restricted list), `internal/mcpserver/server.go` (`findContact`), `docs/API.md`, `docs/openapi.yaml`, `docs/MCP.md`, `docs/security/SCOPED-TOKENS.md` | routes and scopes above, import/export |
| admin + tui | `internal/admin/contacts.go` (new), `client.go`, `model.go`, `view.go`, `admin_test.go`, `internal/tui/commands.go`, `complete.go`, `view.go`, `tui_test.go`, `docs/ADMIN.md`, `docs/TUI.md` | Contacts tab (list, edit, tag filter, import), `/name`, `/tag` |

Also: `AGENTS.md` (layout rows, table list, audit actions), `ADMIN-PANEL.md` audit table, delete the
BACKLOG pointer. Tests: migration idempotent and in `rename_test.go`'s schema; relink merges a `@lid`
row's fields and tags; 9th-digit variant lookups; name order (each source, `~push`, groups, `Me`);
own-name map refreshed after writes; import atomic with line errors, alias columns, `;` files, dry
run; restricted list filtered, `/contacts/tags` 403, `{number}` outside allowlist 403 with the fixed
text and no lookup (count `Lookups` before/after); read token sees no notes; audit details carry no
values; admin tab sanitizes a name with ANSI and bidi characters; 404 probe message.

Risks: the `~push` display change surprises users; `Bot.own` drifting from the table (only `Bot`
writes, tests cover each path); the restricted `/contacts` change (security review).

**As built (store + bot core), where it differs from the above:**

- Only `wame_contacts` exists; `wame_contact_cache`, its relink statements and the cached
  `verified_name` step of `NamesOf` come with Phase 2 (the business step is `BusinessName` for now).
- Store: `OwnContact` is the row type. Writes take a `ContactEdit` (keys, values, named fields,
  `AddTags`/`RemoveTags`, actor) in one transaction that takes the write lock before reading, so tag
  edits can't overwrite each other: `SetContact(e) (ContactChange, error)` and
  `ImportContacts(edits, dryRun) (ImportResult, []ContactChange, error)`. Also `Contact(keys...)`,
  `DeleteAllContacts()`, `NormalizeTags`, and `EditError` (which edit of an import broke a limit:
  `ErrTooManyTags`, `ErrContactLimit`).
- Bot: one body type `ContactInput` for `PUT` and `PATCH` (its `UnmarshalJSON` tells null from
  absent and refuses unknown fields), `ContactView` (list entry + own fields) from `ContactOf` and
  the writes, `DeleteAllContacts` audited as `contact_delete_all` (`N contacts`). `contact_set`
  says `no change` when nothing changed; `contact_import` adds ` (dry run)`.
- The own-name map keeps only name and time zone (never notes). It remembers `Store.ContactsGen`
  when loaded and reloads when that changed behind it (a relink); Bot's own writes update it in
  place (`syncOwn`) when they were the store's only change. Names skip rows without a name, so a
  tags-only phone row doesn't hide the name under its `@lid`.
- `GET /contacts/export` goes through `Bot.ExportContacts`, audited as `contact_export`
  (`N contacts`).
- Keys come from `Bot.localJID` (shared with `LocalChat`): the phone JID without a device suffix.
- `known=1` keeps anyone with an own row, even without a name.
- `readCSVTable(r, csvTable{columns, aliases, required, limit, noun})`; a contacts CSV also splits
  tags on commas. The 4 MB cap is the caller's (`bot.MaxContactImportBytes`), like the queue.

### Phase 2: WhatsApp profile cache and the profile card (≈ 1,100 lines)

| Agent | Files | Work |
|---|---|---|
| store | `internal/store/contactcache.go` (new), `contacts.go` (`ContactStats`), `state.go` (`members` column, `SaveGroups`, `GroupsWith`), tests | cache upserts, event updates of existing rows, prune, clear |
| bot | `internal/bot/profile.go` (new), `events.go` (`Picture`, `UserAbout`, `BusinessName`, `LoggedOut` clear), `contacts.go` (`refreshGroups` saves members), `outbox.go` (`pruneOutbox` also prunes the cache), tests | `Profile`, refresh with lookups and the 5-minute guard, TTLs, error mapping |
| api + mcp | `internal/api/contacts.go` (profile, `refresh=1`), `internal/mcpserver/contacts.go` (new, `contact_profile`), `server.go` (register), tests, `docs/API.md`, `docs/openapi.yaml`, `docs/MCP.md` | |
| admin | `internal/admin/contacts.go` (profile pane, `W`), `chats.go` (`p`), `view.go`, tests, `docs/ADMIN.md` | |

Tests (no network: a fake for the four whatsmeow calls behind a small interface in `profile.go`):
refresh charges the right number of lookups and returns 429 past the budget; second refresh inside
5 min returns the cache; a read or restricted caller never reaches the fake; events update only
existing rows and ignore groups; logout clears the cache; stats SQL against a seeded DB (variants,
LID keys, 7-day queue window); MCP output has no notes, URL, tokens or opt-out and wraps names in
`untrusted`; the panel renders a hostile `about`.

Risks: whatsmeow call shapes change (keep them behind the interface); `GetUserInfo` also writes LID
mappings (good, but relink must be triggered: call `relinkLater`); `wame_groups.members` grows the
table (a few hundred KB).

**As built, where it differs from the above:**

- **The LID is part of the profile** (`lid`, empty when unknown). It comes from whatsmeow's local
  LID map first (never a lookup, so every profile read shows it), then from the `lid` column of the
  cache row (what `GetUserInfo` returned on the last refresh). A profile opened by an `@lid` key
  answers the phone JID too when the mapping is known. MCP's `contact_profile` returns it at the top
  level next to `chat` and `phone` (an identifier, cleaned like message ids), not under `untrusted`.
- **Group members live in a pivot table**, not a JSON column on `wame_groups`:
  `wame_group_members (group_jid, member, role, first_seen, last_seen)`, primary key
  `(group_jid, member)`, indexed on `member`. `role` is `member`, `admin` or `superadmin`.
  `refreshGroups` stores each participant's phone JID (through `PhoneJID`, or the participant's
  own phone number); an `@lid` stays only until its number is known. `SaveGroups` upserts the
  current members of each refreshed group (keeping `first_seen`, updating `role` and `last_seen`)
  and deletes the ones that left, in the same transaction; `SaveJoinedGroups` (what
  `refreshGroups` calls with the full list) also deletes the members of groups the account left. `relinkLIDs` moves `@lid` member rows onto the phone JID like other tables, merging
  with a phone row already there (older `first_seen`, newer `last_seen` and its role).
  `GroupsWith(keys...)` is an indexed join. The `admins` column stays as it was.
- The cache table is `wame_contact_cache` as planned, plus a `lid` column. `updated_at` is the last
  refresh only; notification updates (`CacheAbout`, `CachePicture`, `CacheVerifiedName`) don't
  touch it, so the 90-day prune counts from the last refresh. An older `events.UserAbout` never
  replaces a newer about.
- `ProfileOptions` has a third level, `Details` (about, verified name, language, time zone and
  activity, without notes, picture, business, queue, opt-out or tokens): what `contact_profile`
  uses. `Refresh` needs `Full`. Unlike the plan, `contact_profile` leaves out language and time
  zone: they're your own fields, which reach admin callers only and never MCP (AGENTS.md).
  `whatsapp.stale` is in every view.
- A refresh always asks `IsOnWhatsApp` (numbers only) and `GetUserInfo`, reserving both lookups
  together so the budget can't stop it between them; a number WhatsApp doesn't know stops after the
  first call and clears the cached profile. `as_of` (`info_at`) moves only when `GetUserInfo`
  answered. The row is stored under the JID WhatsApp resolved, and the person's rows under their
  other keys (the 9th-digit form) are deleted, so an older copy can't hide it. The picture is fetched only when
  `GetUserInfo` reports a picture id that changed or whose URL is older than 6 hours; an empty id
  is stored as "none" (WhatsApp may hide it there too, but telling the two apart would cost a
  lookup every time). The business profile is fetched on every refresh of a verified business.
  Its kept fields are address, email, categories (≤ 10), profile options (≤ 10) and the hours'
  time zone; the opening hours are left out.
- The 5-minute guard is in memory (it resets on restart, which can't be used to spend the budget
  faster than the lookup limiter allows) and shared by both 9th-digit forms of a number. A refresh
  refused by the budget before its first call frees the slot; one refused later (picture or
  business) keeps what it learned and answers 429. A WhatsApp error mid-refresh stores what was learned plus the cleaned error and
  answers 200 with `refreshed: false`; the audit entry carries the error.
- The cached verified name feeds the profile's `names` only (business step, when the address book
  has no business name). `NamesOf` doesn't read the cache, so `/chats` stays free of per-chat
  queries.
- `GET /contacts/{number}` answers the profile; `PUT` and `PATCH` still answer the phase 1
  `ContactView` (the panel reloads the profile after a write). The full profile and `refresh=1`
  need an admin token **without** a chat allowlist; a restricted admin token (which `auth.Issue`
  never mints, but the check doesn't rely on that) gets the basic view and a fixed 403 on refresh,
  checked before `Bot` is called. The basic view also carries `lid` and `whatsapp.stale`.
- MCP's `contact_profile` doesn't use `chat()` (which canonicalizes on WhatsApp for unrestricted
  callers): `personChat` checks the allowlist locally for restricted callers and passes everyone
  else's input to `Profile`, which parses it without a lookup. Tags go under `untrusted` like
  names; restricted callers see only the groups in common that their allowlist allows.
- "Tokens that can see the chat" compares each restricted token's allowlist with the person's keys
  locally; it never expands an allowlist, so opening a profile never asks WhatsApp.

### Phase 3: tags, templates and dedupe in the queue (≈ 1,000 lines)

| Agent | Files | Work |
|---|---|---|
| store | `internal/store/outbox.go` (`recipient_key`, `source_hash`, index, skip query inside `Enqueue` via an `EnqueueOptions`), `outbox_test.go` | |
| bot | `internal/bot/outbox.go` (`QueueMessages` options: expansion, template, dedupe, skip, report), `template.go` (new), `queuecsv.go` (`name` column, `tag:` in `to`), tests | |
| api + mcp | `internal/api/queue.go` (params, `skipped` answer), `internal/mcpserver/queue.go` (`tag:` and `template` on `schedule_message`), tests, `docs/API.md`, `docs/openapi.yaml`, `docs/QUEUE.md`, `docs/MCP.md` | |
| cli | `queue.go` (`-dedupe` whole-file, `-skip-sent`, `-template`, report), `main.go` (`usage`), `docs/CLI.md`, README table | |

Tests: keys keep original indexes when rows are skipped; replay after a skip; concurrent imports
with `skip_sent` (only one queues); non-admin skip ignores other actors; `batch:` form; 7-day cap;
expansion drops non-allowlisted members without counting them; expansion keys survive a tag change;
templates: unknown placeholder, `{{`, fallback, missing name lists lines, push name never used, a
name with newlines and bidi characters cleaned; replay after a rename is not a 409 (`source_hash`);
the response and audit never carry names.

Risks: idempotency regressions (the queue's most delicate part: review `sameRequest` and key
derivation as an attacker would); templates turning wame into a mass-personalization tool (keep the
opt-in and the existing limits; nothing here raises a rate).

### Phase 4 (later)

Per-recipient quiet hours; refresh a tag's profiles in batches of 50 (admin, budgeted); opt-in
photo previews.

## Decisions (2026-10-13)

1. **Writes are admin only.** No separate `contacts` scope; a sync integration uses an admin token.
2. ~~Logout keeps your own contacts~~ **Reversed (2026-10-15): logout wipes all contact data**:
   own contacts (tags, notes, language, time zone), the WhatsApp profile cache and group members.
   Messages and the audit log are kept. `wame backup` before logging out keeps a copy.
3. **`-skip-sent` counts every message you sent**, not only queued ones: it uses `wame_messages`
   (from_me), so the phone, `/send` and the TUI count too, and the window may be longer than the
   queue's 7 days (cap it, e.g. 90 days).
4. **Group messages aren't counted in the profile** for now: no index on `wame_messages (sender)`.
   Revisit if the profile needs it.
5. **`{name}` never uses push names; `{push_name}` is an explicit opt-in placeholder** with the same
   fallback rules.
