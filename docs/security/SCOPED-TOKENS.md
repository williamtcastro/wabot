# Scoped API tokens: security review

Review date: 2026-10-11. **Status: all findings (F1–F16) fixed**; see [Changelog](#changelog) and
the Status line under each finding. Scope: `internal/auth/auth.go`, `internal/store/tokens.go`,
`internal/api/authz.go`, `internal/api/api.go`, `internal/api/middleware.go`, the `token` subcommand
and the serve guard in `main.go`, and the tests that came with them. Line numbers refer to the tree
as reviewed and may drift. The "Design as reviewed" section describes the code *before* the fixes;
where it differs today, a **Now:** note says so.

## Changelog

- 2026-10-11: review written; F1–F12 opened.
- 2026-10-11: F1–F12 fixed, each with a regression test (listed per finding). The MCP endpoint
  (`/mcp`) and `wame mcp` were added in the same change and use the same principals, scopes and
  allowlists; see [MCP.md](../MCP.md).
- 2026-10-11: follow-up pass over `/mcp` and `wame mcp` found F13–F16 (the MCP surface didn't carry
  over every F-fix); all fixed with regression tests. See [Follow-up: MCP](#follow-up-mcp-f13f16).
- 2026-10-12: new admin-only routes (`/stats`, `/settings`, `/webhook/test`, `/webhook/replay`) and
  the remote panel `wame admin`; reviewed under [Follow-up: admin routes](#follow-up-admin-routes).
  No new way to authenticate.

## Summary

The core of the scheme is sound. Tokens are opaque, carry 256 random bits, and the database stores
only their SHA-256 hashes. Comparisons run in constant time, and every route in `NewHandler` has a
scope. The chat allowlist is enforced consistently on the data paths: `/messages`, `/messages/wait`,
`/events` (messages and updates), `/media`, `/chats`, `/send`, `/send/file`, `/react`, `/edit`,
`/delete`, `/read` and `/typing`. `reply_to` and target message ids are looked up inside the
authorized chat only, so a restricted token can't quote, react to, edit or delete a message from
another chat.

The weak points were in the transitions around that core (all fixed since; see each finding):

- **Token-less mode can come back at runtime.** It returns when the last token is revoked or expires,
  and also when the token-count query fails. On a LAN listener, `localOnly` trusts the
  attacker-controlled `Host` header, so this hands an unauthenticated network client the owner
  principal (F1, F2).
- **Revocation and expiry don't reach streams that are already open (F3).**
- **Restricted tokens can trigger WhatsApp number lookups** before the allowlist check. That burns
  the anti-ban lookup budget and leaks resolution results (F4).
- **Smaller gaps in scope and lifetime:** the QR code leaks to `read` tokens (F6), an expiring admin
  token can mint tokens that never expire (F5), and tokens are accepted in URLs, where the query
  value overrides the header (F7).

Every finding below was reproduced with a throwaway test in a scratch copy of the repo, using fake
data and no WhatsApp connection.

## Threat model

| Actor | Has | Wants |
|---|---|---|
| Unauthenticated LAN client | network reach to `serve -addr 0.0.0.0:…` | any access; mint a token |
| Agent with a limited token (`read`/`send`, chat allowlist, rate, expiry) | its own token; may be prompt-injected | read other chats, send elsewhere, mint/revoke tokens, read audit, log out, exhaust limits |
| Holder of a leaked token | a token from logs, shell history, a URL, or an agent's context | keep using it after revocation, or widen it |
| Malicious web page in the owner's browser | CSRF / DNS rebinding against `127.0.0.1` | act through token-less mode |

Out of scope: an attacker with local file access to `~/.config/wame/wame.db`, who already has the
WhatsApp session keys.

## Design as reviewed

### Token format

`wame_<id>_<secret>`

- `id`: 5 random bytes, lowercase base32 without padding (8 characters, alphabet `a-z2-7`, so never `_`).
- `secret`: 32 random bytes from `crypto/rand`, base64url without padding (43 characters, may contain `_` and `-`).
- `tokenID` splits at the first `_` after the prefix. That is unambiguous because the id can't contain `_`.

### Storage

Table `wame_tokens` holds `id` (primary key), `name` (UNIQUE, never deleted, so revoked names stay
reserved), `hash = SHA-256(full plaintext)`, scopes, chats, rate, created, expires, last_used and
revoked. The plaintext is returned once, by `POST /tokens` or `wame token create`. `GET /tokens`
leaves out the hash (`json:"-"`). Audit `detail` holds only the scope list, and the request log
leaves out the query string.

**Why unsalted SHA-256 is fine here.** A salt and a slow hash (bcrypt, Argon2) protect low-entropy
secrets chosen by humans against offline guessing. A 256-bit uniform random secret can't be guessed
offline at any hash speed, and identical hashes can't occur, so salting adds nothing. GitHub and npm
make the same choice for their generated tokens. This does **not** cover the owner `-token` /
`WAME_TOKEN`: the owner chooses that value freely. It is never stored, but it can be guessed online
(F9).

### Authentication flow (`withAuth`, `Authenticator.Authenticate`)

1. The token is taken from the `Authorization` header with the literal `"Bearer "` prefix stripped.
   If there is no such prefix, the whole header value is used. If `?token=` is present it **replaces**
   the header value. **Now:** the header always wins and must be `Bearer <token>` (scheme
   case-insensitive); `?token=` is only read on `GET /events`, `GET /qr` and `GET /media/{seq}` (F7, F8).
2. If no token was presented and `Required()` is false (no owner secret and no active DB token), the
   request goes through `localOnly` as the owner. **Now:** `Required()` is true once any token row has
   ever existed, and on DB errors (F1, F2); `localOnly` also requires a loopback `RemoteAddr`.
3. Otherwise:
   - The owner secret is compared first, as `SHA-256(presented)` against `SHA-256(owner)` in constant
     time.
   - Then the token is looked up by its `id`. The hash is compared in constant time, and a dummy
     compare runs when the id is unknown.
   - "revoked" and "expired" are reported only after the secret matched, so they reveal nothing to
     someone without the secret.
   - The timing gap between a known and an unknown id is one indexed SQLite read. The id is not a
     secret anyway, since knowing it without the secret gets an attacker nothing.

### Scopes per route

| Route | Scope | Allowlist handling |
|---|---|---|
| `GET /status` | read | none (returns QR, own JID and push name; see F6). **Now:** QR only for admin; JID and push name hidden from restricted tokens |
| `GET /qr`, `POST /pair`, `POST /logout` | admin | n/a (admin can't be restricted) |
| `POST /send`, `/send/file` | send | `sendGuard`: resolve, then allowlist, then per-token rate. **Now:** restricted tokens are checked with `LocalChat` (no lookup), then the per-token rate |
| `POST /react`, `/edit`, `/delete`, `/read`, `/typing` | send | `sendGuard` on `chat`; target id looked up in that chat only |
| `GET /messages`, `/messages/wait`, `/events` | read | `restrictFilter`: `?chat` must be allowed and forces `ChatOnly`; `Chats IN allowlist` always |
| `GET /media/{seq}` | read | message's chat must be allowed (404 otherwise) |
| `GET /chats` | read | list filtered to the allowlist |
| `GET /contacts`, `/groups` | read | forbidden to restricted tokens. **Now:** `GET /contacts` is filtered to the allowlist (see [Follow-up: contacts](#follow-up-contacts)); `/groups` stays forbidden |
| `POST /check` | send | forbidden to restricted tokens |
| `GET /audit`, `GET/POST /tokens`, `DELETE /tokens/{name}` | admin | n/a |
| `GET /stats`, `GET/PATCH /settings`, `POST /webhook/test`, `/webhook/replay` | admin | n/a (added later, see [Follow-up: admin routes](#follow-up-admin-routes)) |
| `GET /docs`, `/openapi.yaml` | public | n/a |

`admin` implies every scope. Admin tokens can't have an allowlist.

### Allowlist semantics

- Entries are normalized JIDs. Phone entries (`…@s.whatsapp.net`) also match whatever the number
  resolves to on WhatsApp, which covers Brazil's optional 9th digit.
- A group JID in the list allows the whole group, every sender included.
- A person's JID allows only the 1:1 chat. Group messages that person sent are excluded because
  `ChatOnly` is forced.
- Unknown LIDs, device-suffixed JIDs and differently cased servers all fail closed.

### Rate limits

- Per token: an optional `rate` sends per minute (`tokenLimits`, keyed by token id, 30 s max wait),
  applied in `sendGuard` (since split into `authorizeChat` and `throttle`, which runs inside the
  idempotent part of a send, so a retried key doesn't spend a slot), which every `send`-scope route except `/check` passes through. **Now:**
  new tokens default to 10/min (`auth.DefaultRate`); an explicit `0` means global limit only (F11) The
  limiter now lives in `Bot.ThrottleToken` and the MCP send tools use it too (F13).
- Global: `Bot.Limiter` (sends) and `Bot.Lookups` (number checks and presence) still apply to
  everyone.
- The idempotency cache is namespaced by principal name. Names are unique and reserved forever, and
  `api`/`owner` are reserved, so tokens can't share or poison each other's keys.

### Token-less mode

`Required()` is evaluated **on every request**: `Owner != "" || HasActiveTokens()`. The startup guard
in `main.go` (`serve` and `tui -addr`) refuses a non-loopback address only when there is neither an
owner secret nor an active token **at startup**.

**Now:** `Required()` is `Owner != "" || HasTokens()`, where `HasTokens` counts every row ever
created (revoked and expired included) and a DB error counts as "required". Token-less mode only
exists before the first token is ever created, and even then `localOnly` needs a loopback
connection, a loopback `Host` and a same-origin request. `guardPublicAddr` also refuses owner
secrets shorter than 16 characters.

## Library decision: keep the hand-rolled opaque tokens

| Option | Verdict |
|---|---|
| [golang-jwt/jwt](https://github.com/golang-jwt/jwt) (JWT) | No. JWTs are stateless: revoking one needs a denylist that is checked on every request. That is the same table lookup wame already does, plus key management and algorithm-choice pitfalls. Scopes and allowlists would be frozen into the token until it expires. |
| [aidantwoods/go-paseto](https://github.com/aidantwoods/go-paseto) (PASETO v4) | No. It fixes JWT's algorithm agility, but it is still self-contained and stateless, so revocation and changing an allowlist have the same problem. It is worth using only if wame ever needs tokens checked without the DB. |
| [gunsluo/go-api-key](https://github.com/gunsluo/go-api-key) (fork of `stfsy/go-api-key`) | No. Its design matches what wame already does (prefix + short id + hashed long secret), but the fork had 0 stars and no releases at review time. That is a supply-chain dependency with no maturity behind it, for about 40 lines of code that are already correct. |
| [casbin](https://github.com/casbin/casbin) | No. It is a policy engine. Three scopes plus an allowlist fit in `authz.go`, and a policy DSL would add attack surface without adding clarity. |

Reasons to keep the current design:

- Opaque tokens are revoked instantly by a DB lookup that already happens.
- The code adds no new dependencies beyond the standard library (`crypto/rand`, `crypto/sha256`,
  `crypto/subtle`).
- The format already follows the industry pattern from
  [GitHub's token format](https://github.blog/engineering/platform-security/behind-githubs-new-authentication-token-formats/)
  (identifiable prefix, `_` separator).

Optional improvement: append a CRC32 checksum (GitHub's last 6 characters) so secret scanners can tell
real tokens from look-alikes without a DB.

## Findings

| ID | Severity | Title | Status |
|---|---|---|---|
| F1 | High | Token-less mode comes back on a LAN listener after the last token is revoked or expires; `Host: localhost` then gives owner access, including minting tokens | fixed |
| F2 | High | `HasActiveTokens` fails open: any DB error switches to token-less mode | fixed |
| F3 | Medium | Revocation and expiry don't end open `/events` (and `/messages/wait`) streams | fixed |
| F4 | Medium | Restricted tokens trigger `IsOnWhatsApp` lookups before the allowlist check: lookup-budget DoS and a resolution oracle | fixed |
| F5 | Medium | An expiring admin token can mint never-expiring admin tokens; children survive the parent's revocation | fixed |
| F6 | Medium | QR code (admin-only at `/qr`) is readable by any `read` token through `/status` and `/events` | fixed |
| F7 | Medium | `?token=` is accepted on every route, including admin POSTs, and overrides the `Authorization` header | fixed |
| F8 | Low | `Authorization` parsing: a raw token without a scheme is accepted, lowercase `bearer` is rejected | fixed |
| F9 | Low | No throttling of failed authentication; the owner secret has no minimum strength | fixed |
| F10 | Low | No per-token cap on concurrent SSE/long-poll connections; `?since=0` replays the full history per connection | fixed |
| F11 | Low | Tokens with `rate` 0 can use up the global send budget shared with the owner and auto-replies | fixed |
| F12 | Info | Token id collision is reported as "name already exists" | fixed |

### F1: token-less mode comes back on a LAN listener (High)

**Status: fixed.** Once any row has ever existed in `wame_tokens`, tokens are required forever, even
if every token is revoked or expired (`Store.HasTokens`, `Authenticator.Required`). `localOnly` now
also requires a loopback `RemoteAddr`, so a LAN client can't pass by sending `Host: localhost`. The
CLI message is accurate again. Regression test: `TestF1TokenlessModeNeverReturns`.

**Where:**

- `internal/api/authz.go:37` (`presented == "" && !authn.Required()`)
- `internal/auth/auth.go:144` (`Required`)
- `internal/api/middleware.go:16-21` (`localOnly` trusts `r.Host`)
- `main.go:342` (startup-only guard)
- `main.go:555` (the CLI promise "From now on the API requires a token on every request")

**Description:**

1. The owner runs `wame serve -addr 0.0.0.0:8080` without `WAME_TOKEN`. The guard lets this
   through because a DB token exists.
2. Later the last active token is revoked or expires. Revoking is exactly what the owner does after
   a leak, and an admin token can also revoke itself and every other token.
3. From then on `Required()` is false, and requests without a token go to `localOnly`.
4. `localOnly` only checks the `Host` header and `Origin`/`Sec-Fetch-Site`. Those protect against
   browsers (DNS rebinding, CSRF), not against a direct client, which can send `Host: localhost`.

**Proof:**

1. `TestSec_F1_LANOpenAfterLastTokenRevoked` issues one token, revokes it, and sends requests with
   `RemoteAddr=192.168.1.66` and `Host: localhost:8080`, without any token.
2. `GET /status` returns 200.
3. `POST /tokens {"name":"pwn","scopes":["admin"]}` returns **201 with a fresh admin token**. The
   attacker now keeps access even after the owner creates new tokens.
4. `TestSec_F1b_LANOpenAfterExpiry` shows the same thing after a 20 ms TTL runs out (`GET /audit`
   returns 200).

**Impact:** an unauthenticated LAN client takes over completely: it can read all messages, send as
the owner, log out, and mint persistent admin tokens.

**Fix (both parts):**

1. Make the mode sticky. Require a token once any row exists in `wame_tokens`, revoked or expired
   included. Or decide token-less vs required once in `serve`, and refuse to fall back when the
   listener isn't loopback.
2. Defense in depth: in `localOnly`, also require `r.RemoteAddr` to be a loopback IP.

Also correct the CLI message.

### F2: `HasActiveTokens` fails open (High)

**Status: fixed.** `HasTokens` returns `(bool, error)` and `Required()` returns true on any error, so a
DB failure demands a token instead of dropping auth. Regression test: `TestF2RequiredFailsClosed`
(in `internal/auth`).

**Where:** `internal/store/tokens.go:116-119`. The `Scan` error is discarded and `n` stays 0.

**Description:** any SQLite error turns into "no active tokens", so `Required()` is false and
requests without a token are served as the owner (subject to `localOnly`, which F1 shows can be
bypassed from the LAN). Possible causes include a locked or busy database, disk I/O errors, a
corrupted page, or running out of file descriptors.

**Proof:** `TestSec_F2_FailOpenOnDBError` creates a token, then makes the `COUNT` query fail by
renaming the table from a second connection. A LAN request without a token, with
`Host: localhost`, returns 200 on `/status`. `/status` doesn't touch the DB, so the handler keeps
working while auth has failed open.

**Impact:** auth is bypassed whenever the DB misbehaves.

**Fix:** have `HasActiveTokens` (or the sticky replacement from F1) return `(bool, error)`, and make
`Required()` return true on error. The `main.go` guard already fails closed, because an error makes
it refuse non-loopback.

### F3: revocation doesn't reach open streams (Medium)

**Status: fixed.** Open `/events` streams re-check their token every 10 s and before each event, and
close when it's revoked or expired; `/messages/wait` re-checks before each query and answers 401.
Regression test: `TestF3RevocationEndsOpenStreams`.

**Where:** `internal/api/authz.go:28-47`, which authenticates once per request, and
`internal/api/api.go:321-400` (`events`), which loops until the client disconnects and sends a ping
every 25 s that keeps the connection alive. `/messages/wait` is bounded at 120 s.

**Proof:** in `TestSec_F3_SSESurvivesRevocation`:

1. A token opens `/events`.
2. The token is revoked. A new request with it now gets 401.
3. A message saved after the revocation is still delivered on the old stream
   (`event: message … "text":"private after revoke"`).

Expiry behaves the same way.

**Impact:** a leaked or compromised agent token that holds a stream open keeps reading every message
it is allowed to see for as long as the process runs. Revoking it does nothing to that stream.

**Fix:** in the SSE loop, check the principal again on each ping and before each send. Close the
stream when `TokenByID` reports the token revoked, or when `time.Now()` passes its expiry. A cheap
version: keep an in-memory revocation generation counter that `RevokeToken` bumps, and re-check only
when it changes. Give `/messages/wait` the same check before it writes.

### F4: restricted tokens trigger number lookups before the allowlist check (Medium)

**Status: fixed.** Restricted callers' input is never resolved on WhatsApp: `filter()` and
`authorizeChat` use `Bot.LocalChat` (parse + known LID mapping, no network) and `ChatAllowed`.
Allowlist phone entries are expanded once and cached for 10 minutes, including misses
(`Bot.ExpandAllowlist`). The 403 text is fixed: "this token can't access that chat". Regression
test: `TestF4RestrictedTokensDontSpendLookups`.

**Where:**

- `internal/api/api.go:258`: `filter()` resolves `?chat=` with `Resolve` before `restrictFilter`.
- `internal/api/authz.go:97`: `authorizeChat` resolves `to` before checking the allowlist.
- `internal/api/authz.go:127-138`: `allowedChats` resolves every phone entry on every request.
  Numbers that aren't on WhatsApp are never cached, so they cost a lookup each time.
- `internal/api/authz.go:118`: the 403 message echoes the **resolved** JID.

**Description:** a token limited to Maria, even a `read`-only one, can make wame call
`IsOnWhatsApp` for arbitrary numbers. Each call is charged to the shared `Bot.Lookups` budget, the
anti-ban limiter. Mass lookups are what that limiter exists to prevent, and `/check` is deliberately
`send` + unrestricted.

**Proof:** in `TestSec_F4_RestrictedTokenBurnsLookups`, with `Lookups` set to 3/min:

1. A restricted `read` token calls `GET /messages?chat=<three different numbers>`. Each call returns
   403, but each one used a lookup.
2. The owner's next `Resolve` then fails with `rate limited, retry in 1m0s`.

With a live connection, the 403 body (`this token can't access <resolved jid>`) also shows whether
and how a number is registered (for example, the 9th-digit variant). That makes it an `/check`
oracle open to restricted `read` tokens.

**Impact:**

- Owner sends and auto-replies that need a lookup fail with 429 (DoS).
- Number enumeration, which raises the ban risk for the account.
- An information leak that bypasses the `/check` policy.

**Fix:** for restricted principals, never call `Resolve` on caller input.

1. Resolve allowlist entries once, at token creation or on first use, and cache the canonical JIDs
   with the token.
2. Compare the caller's `ParseRecipient(to)` plus `PhoneJID` against that set, with no network call.
3. Return a fixed 403 that doesn't echo the resolved JID.

### F5: admin token expiry is not a ceiling (Medium)

**Status: fixed.** Tokens can't mint `admin` tokens over the API (403; only `wame token create` or the
owner secret can). A token minted by another token records `created_by`, can't outlive its parent
(`expires` is capped at the parent's), and `RevokeToken` cascades to everything it created,
recursively. Regression test: `TestF5ChildTokensAreBoundToTheirParent`.

**Where:** `internal/api/authz.go:185-220` (`createToken`); there is no parent/child link in
`wame_tokens`.

**Proof:** in `TestSec_F5_AdminOutlivesItsExpiry`:

1. An admin token with a 1 h TTL mints `{"name":"forever","scopes":["admin"]}`. The child has
   `Expires: 0`.
2. The parent is revoked. The child stays active.

**Impact:** expiry on an admin token, which you'd give a short-lived agent, does nothing. Whoever
holds it can make their access permanent in one call, and revoking the original doesn't remove it.

**Fix:**

- Cap a minted token's `expires` at the caller's expiry.
- Record `created_by` (the token id).
- Either cascade revocation to children or list them in `wame token list`.
- Consider letting only the owner secret or the CLI create `admin` tokens.

### F6: QR code leaks to `read` tokens (Medium)

**Status: fixed.** `/status` and SSE `status` events (`visibleStatus`) drop `qr` for non-admin
callers, and `jid` / `push_name` for tokens limited to specific chats. Regression test:
`TestF6StatusHidesQRAndAccount`.

**Where:**

- `internal/api/api.go:57`: `/status` is `read` and returns `bot.Status`, which includes `qr`.
- `internal/api/api.go:392`: the `status` SSE events aren't filtered by scope or allowlist.
- By contrast, `/qr` requires `admin`.

**Proof:** `TestSec_F6_QRLeaksToReadScope`. A `read` token limited to Maria gets 403 on `/qr`, but
`/status` returns `"qr":"2@FAKEQRPAYLOAD,…"`.

**Impact:**

- While wame is waiting to be paired (after a logout, for example), any read token, or an agent
  tricked by a prompt injection, can pass the QR payload to someone else. Whoever scans it first
  links their own account to wame, and the owner's agents then send from and read an account the
  attacker controls.
- Restricted tokens also learn the owner's JID and push name.

**Fix:** blank `QR` (and, for restricted principals, `JID`/`PushName`) unless the principal has
`admin`, both in `/status` and in SSE `status` events.

### F7: tokens in URLs, and the query overrides the header (Medium)

**Status: fixed.** The `Authorization` header always wins. `?token=` is read only on `GET /events`,
`GET /qr` and `GET /media/{seq}`; anywhere else it's rejected with 401. Regression tests:
`TestF7QueryTokenOnlyOnBrowserRoutes` and `TestTokenAuth`.

**Where:** `internal/api/authz.go:33-36`.

**Proof:** in `TestSec_F7_QueryAndHeaderParsing`:

- A valid admin `Authorization` header plus `?token=x` returns 401, so the query value wins.
- `POST /tokens?token=<admin>` returns 201, so tokens in URLs work on state-changing admin routes.

**Impact:** URLs end up in reverse-proxy logs, browser history, shell history and agent transcripts.
Accepting tokens in URLs on every route invites leaking admin tokens. Because the query value wins,
a header token can be silently replaced by a URL the client didn't build, such as a link from a
prompt injection. The request is then made as a different principal, which confuses attribution.

**Fix:**

- The header always wins.
- Accept `?token=` only on `GET /events` and `GET /media/{seq}`, where `EventSource` and `<img>`
  can't set headers, and reject it everywhere else with 400.
- Document it as a last resort.

### F8: lax `Authorization` parsing (Low)

**Status: fixed.** The header must be `<scheme> <token>` with the scheme equal to `Bearer`
case-insensitively; a bare token or another scheme is a 401. Covered by the `TestTokenAuth` cases.

**Where:** `internal/api/authz.go:33`. `CutPrefix` ignores whether the prefix was found.

**Proof:** `Authorization: <token>` without a scheme returns 200. `Authorization: bearer <token>`
returns 401, although RFC 6750 makes the scheme case-insensitive.

**Impact:** mostly interoperability. Accepting values without a scheme also means a `Basic …` header
added by a proxy gets hashed and compared as if it were a token.

**Fix:** split on the first space. Require `strings.EqualFold(scheme, "Bearer")` and trim the token.

### F9: no failed-auth throttling; weak owner secret allowed (Low)

**Status: fixed.** 10 failed authentications per minute from one IP blocks that IP for a minute
(429, `Retry-After: 60`). The owner secret must be at least 16 characters; `serve` and `tui -addr`
refuse to start otherwise. Regression test: `TestF9FailedAttemptsAreThrottled`.

**Where:** `internal/api/authz.go:41-44`, `main.go` (`-token` / `WAME_TOKEN` are not validated).

**Description:** generated tokens can't be brute-forced (256 bits). The owner secret is
user-chosen, though, and could be `-token test`. Every 401 is free and immediate, so a LAN attacker
can guess online without limit.

**Fix:** refuse (or at least warn about) owner secrets shorter than 32 characters, and suggest
`openssl rand -base64 32`. Add a small per-remote-IP backoff after repeated 401s.

### F10: unbounded streams per token (Low)

**Status: fixed.** At most 4 concurrent `/events` + `/messages/wait` connections per token; more get
429. Regression test: `TestF10StreamsPerTokenAreCapped`.

**Where:** `internal/api/api.go:321-400`, `290-319`.

**Description:**

- Any `read` token can open many `/events` connections.
- Each `?since=0` connection first replays the whole message table in pages of 500.
- The server has no `WriteTimeout`, which is correct for SSE but means nothing bounds a connection.

**Impact:** CPU, I/O and memory DoS from a single low-privilege token.

**Fix:** cap concurrent streams per token id (for example 4) and per IP for unauthenticated
attempts. Optionally cap the catch-up depth for restricted tokens.

### F11: tokens can use up the global send budget (Low)

**Status: fixed.** Tokens get a default per-token rate of 10 sends per minute (`wame token create
-rate`, default 10; over the API, omitting `rate` gives 10 and an explicit `0` means global limit
only). Asserted in `TestF5ChildTokensAreBoundToTheirParent`.

**Where:** `internal/api/authz.go:146-165`. `rate` 0 means "global limit only".

**Description:** one `send` token with no per-token `rate` can fill `Bot.Limiter`'s per-minute
window. The owner's TUI/CLI sends and auto-replies then wait or get 429. F4 does the same to the
lookup budget.

**Fix:** give tokens a default per-token rate below the global one (for example half), or keep a
reserved share for the owner.

### F12: id collision is reported as a name conflict (Info)

**Status: fixed.** `InsertToken` distinguishes `ErrTokenID` from `ErrTokenName`, and `Issue` retries
with a fresh id on `ErrTokenID` (up to 3 times).

**Where:** `internal/store/tokens.go:49-55`.

**Description:** any UNIQUE violation is mapped to `ErrTokenName`, including a clash on the 40-bit
`id` primary key. That needs about a million tokens before a collision becomes likely, so the impact
is a misleading error, not a security issue.

**Fix:** retry `Issue` with a new id when the violation is on `wame_tokens.id`.

### Checked and not a problem

- **Owner path:** `SHA-256(presented)` vs `SHA-256(owner)` with `ConstantTimeCompare` is fine. Both
  inputs have a fixed length, so the secret's length doesn't leak.
- **Parsing:** secrets containing `_` parse correctly, because the split happens at the first `_`
  after the prefix and the id alphabet has no `_`.
- **Error messages:** "revoked" and "expired" only follow a correct secret.
- **Plaintext handling:** the plaintext never reaches `GET /tokens`, slog lines or the audit log.
- **Escalation:** non-admin tokens have no route to `/tokens`, `/audit`, `/logout`, `/pair` or `/qr`.
- **Names:** `api`, `owner`, `local`, `tui`, `cli`, `autoreply` and `whatsapp` are reserved (they're audit actors), case is enforced by the pattern, and revoked names can't
  be reissued. The owner can't be revoked through `DELETE /tokens/{name}`.
- **Message ids:** `reply_to`, `react`, `edit`, `delete` and `read` all look up the id inside the
  authorized chat (`Store.Get(chat, id)`), so ids from other chats return 404.
- **SSE `update` events** go through the same `Chats` filter as messages.
- **Rate limits:** `/read` and `/typing` count against the per-token rate (by design). The per-token
  limiter map is bounded by the number of tokens.
- **Unauthenticated cost:** one SQLite read per request (indexed `TokenByID` or `COUNT(*)`).
  Acceptable, aside from F2.

## Follow-up: MCP (F13–F16)

| ID | Severity | Title | Status |
|---|---|---|---|
| F13 | Medium | MCP send tools skipped the per-token rate (reopened F11 over `/mcp` and `wame mcp`) | fixed |
| F14 | Medium | Failed logins on `/mcp` weren't throttled (reopened F9: owner-secret guessing through `/mcp`) | fixed |
| F15 | Low | `wame mcp -mcp-token` checked the token only at startup; revocation and expiry didn't reach a running process | fixed |
| F16 | Low | The MCP `status` tool returned the account JID to tokens limited to chats (F6 not carried over) | fixed |

- **F13.** The per-token limiter moved from the API into `Bot.ThrottleToken`, so a token has one budget
  whether it calls `/send` or `send_message`. `send_message`, `react` and `mark_read` go through
  `sendTarget`, which checks the allowlist and then charges the rate. Test: `TestSendsUseTheTokenRate`.
- **F14.** `/mcp` shares the API's `failureLimiter`: a blocked IP gets 429 on `/mcp`, and failed bearer
  tokens on `/mcp` count toward the block. Test: `TestMCPFailedAttemptsShareTheThrottle`.
- **F15.** Every MCP tool call re-reads the token (`require`) and refuses it once revoked or expired,
  for HTTP and stdio alike. Test: `TestRevokedTokenStopsWorking`.
- **F16.** `status` drops `account` for restricted callers, matching `visibleStatus` on HTTP.
- Also: `wame token list` shows `CREATED BY`, so the revoke cascade is visible from the CLI; a stale doc
  comment on `NewHandler` was removed. `/typing` was checked and already goes through `sendGuard` (allowlist
  and per-token rate), so its presence lookup is bounded per token.

## Follow-up: admin routes

Added after the review; all require `admin` (so they're never reachable by a restricted token) and
go through the same `withAuth` and failed-login throttle.

- **`GET /stats`** exposes, per caller name, requests and sends in the last 24 h, last seen time,
  last client IP and open streams; plus limiter and webhook counters. All in memory, reset on
  restart. It never includes the webhook URL, its secret or message text (`TestWebhookStats`).
- **`PATCH /settings`** can raise `rate` (up to 1000/min, or 0 = unlimited) and change the webhook
  filter, so an admin token can loosen the anti-ban limits; the change is audited with its values
  and persists across restarts until reset. Every value is validated before any is applied
  (`TestRuntimeSettings`). Per-token rates still apply on top.
- **`POST /webhook/replay`** only accepts delivery ids from the recent failures list, so it can't be
  used to push arbitrary stored messages, including ones the filter excluded, to the webhook. The
  body is rebuilt from the stored message (`TestWebhookTestAndReplay`).
- **`wame admin`** sends the token on every request: it refuses plain http beyond loopback unless
  told otherwise, and a server address read from an env file is only used with a token from the
  same file, so a stray `.env` can't redirect a token from the shell to another host
  (`TestResolve`, `TestNewClientGuardsTheToken`). Text other people wrote is stripped of control
  characters before it's drawn (`TestSafe`).

## Follow-up: contacts

Your own contact list (`wame_contacts`: name, tags, notes, language, time zone per person) changed
the token model in one place: **`GET /contacts` went from 403 to filtered** for tokens limited to
chats.

| Route | Scope | Allowlist handling |
|---|---|---|
| `GET /contacts` | read | `Bot.Contacts` with `Allow` = the token's list, expanded by `ExpandAllowlist` (only the list's own entries are ever resolved); `tag=` and `q=` filter inside it |
| `GET /contacts/tags` | read | forbidden (`unrestricted`): tag names and counts span every contact |
| `GET /contacts/{number}` | read | `authorizeChat` (`LocalChat` + `ChatAllowed`, no lookup), the fixed 403 text otherwise; a restricted token gets the basic profile; the full one (notes included) needs an unrestricted admin token. `auth.Issue` never mints an admin token limited to chats, and the handler checks `admin && !Restricted()` anyway as a safety net |
| `GET /contacts/{number}?refresh=1` | admin | needs an unrestricted admin token, else 403 `refresh=1 needs an admin token without a chat allowlist` (the same safety net), before anything is looked up. Refused too (403 `refresh=1 is refused from another site or port`) when `Sec-Fetch-Site` is present and not `same-origin`/`none`, or an `Origin`/`Referer` host differs from `Host`: a refresh spends lookups from a GET, so otherwise a page on another localhost port could trigger it with an `<img>` in token-less mode. Applied in every mode; CLI and admin clients send none of these headers |
| `PUT`/`PATCH`/`DELETE /contacts/{number}`, `DELETE /contacts?all=true`, `POST /contacts/import`, `GET /contacts/export` | admin | n/a |

- **Why filtered is safe.** A restricted token could already see these people's names through
  `/chats` and the messages it can read. The list never carries notes, and its count is that of
  the allowed chats, so it says nothing about the rest of the list. Tags it sees are only those of
  allowed people; the counts per tag stay admin-ish (unrestricted only).
- **No oracle, no lookups.** `{number}` outside the list gets the same `this token can't access
  that chat` as every other route, whether or not the person is in the list, and nothing a
  restricted token sends is looked up on WhatsApp. Even unrestricted reads never look anything up;
  only an admin `PUT` while logged in resolves the number once.
- **Writes are admin only.** A name feeds every token's display and a tag will decide who a
  `tag:` batch reaches, so a `send` or `read` token can't change them.
- **Notes** are only in `own`, which is returned to admin callers only, and in the admin-only CSV
  export (audited as `contact_export`). MCP's `find_contact` and `contact_profile` never return them, even to an admin
  connection (`contact_profile` uses the allowlist, with no lookups). Audit details carry field names and counts, never values; the `error` of a rejected
  write may quote the refused tag, language, time zone or field name (cut to at most 64
  characters), and the audit log is admin-only.
- **9th digit, mobiles only.** The variant with or without Brazil's 9th digit is only computed when
  the 8 digits after it start with 6 to 9, so `5521923456789` is never read as the landline
  `552123456789`: a token allowed only the mobile can't see the landline's name or tags, and an
  admin write or delete on one never touches the other's row.
- **Sizes.** Bodies are capped at 64 KB (`PUT`/`PATCH`) and 4 MB / 5000 rows (import), 413 above;
  at most 100,000 people in the list.

Regression tests: `TestContactMobileNeverHitsLandline`, `TestRestrictedContacts` (list filtered, `/contacts/tags` 403, `{number}`
outside the list 403 with the fixed text and no lookups), `TestContactReads` (a `read` token
never sees `own`), `TestContactWrites` (admin only, unknown fields, 413, audit without values),
MCP `TestFindContact`.

**The queue (contacts phase 3).** `POST /queue` and MCP `schedule_message` accept `tag:NAME`, and
templates that fill in names:

- **Both need `read` as well as `send`**: the queued items show who has the tag, and the filled
  text (returned to the token by `GET /queue/{id}`) shows names, which a `send`-only token can't
  read anywhere else. `Bot.Queue` checks it (`QueueOptions.Read`), so every front end gets the
  same rule.
- **Restricted tokens:** a tag's members outside the allowlist are dropped before anything is
  stored, counted or listed in `skipped_rows`; an empty result is `tag NAME has no contacts this
  token can reach`, whether the tag is empty or only has others. Members are checked with
  `ChatAllowed` on their stored key: nothing is looked up.
- **`{name}` never uses a push name** (a stranger's choice, sent under your account);
  `{push_name}` is explicit.
- **`skip_sent` without `admin`** counts only the token's own queue items (`OutboxFilter.For`), so
  it can't tell whether another token messaged someone; sent messages in `wame_messages` count only
  for a token with `read`, which could list them anyway.

Regression tests: `TestQueueTagsTemplatesAndSkips` (api), `TestQueueTagExpansion`,
`TestQueueSkipSent` (bot), MCP `TestScheduleTagAndTemplate`.

## Residual risks (accepted by design)

- Per-token rates default to 10/min against a global 20/min, so two busy `send` tokens can still fill
  the owner's budget. There's no reserved share for the owner; lower `-rate` on agent tokens if that
  matters.
- A restricted token's allowlist entries that are phone numbers are resolved on WhatsApp (cached for
  10 minutes) to match the 9th-digit variant. The caller can't choose what's looked up, only the list's
  own entries are.
- `wame mcp` runs auto-replies like `serve` does; pass `-autoreply=false` for a read-only session.

- A restricted `send` token for a group where the owner is an admin can delete other members'
  messages in that group.
- `read` without an allowlist is "all chats". Agents should get an allowlist unless they truly need
  everything.
- Anyone who can read `wame.db` has the WhatsApp session keys. Token hashes are irrelevant at that
  point.
- Token-less loopback mode trusts every local user and process on the machine, and any process that
  can reach `127.0.0.1` (containers with host networking, for example).
- Prompt injection: an agent with `send` can be steered into sending to any chat on its allowlist.
  Allowlists and per-token rates limit the damage, but can't prevent it.

## Operational guidance

- **One token per agent.** Use the narrowest scopes (`read` alone if it only reads), an allowlist
  (`-chats 5511999999999`), a per-token `-rate`, and an `-expires` (for example `720h`).
- **Rotate** by creating `agent-v2`, switching the agent over, then revoking `agent`. Names can't be
  reused. The admin panel's `R` does create-then-revoke in one step (`agent` → `agent-2`), keeping
  scopes, allowlist, rate and lifetime; tokens unused for 30 days are flagged there.
- **Revoke immediately** on any suspected leak (`wame token revoke NAME`). Open streams close within
  10 s, and tokens that token created are revoked with it.
- **On a LAN listener**, an owner secret or any token ever created keeps the API locked; there is no
  way back to token-less mode short of deleting the `wame_tokens` rows by hand.
- **Pass secrets through environment variables** (`WAME_TOKEN`), not the `-token` flag, which shows
  up in `ps` and shell history. Generate the owner secret with `openssl rand -hex 24` (at least 16
  characters are required).
- **Never put tokens in prompts, URLs or committed files.** Give an agent its token through its tool
  configuration or environment, not in conversation text it can repeat. Use the
  `Authorization: Bearer` header. `?token=` works only on `GET /events`, `GET /qr` and `GET /media/{seq}`
  (for an `EventSource` or `<img>` that can't send headers); never use it behind a proxy that logs URLs.
- **Prefer loopback.** For remote access, use an SSH tunnel or a reverse proxy with TLS rather than
  binding `0.0.0.0`. The API speaks plain HTTP, so on a LAN the token travels in clear text.
- **Scan for leaks.** Every token starts with `wame_`. A pattern such as
  `wame_[a-z2-7]{8}_[A-Za-z0-9_-]{43}` finds them in logs and repositories.
- **Review regularly:** run `wame token list` (look at last-used and expiry) and `GET /audit` for
  `token_create` / `token_revoke`.
