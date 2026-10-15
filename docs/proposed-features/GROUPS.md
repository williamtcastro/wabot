# Groups: members, history and a profile per group (proposal)

> **Deferred proposal.** Not built. Today wame caches each group's name and admin list
> (`wame_groups`), and `GET /groups` asks WhatsApp for the whole list on every request. It builds on
> contacts phase 2 ([CONTACTS.md](CONTACTS.md)), which adds the `wame_group_members` pivot table.
> See the [list of proposals](README.md).

## Goals

- **Who is in each group, and since when**, on top of the `wame_group_members` pivot that contacts
  phase 2 adds. It answers "who is in this group" and "which groups is this person in".
- **A history per group.** It records who joined, left, was added, removed, promoted or demoted, by
  whom and when, plus name, description and settings changes. The history comes from WhatsApp's
  group notifications, and refresh diffs fill the gaps.
- **A profile per group**: name, description, creator, created time, settings, picture id,
  invite-link changes, and the member count over time. Free fields come from events and the
  existing refresh. Anything else comes from an admin `refresh=1`.
- **Serve `GET /groups` from the cache.** Today every request asks WhatsApp.
- **The pivot in use** in the API, MCP, the admin panel and the TUI, plus per-group stats from
  `wame_messages`.

## Non-goals

- **Never changing a group.** No add, remove, promote, kick, rename, settings change or link reset.
  whatsmeow has all of them (`UpdateGroupParticipants`, `SetGroupName`, `GetGroupInviteLink(…, true)`
  …). This is tracking, not moderation.
- **No tracking of groups you're not in.** That covers groups you left, groups you only see in an
  invite, and other groups in a community.
- **Never storing the invite code.** It's a credential to join the group. Only "the link was reset,
  at T, by X" is kept.
