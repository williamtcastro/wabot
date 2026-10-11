# Routing rules and per-chat state for bots (proposal)

> **Deferred proposal.** Not built. Today routing is one `-webhook` URL with one `-webhook-filter`,
> plus the built-in `!ping`/`!time`/`!help` auto-replies. Bots keep their own per-chat state
> somewhere else. See the [list of proposals](README.md).

## Goal

1. **Rules:** declarative "when a message looks like this, do that": send it to a specific webhook,
   route it to a specific agent's stream, react, or drop it. Ordered, testable with a dry run,
   managed over the API or from a file.
2. **State:** a small per-chat key-value store that bots can share, with TTL and compare-and-set,
   so two agents working the same chat don't overwrite each other ("handoff=human",
   "last_order=8812", "language=pt").
3. SQLite by default (the current DB, no new service). Redis optional, for setups with several
   processes.

Non-goals: a scripting language, rules that send free text (that's what agents and
[drafts](DRAFTS-APPROVAL.md) are for), state that isn't tied to a chat, replacing `/events` or
`/messages/wait`.

## Assumptions about tokens

Scoped per-agent tokens exist (see [API.md → Tokens and scopes](../API.md#tokens-and-scopes)): each
has a **name** (the audit actor), **scopes** and an optional chat allowlist. Today's scopes are `read`,
`send` and `admin`. This proposal would add:

| Scope | Allows |
|---|---|
| `rules` | read and change rules, run dry runs |
| `state` | read and write per-chat state in chats on the token's allowlist |
| `read` | (existing) also needed to consume the `routed` stream below |

## Rules

### Shape

```yaml
# rules.yaml (optional; the same objects are accepted by POST /rules as JSON)
rules:
  - name: support-pt
    priority: 10
    match:
      chat: ["5511999999999", "120363012345678901@g.us"]
      from_me: false
      type: [text, image]
      regex: "(?i)\\b(pedido|order)\\s*#?\\d+"
    actions:
      - route: support-bot            # a token name: goes to that token's routed stream
      - react: "👀"
    stop: true

  - name: voice-to-transcriber
    priority: 20
    match:
      voice_note: true
      is_group: false
    actions:
      - webhook: transcriber          # a named webhook target, see below

  - name: mentions-in-family
    priority: 30
    match:
      chat: ["120363012345678901@g.us"]
      mentions_me: true
    actions:
      - route: assistant

  - name: mute-ana-in-groups
    priority: 5
    match:
      sender: ["552187654321"]
      is_group: true
    actions:
      - drop: {}
    stop: true

webhooks:
  transcriber:
    url: https://example.com/hooks/transcribe
    secret_env: WABOT_TRANSCRIBER_SECRET
```

### Match fields

All present fields must match (AND); lists are OR within the field.

| Field | Matches | Notes |
|---|---|---|
| `chat` | the chat JID | numbers resolved like `-webhook-filter` (`Bot.Resolve` after login, then `PhoneJID`). Exact chat, not sender: unlike `Filter.Chat`, a person's number here doesn't match their group messages. Use `sender` for that |
| `sender` | the sender JID | same resolution |
| `is_group`, `from_me` | as in [Filters](../API.md#filters) | |
| `type` | message type list | same values as `Filter.Types` |
| `q` | word search | same rule as `q` today (`textMatches`) |
| `regex` | Go RE2 regexp over `text` | compiled once on load; max 512 chars; RE2 has no catastrophic backtracking |
| `mentions_me` | message @-mentions the account | **not stored today**: parse `ContextInfo.MentionedJID` in `classify`/`toMessage` and add a `mentions` column (`TEXT`, comma-separated JIDs, `DEFAULT ''`). Also LID-normalized |
| `voice_note` | push-to-talk audio | **not stored today**: `type` is `audio` for both voice notes and audio files. Add a `ptt INTEGER DEFAULT 0` column from `AudioMessage.PTT` |
| `reply_to_me` | replies to one of my messages | look up `reply_to` with `Store.Get`; only evaluated if present |

Internally the first five reuse `store.Filter` (with a new `ChatOnly`-style flag) and
`Filter.Match`; the rest are a `Rule.Match` on top. New columns go in `messagesTable`
(`addMissingColumns` handles old DBs).

### Actions

| Action | Does | Limits |
|---|---|---|
| `webhook: <name>` | POSTs the Message to a named target, same body, headers and HMAC signing as `-webhook`, with `X-Wabot-Rule: <rule name>` | each target gets its own `bot.Webhook` (queue, workers, retries). URLs and secrets never logged |
| `route: <token name>` | marks the message as routed to that token; it shows up on that token's `routed` stream (below) | the token must exist; a route to a deleted token is a load error |
| `react: <emoji>` | `Bot.React(ctx, "rule:<name>", …)` | goes through `Bot.deliver`, so `Bot.Limiter` applies; plus a rule budget like `autoLimit` (default 6/min, one per 10 s per chat), **dropped** when over budget, never queued |
| `drop: {}` | stops default handling: no default `-webhook` delivery, no auto-reply, no later rules | the message is still stored and still visible on `/messages` and `/events`. Dropping storage isn't offered |
| `set_state: {key, value, ttl}` | (phase 2) writes per-chat state, e.g. `handoff: human` | counts against state limits |

Rules don't send text. Anything that writes to WhatsApp is a reaction (low-risk, rate-limited)
or goes through an agent.

### Order and evaluation

- Rules run by `priority` ascending, then `name`. Every matching rule's actions run, until a
  matching rule has `stop: true`.
- Only new messages stored by `Bot.handle` (incoming and from the phone) are evaluated, the same
  set the webhook sees today. Not history imports, not messages sent through wabot, not
  edits/deletes in v1.
- Evaluation is in-memory and synchronous right after `b.save`, before `Webhook.Deliver`; actions
  that do I/O run in goroutines. Never block the whatsmeow event handler.
- Rules are held in an `atomic.Pointer[ruleSet]`, swapped on change.

### Interplay with what exists

- **`-webhook` / `-webhook-filter`** stay as the default route. If no rule matched with `drop`,
  the default webhook gets the message exactly as today. So adding rules never changes existing
  deliveries unless a rule says `drop`.
- **Auto-replies** (`-autoreply`) run after rules, and are skipped when a rule dropped the message.
- **Limiter.** Reactions from rules take a `Bot.Limiter` slot like any send. A burst that matches
  a reacting rule could eat the send budget, which is why rule reactions have their own small
  drop-not-queue budget first. Webhook and route actions don't touch WhatsApp, so they're not limited.
- **Audit.** Rule changes are audited (`rule_create`, `rule_update`, `rule_delete`, actor = token
  name or `file`). Reactions are audited by `Bot.React` as today with actor `rule:<name>`.
  Matches themselves aren't audited (too many); they're counted.

### The `routed` stream

- `GET /events?events=routed` for a token returns only messages routed to **that** token, as
  `event: routed`, `data: {"rule":"support-pt","message":{…}}`, with `id: <seq>`.
- To make it resumable like `message` events, routing decisions are stored:
  `wabot_routes (seq INTEGER, token TEXT, rule TEXT, PRIMARY KEY (token, seq))`, pruned after
  7 days. Catch-up reads it joined with `wabot_messages`, so `Last-Event-ID` works.
- `GET /messages/wait?routed=true` does the same for long-pollers.

### API

| Method | Path | Scope | |
|---|---|---|---|
| GET | `/rules` | `rules` | all rules, with `source` (`api` or `file`) and match counters |
| POST | `/rules` | `rules` | create; 400 on bad regex, unknown token or webhook name |
| PUT | `/rules/{name}` | `rules` | replace; `If-Match: "<version>"` required |
| DELETE | `/rules/{name}` | `rules` | |
| POST | `/rules/test` | `rules` | dry run, below |

Rules from the YAML file are read-only over the API (409 on PUT/DELETE).

**Dry run.** `POST /rules/test` with either `{"seq": 1042}` (a stored message) or
`{"since": 1000, "limit": 200}` (replay stored messages), and optionally `{"rules": [...]}` to
test unsaved rules. Nothing is sent, posted or stored:

```json
{
  "results": [
    { "seq": 1042, "matched": ["support-pt"], "stopped_at": "support-pt",
      "actions": [ { "route": "support-bot" }, { "react": "👀" } ],
      "default_webhook": true }
  ]
}
```

Each rule can also be saved with `dry_run: true`: it's evaluated live and counted, but its actions
are only logged (debug level, rule name and `seq`, never text).

### Where rules live

- **DB:** `wabot_rules (name TEXT PRIMARY KEY, priority INTEGER, enabled INTEGER, dry_run INTEGER,
  spec TEXT /* JSON of match, actions, stop */, version INTEGER, updated_at INTEGER, updated_by TEXT)`
  and `wabot_rule_webhooks (name, url, secret_ref)`. Secrets are stored as env var names
  (`secret_env`), not values, so a DB copy doesn't leak them.
- **File:** `-rules PATH` / `WABOT_RULES`. Loaded at startup and on `SIGHUP`; a bad file keeps the
  previous rules and logs the error. File rules win over DB rules with the same name.
  YAML via `go.yaml.in/yaml/v3` (v3.0.5; the maintained continuation of `gopkg.in/yaml.v3`), or
  accept only JSON to avoid the dependency. See open questions.

## Per-chat state

### API

| Method | Path | Scope | |
|---|---|---|---|
| GET | `/chats/{jid}/state` | `state` | list keys in the namespace (values included, up to the limits) |
| GET | `/chats/{jid}/state/{key}` | `state` | one entry; `ETag: "<version>"` |
| PUT | `/chats/{jid}/state/{key}` | `state` | set; optional CAS and TTL |
| DELETE | `/chats/{jid}/state/{key}` | `state` | optional `If-Match` |

`?ns=` picks a namespace. Default is the caller's token name, so bots don't collide by accident;
`ns=shared` is the one place bots meet. A token can only use its own namespace and `shared` (plus
any listed in its config).

```sh
curl -X PUT -H "Authorization: Bearer $T" -H 'If-Match: "3"' \
  "localhost:8080/chats/5511999999999/state/handoff?ns=shared" \
  -d '{"value":{"to":"human","by":"Ana"},"ttl":"2h"}'
```

```json
{ "key": "handoff", "value": {"to":"human","by":"Ana"}, "version": 4,
  "expires_at": "2026-10-11T12:30:00-03:00", "updated_by": "support-bot" }
```

- **CAS.** `If-Match: "N"` sets only if the current version is N; `If-None-Match: *` only if the
  key doesn't exist. Mismatch → **412** with the current entry in the body, so the caller can
  merge and retry. (412 is new for this API: `writeError` needs an `errPrecondition` mapping.)
  Without either header, the write is unconditional.
- **TTL.** `ttl` as a Go duration, max 30 days. Omitted = no expiry. An expired key reads as 404.
- **Limits.** Key: 1–128 chars, `[A-Za-z0-9._:-]`. Value: any JSON, max 16 KB. Max 256 keys per
  chat per namespace; 400 when exceeded. These keep it a state store, not a database.
- Chat allowlists apply (404 outside the list). `{jid}` goes through `Resolve` + `PhoneJID`, and
  `RelinkLIDs` must move `@lid` rows, or state splits like chats used to.
- Writes aren't audited per call (too chatty); `DELETE` of a whole namespace (if added) would be.
  Values are never logged.

Note the name clash: `wabot_chat_state` already exists (read positions). The new table is
`wabot_kv`.

### Storage interface

```go
// package kv (internal/kv)
type Entry struct {
	Key       string
	Value     json.RawMessage
	Version   int64
	ExpiresAt time.Time // zero = never
	UpdatedBy string
}

// Store is per-chat key-value state. ifVersion: -1 = unconditional, 0 = must not exist, n = must be n.
type Store interface {
	Get(ctx context.Context, ns, chat, key string) (Entry, error)
	List(ctx context.Context, ns, chat string) ([]Entry, error)
	Set(ctx context.Context, ns, chat, key string, value json.RawMessage, ttl time.Duration, ifVersion int64, by string) (Entry, error)
	Delete(ctx context.Context, ns, chat, key string, ifVersion int64) error
	Close() error
}

var ErrVersion = errors.New("version mismatch") // → 412
```

`internal/kv` depends on nothing internal (like `store`); `bot` holds a `kv.Store`, `api` calls
`Bot` methods that enforce limits and namespaces. Errors: `store.ErrNotFound` for missing keys
so `writeError` already maps them to 404.

### SQLite backend (default)

```sql
CREATE TABLE IF NOT EXISTS wabot_kv (
	ns         TEXT NOT NULL,
	chat       TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	version    INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	updated_by TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (ns, chat, key)
);
CREATE INDEX IF NOT EXISTS wabot_kv_expires ON wabot_kv (expires_at) WHERE expires_at > 0;
```

- CAS is one statement: `UPDATE … SET value=?, version=version+1 WHERE ns=? AND chat=? AND key=?
  AND version=? AND (expires_at=0 OR expires_at>?)`, checked with `RowsAffected`; insert-if-absent
  is `INSERT … ON CONFLICT DO NOTHING` (treating an expired row as absent). SQLite serializes
  writers, so this is safe within one process and across processes sharing the file.
- Expired rows are hidden on read and deleted by a sweep every few minutes.
- No new dependency, same backups as everything else.

### Redis backend (optional)

`-state redis://user:pass@host:6379/0` (or `rediss://` for TLS), env `WABOT_STATE`. Default is
`sqlite`. The URL holds a password: never log it (same rule as the webhook URL). `-state-prefix`
(default `wabot:`) namespaces keys so several wabot accounts can share one Redis.

**Client.** Recommend [`github.com/redis/go-redis/v9`](https://github.com/redis/go-redis):
the official Redis-org client, the most widely used, pure Go (fine with `CGO_ENABLED=0`),
`redis.ParseURL` for the flag, and a plain API. Latest is **v9.23.0** (2026-10-05, checked with
`go list -m -versions`). Alternatives:

| Client | Latest | Notes |
|---|---|---|
| `github.com/redis/go-redis/v9` | v9.23.0 | recommended. Simple API, Lua scripts via `redis.NewScript`, pub/sub, streams |
| `github.com/redis/rueidis` | v1.0.78 (2026-09-15) | also official; faster (auto-pipelining, client-side caching), but a builder-style API that's more code for a handful of commands. Worth it only under heavy load, which a single WhatsApp account won't produce |
| `github.com/gomodule/redigo` | v1.9.3 (2025-10-08) | older, low-level, slower release pace |

For tests, `github.com/alicebob/miniredis/v2` (v2.40.0) runs an in-process fake Redis, so tests
stay offline as `AGENTS.md` requires.

**Mapping.**

- One hash per entry: `wabot:kv:{<chat>}:<ns>:<key>` with fields `v` (JSON), `ver`, `by`. The
  `{<chat>}` hash tag keeps a chat's keys on one Redis Cluster slot.
- TTL is native (`PEXPIRE`); no sweep needed.
- CAS is a short Lua script (`EVALSHA`): read `ver`, compare, `HSET`, `HINCRBY ver`, `PEXPIRE`,
  atomically. `WATCH`/`MULTI` works too but needs retries.
- `List` uses a per-chat set index `wabot:kvidx:{<chat>}:<ns>` maintained by the same scripts
  (cleaned lazily when a member has expired), not `SCAN`.
- The 16 KB / 256-key limits are checked in `bot`, so both backends enforce them identically.

**What Redis adds.**

- Shared state between processes: several wabot instances (one per WhatsApp account, since one
  process holds one session), and bots that want to read the same keys directly from Redis
  without going through wabot's API.
- Native TTL and expiry without sweeps.
- Fan-out across processes: with Redis configured, the `route` action can also `PUBLISH` to
  `wabot:route:<token>`, so a pool of worker processes can share one agent's traffic. Pub/sub is
  fire-and-forget; if workers need at-least-once, use a Redis Stream (`XADD` + consumer groups)
  instead. Within one process, `Bot.publish` and SSE already do the fan-out, so this only matters
  for multi-process setups.

**What it costs.**

- Another service to run, secure (AUTH, TLS, not exposed), back up, and monitor. State in Redis
  isn't in `wabot.db` backups; without AOF/RDB persistence a Redis restart loses it.
- New failure mode: Redis down → state endpoints return 502, while messaging keeps working. Rules
  must not depend on state being reachable in v1.
- One more dependency tree (go-redis pulls a couple of small modules).

So SQLite stays the default and the docs say when Redis is worth it: more than one wabot process,
or bots outside wabot that already use Redis.

## Where the code goes

- `internal/store`: `mentions` and `ptt` columns; `wabot_rules`, `wabot_rule_webhooks`,
  `wabot_routes` tables and queries (in a new `rules.go`); LID relink for `wabot_routes` and `wabot_kv`.
- `internal/kv`: `Store` interface, `sqlite.go`, `redis.go`, shared limit checks in `bot`.
- `internal/bot/rules.go`: `Rule`, `ruleSet`, load/validate (from DB and file), `evaluate(m)`,
  action dispatch, counters, the rule reaction budget. Hooked in `Bot.handle` next to
  `Webhook.Deliver`. `classify`/`toMessage` fill `Mentions` and `PTT`.
- `internal/bot/state.go`: `Bot.State*` methods (namespace checks, limits).
- `internal/api`: `/rules*`, `/chats/{jid}/state*`, `routed` on `/events` and `/messages/wait`,
  412 mapping in `respond.go`.
- `main.go`: `-rules`, `-state`, `-state-prefix`; SIGHUP reload. Docs: `CLI.md`, README table,
  `API.md`, `openapi.yaml`, a "routing" section in `BOT.md`.

## Effort and risks

| Piece | Effort |
|---|---|
| `mentions` / `ptt` columns and classification | 1 day |
| Rules engine, DB storage, API, dry run | 3–4 days |
| YAML file + SIGHUP | 1 day |
| `routed` stream with replay | 1–2 days |
| State API + SQLite backend | 2 days |
| Redis backend + miniredis tests | 2 days |

Risks: rules quietly eating messages (`drop` is explicit, dry-run first, counters on `GET /rules`);
regex cost on every message (RE2, length cap, compiled once); rule reactions adding ban risk
(own budget, drop not queue); state drifting into a general database (hard limits); two
state backends behaving differently (one shared conformance test suite run against both).

## Tests

- rules: match table for every field; order and `stop`; `drop` suppresses default webhook and
  auto-reply but not storage/SSE; dry run sends nothing; bad regex / unknown token rejected;
  file reload keeps old rules on error; reaction budget drops.
- routed stream: only the target token sees it; `Last-Event-ID` replay from `wabot_routes`.
- kv conformance suite (SQLite and miniredis): get/set/delete, CAS success and 412, create-only,
  TTL expiry, limits, namespaces isolated, concurrent CAS (N goroutines, exactly one wins per version).
- api: scopes and allowlists (404), `ETag`/`If-Match`, 412 body.

## Open questions

- YAML or JSON for the rules file? YAML is friendlier; JSON needs no dependency.
- Should rules see edits/deletes (`update` events)?
- Rule conditions on state (`when: {state: {ns: shared, key: handoff, equals: human}}`) to stop
  routing to a bot while a human handles the chat: phase 2, or v1 since it's the main use case?
- With Redis configured, should routing decisions (`wabot_routes`) also go to Redis Streams, or
  stay in SQLite?
- Do we need state that isn't per chat (per contact across chats, global)?