- **No photo bytes**, same as contacts.
- **No sending to group members** in this plan. See [Queue](#queue-later-and-risky).

## What exists today (verified)

| Piece | Where | Notes |
|---|---|---|
| Group cache table | `internal/store/state.go:25-30` | `wame_groups (jid PK, name, admins TEXT '[]', updated)`. No members, description or settings |
| Save and load | `internal/store/state.go:140-156` `SaveGroups`, `:158` `Groups` | upsert only: **a group you left is never deleted**, so its row and stale admin list stay forever |
| Refresh | `internal/bot/contacts.go:153-206` `refreshGroups` | one `GetJoinedGroups` call (`:175`). It has the full `[]*types.GroupInfo` but keeps only name and admins (`:184-196`), stored as phone JIDs via `PhoneJID` of `JID`, `PhoneNumber` and `LID`. It publishes `NamesChanged` (`:205`). It's **not charged** to `b.lookup` |
| When it runs | `internal/bot/events.go:18-21` on `*events.Connected`; `contacts.go:132-149` `group()` when the in-memory copy is over 10 min old (`groupsTTL`, `:151`) | `group()` serves `NamesOf` (`internal/bot/names.go:45`) and `isGroupAdmin` |
| In memory | `internal/bot/bot.go:66` `groups map[string]groupInfo`, loaded from the table at `bot.go:135` (`loadCachedGroups`, `contacts.go:213-228`) | `groupInfo{name, admins}` |
| Admin check for revokes | `internal/bot/events.go:76-86` → `isGroupAdmin` (`contacts.go:128`) | **up to 10 min stale**: someone demoted a minute ago can still have a delete applied |
| `GET /groups` | route `internal/api/api.go:108` `need(read, unrestricted(a.groups))`; handler `api.go:600-616` | calls `Client.GetJoinedGroups` **on every request**, so any read token can make WhatsApp calls without limit. It returns `jid, name, members` (a count). It fails before login. It doesn't save what it fetched |
| TUI `/groups` | `internal/tui/commands.go:143-154` (help `:46`, command list `:63`) | calls `GetJoinedGroups` directly from a `Cmd` and lists JID and name |
| `events.GroupInfo`, `events.JoinedGroup` | `internal/bot/events.go:16-66` | **not handled**: the switch has no case for them. Joins, leaves, promotions, renames and setting changes are dropped. A rename shows only after the next refresh |
| `events.Picture` | not handled | contacts phase 2 handles it for people and ignores groups |
| `events.LoggedOut` | `events.go:38-40` | sets the state and audits. No group data is cleared |
| LID relink | `internal/store/store.go:334-374` `relinkLIDs` | moves messages (chat and sender), chat state, opt-outs, contacts. Nothing for groups |
| SSE / webhook | `internal/api/api.go:434` (default `message, update, status`), `:526` opt-in `queue`; `internal/bot/webhook.go:222` `Deliver` (messages), `outbox.go:348` (`queue`) | no group events |
| Admin panel | `internal/admin/model.go:36` tabs `Status … Contacts` (keys 1–9) | no groups view |
| MCP | `internal/mcpserver/server.go:51-60` | no group tool; groups appear only as chats in `list_chats` |
| Message indexes | `internal/store/store.go:55-77` `UNIQUE (chat, id)`, `wame_messages_ts (chat, ts)`; `state.go:35` `wame_messages_chat_seq (chat, seq)` | no index on `sender` |

**From contacts phase 2 (being built in parallel; assumed, not redefined here):**
`wame_group_members (group_jid, member, role member|admin|superadmin, first_seen, last_seen,
PK (group_jid, member))`, indexed on `member`. `member` is a phone JID, or an `@lid` while unmapped.
`SaveGroups` syncs it from `GetJoinedGroups`, `GroupsWith(keys)` lists a person's groups, and person
profiles get a LID field. **Assumption:** the pivot holds **current** members only, so a sync
deletes people who are gone. If phase 2 keeps former members instead, every "current members"
query below adds `AND last_seen = <the group's last sync>`. Settle this before phase 1 starts
([open question 1](#open-questions)).

whatsmeow (`go.mau.fi/whatsmeow@v0.0.0-20261007111105-c386243a72ba`):

| API | File | What it gives |
|---|---|---|
| `events.GroupInfo{JID, Notify, Sender, SenderPN, Timestamp, Name, Topic, Locked, Announce, Ephemeral, MembershipApprovalMode, Delete, Link, Unlink, NewInviteLink, JoinReason, Join, Leave, Promote, Demote, Suspended, Unsuspended, …}` | `types/events/events.go:493`; parsed in `group.go:877` `parseGroupChange` | one notification. `Join` covers "joined" **and** "was added", and `Leave` covers "left" **and** "was removed": tell them apart by `Sender`. `JoinReason == "invite"` means joined by link. LID mappings are stored before dispatch (`notification.go:479-483`) |
| `events.JoinedGroup{Reason, Type, CreateKey, Sender, SenderPN, Notify, types.GroupInfo}` | `events.go:480`, `group.go:851` | **only** when a group is created with you in it (`<create>`). Being added to an existing group comes as `GroupInfo.Join` containing your own JID |
| `types.GroupInfo` | `types/group.go:21` | `OwnerJID/OwnerPN`, `GroupName{Name, NameSetAt, NameSetBy}`, `GroupTopic{Topic, TopicSetAt, TopicSetBy, TopicDeleted}`, `IsLocked`, `IsAnnounce`, `IsEphemeral/DisappearingTimer`, `IsIncognito`, `IsParent`, `LinkedParentJID`, `IsJoinApprovalRequired`, `AddressingMode` (`pn`/`lid`), `GroupCreated`, `Participants`, `ParticipantCount`, `MemberAddMode`, `Suspended` |
| `types.GroupParticipant{JID, PhoneNumber, LID, IsAdmin, IsSuperAdmin, DisplayName, …}` | `types/group.go:103` | `DisplayName` is set in LID groups where the number is hidden |
| `Client.GetJoinedGroups(ctx)` | `group.go:535` | **one** IQ for every group, with participants and description. Also stores LID mappings (`:568`) |
| `Client.GetGroupInfo(ctx, jid)` | `group.go:623` | one IQ for one group |
| `Client.GetProfilePictureInfo(ctx, groupJID, …)` | `user.go:548` | picture URL, one IQ |
| `events.Picture{JID, Author, Timestamp, Remove, PictureID}` | `events.go:533` | also for groups (`JID` is the group) |

## Data model

The new code goes in `internal/store/groups.go`. Columns are added to `wame_groups` through
`addColumns` in `migrateState`. The new tables come from `migrateGroups`, called from `Open` after
`migrateContacts`. All idempotent.

### `wame_groups`: the profile (new columns)

```sql
-- all NOT NULL with defaults, added through addColumns
topic           TEXT    DEFAULT '',   -- description, ≤ 2048 runes
topic_at        INTEGER DEFAULT 0,
topic_by        TEXT    DEFAULT '',   -- phone JID or @lid
owner           TEXT    DEFAULT '',   -- creator (OwnerPN, else OwnerJID through PhoneJID)
created_at      INTEGER DEFAULT 0,    -- GroupCreated
announce        INTEGER DEFAULT 0,    -- only admins send
locked          INTEGER DEFAULT 0,    -- only admins edit info
ephemeral       INTEGER DEFAULT 0,    -- disappearing timer in seconds, 0 off
approval        INTEGER DEFAULT 0,    -- join approval required
add_mode        TEXT    DEFAULT '',   -- admin_add | all_member_add
addressing      TEXT    DEFAULT '',   -- pn | lid (lid: WhatsApp hides members' numbers)
parent          TEXT    DEFAULT '',   -- community parent JID, '' none
is_community    INTEGER DEFAULT 0,
picture_id      TEXT    DEFAULT '',
picture_at      INTEGER DEFAULT 0,
invite_reset_at INTEGER DEFAULT 0,    -- last NewInviteLink event; the code itself is never stored
member_count    INTEGER DEFAULT 0,
self_role       TEXT    DEFAULT '',   -- member | admin | superadmin: what we are
left_at         INTEGER DEFAULT 0,    -- we left, were removed, or the group was deleted; 0 = still in
synced_at       INTEGER DEFAULT 0     -- last full member sync (refresh); `updated` stays "row touched"
```

- **Groups you're not in** keep their row (stored messages still need the name), get `left_at`, and
  lose their members (the pivot rows are deleted in the same transaction). History stays until
  retention. A refresh that no longer lists a group sets `left_at` on it, which fixes the "never
  deleted" problem above.
- `admins` (JSON) stays during the move, so old binaries reading the DB still work. The source of
  truth becomes the pivot's `role`. `loadCachedGroups` reads admins from the pivot once it exists.

### `wame_group_events`: history

```sql
CREATE TABLE IF NOT EXISTS wame_group_events (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	group_jid TEXT    NOT NULL,
	at        INTEGER NOT NULL,             -- ms: the notification's Timestamp, or the refresh time
	kind      TEXT    NOT NULL,             -- see below
	member    TEXT    NOT NULL DEFAULT '',  -- who it happened to (phone JID or @lid)
	actor     TEXT    NOT NULL DEFAULT '',  -- who did it, '' unknown
	source    TEXT    NOT NULL,             -- event | refresh
	detail    TEXT    NOT NULL DEFAULT ''   -- small JSON, ≤ 512 bytes
);
CREATE INDEX IF NOT EXISTS wame_group_events_group  ON wame_group_events (group_jid, at);
CREATE INDEX IF NOT EXISTS wame_group_events_member ON wame_group_events (member, at) WHERE member != '';
```

| `kind` | From | `member` / `actor` | `detail` |
|---|---|---|---|
| `join` | `Join`, `Sender` empty or = member | the person / '' | `{"via":"invite"}` when `JoinReason == "invite"` |
| `add` | `Join`, `Sender` ≠ member | the person / `Sender` | |
| `leave` | `Leave`, `Sender` empty or = member | the person / '' | |
| `remove` | `Leave`, `Sender` ≠ member | the person / `Sender` | |
| `promote`, `demote` | `Promote`, `Demote` | the person / `Sender` | `{"role":"admin"}` |
| `name` | `Name` | '' / `NameSetBy` | `{"name":"…"}`: the new name, ≤ 100 runes |
| `topic` | `Topic` | '' / `TopicSetBy` | `{"deleted":true}` or `{}`: **no text** (the current one is on the profile) |
| `settings` | `Announce`, `Locked`, `Ephemeral`, `MembershipApprovalMode` | '' / `Sender` | `{"announce":true}` etc. |
| `picture` | `events.Picture` on a group | '' / `Author` | `{"removed":true}` |
| `invite_reset` | `NewInviteLink` | '' / `Sender` | `{}`: never the link |
| `created` | `JoinedGroup` with `Type == "new"` | '' / `OwnerJID` | |
| `self_join`, `self_leave` | our own JID in `Join` / `Leave`, `Delete`, or a group missing from a refresh | us / `Sender` | `{"deleted":true}` for `Delete` |
| `bulk` | a refresh diff with more than 50 changes | '' / '' | `{"joined":120,"left":3,"role":2}`: one row instead of 125 |

- **Two sources.** An event changes the pivot at once (insert, delete or role change in one
  transaction with the history row), so the next refresh finds nothing to record. A refresh diff
  (what whatsmeow said vs the pivot) records only what the events missed, with `source = refresh`,
  `actor = ''` and `at` = refresh time ("seen by", not "happened at"). The API shows `exact: false`
  for those.
- **Diff in the store.** Phase 2's `SaveGroups` becomes `SyncGroups(groups) ([]MemberChange,
  error)`, one transaction. It computes added, gone and role-changed rows before it writes, and
  inserts the history rows. Keeping the same name is fine; the point is that the diff and the write
  happen in one transaction, so two refreshes can't record the same change twice. The first sync of
  a group (no `synced_at`) records nothing: everyone present is the baseline, `first_seen` = now.
- **Unknown times.** `first_seen` from a refresh is "when wame first saw them", not when they joined.
  Only `join`/`add` events give a real join time. The member list says `joined_at` only when such
  an event exists.

### `wame_group_counts`: member count over time

```sql
CREATE TABLE IF NOT EXISTS wame_group_counts (
	group_jid TEXT    NOT NULL,
	day       TEXT    NOT NULL,   -- YYYY-MM-DD, UTC
	members   INTEGER NOT NULL,   -- count at the end of the day (last write wins)
	PRIMARY KEY (group_jid, day)
) WITHOUT ROWID;
```

Upserted in the same transaction as any membership change or sync. One row per group per day with
activity: 300 groups × 365 days is about 110k tiny rows. Why not derive the count from history:
history is capped and has offline gaps, while this stays right because each write records the
pivot's real count.

### LID and phone keys

- Everything stored (`member`, `actor`, `owner`, `topic_by`) goes through `b.PhoneJID(jid.ToNonAD())`
  first, preferring `SenderPN` / `PhoneNumber` when whatsmeow gives them, like `refreshGroups`
  does today. Unmapped people stay `…@lid`.
- **Relink.** `relinkLIDs` gains, in its transaction (in a `relinkGroups(tx)` next to
  `relinkContacts`):

```sql
UPDATE wame_group_events SET member = <pn> WHERE member <mapped>;
UPDATE wame_group_events SET actor  = <pn> WHERE actor  <mapped>;
UPDATE wame_groups SET owner = <pn> WHERE owner <mapped>;
UPDATE wame_groups SET topic_by = <pn> WHERE topic_by <mapped>;
```

  (`<pn>`/`<mapped>` with `chat` replaced by the column, as in CONTACTS.md.) No unique keys, so
  plain `UPDATE`. **The pivot's relink belongs to phase 2.** If phase 2 doesn't do it, this plan
  adds it: merge `@lid` into the phone row with `first_seen = MIN`, `last_seen = MAX`, the higher
  role, and delete the `@lid` row.
- **Our own identity.** "Is it us" checks both `Client.Store.ID` and `Client.Store.LID` (non-AD),
  since LID groups name us by LID.

### Retention and caps

| What | Bound | Where |
|---|---|---|
| `wame_group_events` | 180 days, then at most 5,000 rows per group and 200,000 in total (oldest first) | `Store.PruneGroupEvents(before, perGroup, total)`, from the daily `pruneOutbox` step |
| `bulk` collapse | a refresh diff over 50 changes in one group is one row | `SyncGroups` |
| Event burst | at most 1,000 history rows per notification (a 1,024-member `Join` on create); the rest becomes one `bulk` row | event handler |
| `wame_group_counts` | 400 days | same prune step |
| Left groups | pivot rows deleted on leave; the history ages out normally | `MarkGroupLeft` |
| `wame_group_members` | not capped (WhatsApp caps groups at 1,024 members, communities' announcement groups higher). 300 groups × 1,024 ≈ 300k rows, ~30 MB worst case | stated, not enforced |
| Strings | name 100 runes, topic 2048, `detail` 512 bytes, cleaned of control runes on write (`StripInvisible`) | store |

## Events: what's free

Handled in a new `internal/bot/groups.go`, with cases added to `handle` in `events.go`. They are
**synchronous small writes**, like messages (no network in the handler). Anything that needs
WhatsApp goes through `b.Go`.

| Event | Does |
|---|---|
| `*events.GroupInfo` for a group we're in (row exists, `left_at = 0`) | one transaction: pivot changes, history rows, profile columns (name, topic, settings, `invite_reset_at`), count row. Updates `b.groups` (name **and admins**, which fixes the 10-min stale admin check for revokes). `namesChanged()` on a rename |
| `*events.GroupInfo` for an unknown or left group, with our JID in `Join` | we were added: insert a minimal row (`self_join`, actor = `Sender`) and ask for a refresh (below) |
| `*events.GroupInfo` for an unknown or left group otherwise | **ignored** (nothing stored), plus a debounced refresh, since our cache may be stale |
| `*events.GroupInfo` with our JID in `Leave`, or `Delete` | `self_leave`, `left_at`, pivot rows deleted, `self_role = ''` |
| `*events.JoinedGroup` | full `types.GroupInfo` included: same path as a one-group sync (baseline, no member rows in history) plus `created` when `Type == "new"` |
| `*events.Picture` with a group JID we're in | `picture_id`, `picture_at`, a `picture` row. The URL is never fetched here |
| `*events.LoggedOut` | see [Logout](#privacy-and-security) |

- **Refreshes.** `refreshGroups` stays the one full sync: it runs on `Connected`, from `group()`'s
  TTL, and on request. A new `b.requestGroupRefresh()` coalesces requests from events, at most one per
  minute, through `b.Go`. These automatic syncs are one `GetJoinedGroups` IQ each, which is what
  today's code already does, so they stay uncharged.
- `UnknownChanges`, `Link`/`Unlink`, `Suspended` are ignored for now (`Suspended` could go in
  `settings` later).

## Profile: free vs budgeted

| Field | Free from | Budgeted (admin `refresh=1`) |
|---|---|---|
| name, topic, owner, created, announce, locked, ephemeral, approval, add mode, addressing, parent, members and roles, count | `GetJoinedGroups` (the existing automatic sync) and `GroupInfo` events | `GetGroupInfo(jid)`: 1 lookup, for one group now |
| `picture_id` | `events.Picture` | `GetProfilePictureInfo(group, {Preview: true, ExistingID})`: 1 lookup. The URL is returned to the admin and **not stored** (it expires) |
| invite link | `invite_reset_at` from events | never fetched: `GetGroupInviteLink` only works for group admins and returns the credential |
| `self_role` | sync and Promote/Demote events | |

- **Only `refresh=1` (admin) calls WhatsApp on request**, the same rule as CONTACTS.md. Read
  tokens, restricted tokens, MCP and a polling panel only ever read the cache. Each call is preceded
  by `b.lookup(ctx, 1)`. Over budget it returns 429 with `Retry-After`. One forced refresh per group
  per 5 minutes and one forced full list per minute. Inside that window you get the cache with
  `refreshed: false` and `next_refresh_at`.
- Answers carry `as_of` (`synced_at`) and `stale: true` when `synced_at` is over 1 hour old while
  connected.
- Audit: `group_refresh` (target: the group JID, or `all`; detail `N lookups` or the error).

## HTTP API

New handlers in `internal/api/groups.go`. The routes go in `api.go`.

| Route | Scope | Restricted tokens | Lookups |
|---|---|---|---|
| `GET /groups` | read | **only allowlisted groups** (was 403) | never |
| `GET /groups?refresh=1` | admin | n/a | 1 (`GetJoinedGroups`) |
| `GET /groups/{jid}` | read | allowlisted only (fixed 403 text otherwise) | never; `refresh=1` admin: 1–2 |
| `GET /groups/{jid}/members` | read | allowlisted only; numbers hidden in `lid` groups ([below](#chat-allowlists-and-members)) | never |
| `GET /groups/{jid}/history` | **admin** | n/a | never |
| `GET /groups/{jid}/stats` | read | allowlisted only | never |
| `GET /contacts/{number}` | (existing) | gains `groups`: only allowlisted groups | never |
| `GET /contacts?group=JID` | read | the group must be allowlisted | never |

- `{jid}` accepts `120363012345678901@g.us` (URL-escaped) or the bare `120363012345678901`.
  Anything that isn't `@g.us` is 400 `groups only: use /contacts for people`. It is parsed locally
  and goes through `authorizeChat` for restricted callers. **A group we're not in, or never saw, is
  404** (unlike contacts, there's no useful empty answer). A left group is 200 with `left_at`, and
  members are empty.
- `GET /groups` before login serves the cache (today it fails). Existing fields stay: `jid`,
  `name`, `members` (now `member_count`, the same number). New ones are added:

```json
[ { "jid": "120363012345678901@g.us", "name": "Family", "members": 4, "announce": false,
    "locked": false, "ephemeral": 0, "self_role": "admin", "community": false,
    "last_activity": "2026-10-14T19:02:00-03:00", "as_of": "2026-10-15T08:00:00-03:00", "stale": false } ]
```

  `?left=1` adds left groups (admin only). `?q=` filters on the name. `last_activity` is from
  `wame_messages` (one grouped query on `(chat, ts)`).

`GET /groups/120363012345678901` (admin):

```json
{ "jid": "120363012345678901@g.us", "name": "Family", "topic": "Weekend plans",
  "topic_at": "…", "topic_by": { "jid": "5511999999999@s.whatsapp.net", "name": "Maria" },
  "owner": { "jid": "5511999999999@s.whatsapp.net", "name": "Maria" }, "created_at": "2024-03-02T10:00:00-03:00",
  "settings": { "announce": false, "locked": true, "ephemeral": 0, "approval": false, "add_mode": "admin_add" },
  "addressing": "pn", "self_role": "admin", "member_count": 4, "picture": { "id": "1712345678", "url": null },
  "invite_reset_at": null, "left_at": null, "as_of": "…", "stale": false,
  "counts": [ { "day": "2026-10-01", "members": 3 }, { "day": "2026-10-09", "members": 4 } ] }
```

`counts` covers `?days=` (default 90, max 400). Read tokens get the same shape without `topic_by`,
`owner` numbers (names stay) or `picture`.

`GET /groups/{jid}/members?role=admin&tag=clients&q=&limit=`:

```json
[ { "jid": "5511988887777@s.whatsapp.net", "phone": "+5511988887777", "lid": "…@lid",
    "name": "John", "name_source": "own", "role": "admin", "tags": ["clients"],
    "first_seen": "…", "joined_at": "2026-09-01T12:00:00-03:00", "added_by": "5511999999999@s.whatsapp.net" } ]
```

- Names come from `NamesOf` (never built by hand). `tag=` (AND-ed, same rules as `/contacts`)
  answers "members of group X with tag Y". `limit` defaults to 1024. Sorted by role, then name.
- `joined_at` / `added_by` only when a `join`/`add` history row exists, and only for admin. `lid` is
  admin only.

`GET /groups/{jid}/history?kind=join,leave&member=5521987654321&since=&before=&limit=` (admin),
newest first, cursor `before=<id>`, `limit` ≤ 500:

```json
{ "events": [ { "id": 812, "at": "…", "kind": "remove", "member": { "jid": "5521987654321@s.whatsapp.net", "name": "Ana" },
                "actor": { "jid": "5511999999999@s.whatsapp.net", "name": "Maria" }, "exact": true, "detail": {} } ],
  "next_before": 811 }
```

`GET /contacts/5511988887777` gains `"groups": [ { "jid": "…@g.us", "name": "Family", "role":
"admin", "first_seen": "…" } ]` from `GroupsWith(keys)`. This replaces CONTACTS.md's
`groups_in_common` (names only) with the role. `GET /contacts?group=…&tag=vip` uses the same store
query as `/members?tag=` (`Store.GroupMembers(GroupMemberFilter)`), so the two can't disagree.

Docs: `docs/API.md` (route and scope tables, the 403 change for `/groups`, a `## /groups` section),
`docs/openapi.yaml` (`Group`, `GroupProfile`, `GroupMember`, `GroupEvent`, `GroupStats`, the audit
enum), `docs/security/SCOPED-TOKENS.md` plus a regression test in `security_test.go`.

## Stats per group

`GET /groups/{jid}/stats?days=30` (`days` ≤ 90): `Store.GroupStats(jid, since)`, one read
transaction, `relinkLIDs()` first.

```sql
-- messages and last activity: wame_messages_ts (chat, ts), a range scan of the window
SELECT COUNT(*), SUM(from_me = 1), MAX(ts) FROM wame_messages
WHERE chat = ? AND ts >= ? AND type <> 'reaction' AND deleted = 0;

-- active members: same index range, then a temp b-tree for GROUP BY sender
SELECT sender, COUNT(*), MAX(ts) FROM wame_messages
WHERE chat = ? AND ts >= ? AND from_me = 0 AND type <> 'reaction' AND deleted = 0
GROUP BY sender ORDER BY COUNT(*) DESC LIMIT 50;

-- silent members: pivot minus the senders above, in Go (≤ 1,024 vs ≤ 1,024)
```

Answer: `messages`, `sent_by_me`, `last_activity`, `active_members` (count), `top` (50: member,
name, messages, last), `silent` (count; list for admin only), and `joins`/`leaves` in the window
(from history, admin only).

**Cost.** O(messages in the group's window). A busy group with 30k messages in 30 days is about
20–40 ms, without new indexes. The `GET /groups` `last_activity` column is one
`SELECT chat, MAX(ts) … WHERE is_group = 1 GROUP BY chat`, which walks `(chat, ts)` per chat.

**What would need a new index:** "Maria's messages across all groups" or "when did John last speak
anywhere" need `CREATE INDEX wame_messages_group_sender ON wame_messages (sender, ts) WHERE
is_group = 1`. Cost: one more b-tree write per stored group message (insert cost up maybe 15–25%
for group traffic), about 40–60 bytes per group message on disk (1M group messages ≈ 50 MB), and
a one-time build on upgrade (seconds to a minute on a big DB, at startup). CONTACTS.md deferred the
same index (decision 4). **Not in this plan**; [open question 5](#open-questions).

## MCP

New file `internal/mcpserver/groups.go`, registered in `server.go`, all `ReadOnlyHint`, read scope,
group arguments through `chat()` (allowlist, no lookup), never a refresh:

- **`list_groups`**: the cached list (name, member count, last activity, self role). Restricted
  callers get only their allowlisted groups.
- **`group_info`**: profile (name, topic, settings, count) and members (name, role; numbers by the
  [allowlist rule](#chat-allowlists-and-members)), capped at 200 members with a `truncated` flag, plus
  stats counts and top senders.
- `contact_profile` (contacts phase 2) lists group names in common, already planned.
- **Untrusted:** group names, topics and members' names are third-party text, so they go under
  `untrusted` through `clean()`. Your own contact names do too, as in `find_contact`.
- **Never:** history (who removed whom is surveillance-shaped and not needed by an agent), notes,
  LIDs, picture URLs, invite data, `added_by`, error text from WhatsApp. This holds even for an admin
  connection.
- Docs: `docs/MCP.md`, one line in the server instructions.

## Admin panel

**A Groups tab** (key `0`, after Contacts), one file `internal/admin/groups.go`, in the same layout
as Contacts:

- Left: `GET /groups` (name, member count, last activity, `A` if we're admin, `left` dimmed with
  `L` toggling left groups). Right: the selected group's card (`GET /groups/{jid}`, request sequence
  like `optOutsReq`), with a sub-view chosen by key: `m` members (with role and tags, `/` filter, `t`
  tag filter shared with Contacts), `h` history (paged with `before`), `s` stats.
- `W` forces a refresh of the group (footer: "1–2 lookups"; a refusal shows `next_refresh_at`), `r`
  stays the global reload, `enter` on a member opens their Contacts card, and `c` opens the group's
  chat on Chats.
- The Contacts card shows the person's groups with roles. `enter` on one jumps to Groups.
- `safe()` on group names, topics, member names and push names. Topics are wrapped with newlines
  folded.
- Live: if the server streams `group` events (phase 3), the tab reloads the open group, debounced to
  one per second like the Queue tab. Otherwise it loads on open and on `r`.
- Older servers: `GET /groups/{jid}` 404 for a group that's in the list means an older server ("this
  server doesn't have group details yet: update wame on it"). A 403 on `/history` hides that view
  (non-admin token).
- Docs: footer `keys` in `view.go`, `docs/ADMIN.md`.

## TUI

- `/groups` reads `Bot.Groups` (the cache) instead of calling `GetJoinedGroups` from the `Cmd`: no
  network, works offline, shows member count and last activity. `/groups refresh` forces a sync
  through the same budgeted path (actor `tui`).
- `/members` on an open group lists members with role and tags (names through `safe()`). `/group`
  shows the profile card plus the last 10 history rows. The TUI is the account owner's local view,
  so it sees what an admin does.
- The context bar for a group shows `N members` and `admin` when we are.
- Docs: `tuiCommands`, `tuiHelp`, footer hints, `docs/TUI.md`.

## Queue (later, and risky)

`to=group-members:120363012345678901@g.us` (expand to one item per member, like `tag:`) is
**not in this plan**. Messaging everyone in a group one by one, most of whom never wrote to you, is
the pattern WhatsApp's spam detection looks for. It can also reach people who joined through a
public link. If it's ever built: admin only, intersected with people you've exchanged messages with,
capped (for example 50 per batch), opt-outs applied, and `-skip-sent` on by default.

## Privacy and security

### Scopes

Group lists, profiles, members and stats are `read`, because a member list is what any member sees
in WhatsApp. History, `lid`, `added_by`/`joined_at`, the left-groups list, silent-member lists and
forced refreshes are `admin`. No writes exist. A separate `groups` scope isn't needed
([open question 6](#open-questions)).

### Chat allowlists and members

A token allowed group X sees X's profile, members and stats, and nothing about other groups.

- **Phone numbers in `lid` groups.** When a group is LID-addressed (`addressing = 'lid'`),
  WhatsApp hides members' numbers from other members, but wame may know a number from elsewhere
  (the address book, a 1:1 chat, the LID map). Non-admin callers then get `name` (which falls back
  to `DisplayName`, never a number) and no `phone`. Only admin sees the mapped number. In `pn`
  groups, numbers are what every member sees, so `read` gets them.
- `GET /contacts/{number}`'s `groups` list and `GET /contacts?group=` are filtered to the token's
  allowlisted groups, so a token can't learn which other groups someone is in.
- **Restricted callers never cause a lookup**, never see history, and get the fixed 403 text for a
  group outside the list (no oracle: the same text whether the group exists or not).
- `GET /groups` changing from 403 to filtered is a token-model change: update
  `docs/security/SCOPED-TOKENS.md`, and add regression tests (a list without other groups; a member's
  `groups` list without other groups; no `phone` in a `lid` group; `Lookups` unchanged).

### Members' numbers and LIDs are personal data

- They never go in logs, alerts, `/stats`, `/metrics` or audit details. Errors are logged with `err`
  only. The handler doesn't log participant lists.
- `wame backup` covers the new tables, as it does everything else.
- History can tell you when someone left a group you're in. That's visible in WhatsApp too (the
  system message), but wame keeps it searchable for 180 days. Retention is a setting
  (`group_history_days`, 0 = don't record history at all).

### Unbounded growth

See [Retention and caps](#retention-and-caps). A hostile group can't flood the table: rows are
capped per group, bursts collapse to `bulk`, and only groups we're in are recorded.

### Webhook and SSE `group` events

- **SSE:** opt-in `?events=group` (like `queue`). It's delivered to admin, and to tokens whose
  allowlist contains the group.
- **Webhook:** opt-in through a runtime setting `webhook_group_events` (default off), because
  existing receivers would otherwise start getting a new event type. It's a `webhookJob{event:
  "group"}` like the queue's (`outbox.go:348`) and isn't filtered by the message filter. When the
  filter has a `chat=`, only that group's events go out.
- **Shape**: no names or topic text, since receivers can `GET` them. Numbers are included only
  where `members` would show them:

```json
{ "group": "120363012345678901@g.us", "kind": "add", "at": "2026-10-15T09:00:00-03:00",
  "members": ["5521987654321@s.whatsapp.net"], "actor": "5511999999999@s.whatsapp.net",
  "changed": [], "source": "event" }
```

  `changed` lists field names for `settings`/`name`/`topic` (`["name"]`). Refresh diffs aren't
  sent (they aren't real-time, and a first sync would flood). A `bulk` row is one event with counts.
- `NamesChanged` still carries nothing.

### Logout

On `events.LoggedOut`: delete every `wame_group_members` row, clear `picture_id`, `self_role`,
`invite_reset_at`, and set `synced_at = 0`. That's current state the account can no longer see,
and it would go stale silently. **Keep** `wame_groups` names (stored messages need them, like
`wame_messages`, which survives logout), `wame_group_counts`, and the history until retention. An
admin `DELETE /groups/history` (audited `group_history_delete`) wipes the history. Pairing a
**different** account later starts from a fresh baseline. The old groups then show `left_at` after
the first sync, since they're not in the new list.

## Phases

Each phase lists disjoint file sets so agents can run in parallel (one row per agent). Fix the Go
signatures in the phase's first commit (store types, `Bot` stubs) so the others build. **Phase 1
starts after contacts phase 2 is merged** (it changes `state.go`, `SaveGroups` and
`refreshGroups`).

### Phase 1: profile, events, history, cached `/groups` (≈ 1,400 lines with tests)

| Agent | Files | Work |
|---|---|---|
| store | `internal/store/groups.go` (new), `groups_test.go` (new), `state.go` (columns, `SyncGroups` diff), `store.go` (`Open`, `relinkLIDs` → `relinkGroups`) | `migrateGroups`, `GroupProfile`, `SyncGroups`, `ApplyGroupChange`, `MarkGroupLeft`, `GroupEvents(filter)`, `GroupMembers(filter)`, `GroupCounts`, `PruneGroupEvents`, `ClearGroupState` (logout) |
| bot | `internal/bot/groups.go` (new), `groups_test.go` (new), `events.go` (cases), `contacts.go` (`refreshGroups` saves the profile, `group()` admins from events, `requestGroupRefresh`), `outbox.go` (prune call) | event mapping (join vs add, leave vs remove, self), caps and `bulk`, `Groups`, `Group`, `RefreshGroup` (budgeted, 5-min guard, audited), the whatsmeow calls behind the contacts phase 2 interface (extended, not a second one) |
| api | `internal/api/groups.go` (new), `api.go` (routes only), `groups_test.go` (new), `security_test.go`, `docs/API.md`, `docs/openapi.yaml`, `docs/security/SCOPED-TOKENS.md` | routes, scopes, `lid` number hiding, `/contacts/{number}` `groups`, `/contacts?group=` |

Also: `AGENTS.md` (layout rows, tables list, audit actions `group_refresh`, `group_history_delete`, the
events list), the `ADMIN-PANEL.md` audit table.

Tests (no network: a fake for `GetJoinedGroups`, `GetGroupInfo` and `GetProfilePictureInfo` behind
the interface; events built by hand as `*events.GroupInfo` / `*events.JoinedGroup`):

- `Join` with `Sender` = member → `join`, ≠ → `add`. Same for `Leave`. `JoinReason "invite"`.
- An event then a refresh records the change once. A refresh after a missed event records it with
  `source = refresh`. A first sync records nothing. Over 50 changes → one `bulk`.
- Our JID (phone and LID forms) in `Leave` → `left_at` and the pivot cleared. In `Join` of an unknown
  group → row created and a refresh requested. Other unknown groups store nothing.
- Promote/Demote update `isGroupAdmin` at once (a revoke by a just-demoted admin is ignored).
- `@lid` member and actor relinked. Counts row per day.
- Prune by age, per group and total caps. Logout clears members and keeps names and history.
- `GET /groups` makes **no** fake call (count calls) for read, restricted and admin without
  `refresh=1`. `refresh=1` charges 1 lookup, 429 over budget, second inside a minute returns the
  cache.
- Restricted: list filtered, other group 403 fixed text, `groups` on a contact filtered, no `phone`
  in a `lid` group, history 403.
- Names and topics with ANSI and bidi characters stored cleaned. Audit details carry no numbers.

Risks: getting "us" wrong (phone vs LID) and recording our own leave as someone else's; the
`SaveGroups` → `SyncGroups` change landing on phase 2's code (coordinate signatures first); the
restricted `/groups` change (security review).

### Phase 2: surfaces (≈ 1,000 lines)

| Agent | Files | Work |
|---|---|---|
| mcp | `internal/mcpserver/groups.go` (new), `server.go` (register, instructions), tests, `docs/MCP.md` | `list_groups`, `group_info` |
| admin | `internal/admin/groups.go` (new), `client.go`, `model.go` (tab), `contacts.go` (groups on the card), `view.go`, `admin_test.go`, `docs/ADMIN.md` | Groups tab, sub-views, `W`, jumps |
| tui | `internal/tui/commands.go`, `complete.go`, `view.go` (context bar), `tui_test.go`, `docs/TUI.md` | cached `/groups`, `/members`, `/group` |

Tests: MCP output wraps names and topics in `untrusted` and has no history, LIDs or URLs; the
restricted MCP caller sees only allowlisted groups; the admin tab renders a hostile group name and
topic; the 404 probe; the TUI `/groups` works with a logged-out bot (no client calls).

### Phase 3: stats and live events (≈ 700 lines)

| Agent | Files | Work |
|---|---|---|
| store | `internal/store/groups.go` (`GroupStats`), `groups_test.go` | the queries above, against a seeded DB |
| bot | `internal/bot/groups.go` (`GroupEvent` publish), `webhook.go` (group job), `settings.go` (`webhook_group_events`, `group_history_days`), tests | event shape, opt-in |
| api | `internal/api/groups.go` (`/stats`), `api.go` (SSE `group` case), tests, `docs/API.md`, `docs/openapi.yaml`, `docs/CLI.md` (settings) | |

Tests: stats exclude reactions and deleted messages; a window boundary; silent members; SSE `group`
only for admin or allowlisted tokens; the webhook off by default, never carrying names, and
respecting the `chat=` filter; `group_history_days = 0` records nothing.

Risks: the stats query on a very large group (measure with a seeded 100k-message group, as
`BenchmarkEnqueue` does); webhook receivers that assume only `message`/`update`/`queue` (that's why
it's opt-in).

### Phase 4 (later)

The `sender` index and cross-group activity per person; `to=group-members:` in the queue (see the
warnings); a member-count chart in the panel; communities (parent/sub-groups via `GetSubGroups`).

## Open questions

1. **Pivot semantics.** Does phase 2's `wame_group_members` delete departed members (assumed here)
   or keep them with an old `last_seen`? And does phase 2 relink the pivot's `@lid` rows?
2. **History retention:** 180 days and 5,000 rows per group by default, with `group_history_days`
   to change it or turn it off. Is that right, or should history be opt-in?
3. **Logout:** keep names, counts and history (proposed), or wipe all group data?
4. **`lid` groups:** hide mapped numbers from non-admin callers (proposed), or show what wame knows?
5. **The `sender` index:** worth ~50 MB per million group messages and slower group inserts, for
   "where does this person talk"? Deferred so far.
6. **Read scope for members:** is `read` enough to list a group's members (proposed), or should
   that be `admin` like contact notes?
7. **Webhook `group` events:** wanted at all, or is SSE enough?
8. **`GET /groups` restricted change:** filter to the allowlist (proposed), or keep 403?
