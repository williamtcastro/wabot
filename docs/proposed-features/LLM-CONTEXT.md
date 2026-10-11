# Conversation context for LLMs (proposal)

> **Deferred proposal.** Not built. Today an agent that wants "the last 50 messages of this chat"
> calls `GET /messages` and turns the Message objects into a prompt itself. See the
> [list of proposals](README.md).

## Goal

One call that returns a chat as a compact, prompt-ready transcript: who said what, replies shown
inline, media described in words, reactions folded into the message they belong to, edits and
deletes already applied, day markers, a size budget, and a cursor so the next call returns only
what's new.

Non-goals: summarizing (that's the LLM's job), transcribing voice notes or describing images
(no model calls inside wabot), cross-chat context, any change to `/messages`.

## Endpoint

`GET /chats/{jid}/context`

`{jid}` is a phone number or JID, resolved exactly like `chat` in [Filters](../API.md#filters)
(`Bot.Resolve`, then `PhoneJID`), but matched with `ChatOnly: true` so a person's group messages
don't leak into the 1:1 transcript.

| Query | Default | |
|---|---|---|
| `limit` | 50 | max messages before folding (up to 500). Reactions don't count |
| `since` | none | a `seq`. Only messages after it, oldest first. Without it: the newest `limit` messages |
| `budget` | none | max size of the transcript. `2000` means 2000 tokens; `8000c` means 8000 characters |
| `format` | `text` | `text` (plain transcript) or `json` (structured lines) |
| `tz` | server local | IANA zone for times and day markers, e.g. `America/Sao_Paulo` |
| `names` | `contacts` | `contacts` (address-book name, then push name, then number) or `numbers` (no names at all) |
| `media` | `describe` | `describe` (`[photo: caption]`) or `omit` (drop media lines without text) |

**Scope:** `read`, plus the chat must be in the token's chat allowlist (404, not 403, when it
isn't, so a token can't probe which chats exist).

### Plain-text response

`Content-Type: text/plain; charset=utf-8`. Metadata goes in headers so the body can be pasted into
a prompt as is:

| Header | |
|---|---|
| `X-Wabot-Next-Since` | highest `seq` covered; pass it as `since` next time |
| `X-Wabot-Truncated` | `0`, or how many messages were dropped to fit `budget` |
| `X-Wabot-Chat` | the resolved chat JID |

```
Chat with Maria (+5511999999999). Times in America/Sao_Paulo.
--- Fri 10 Oct 2026 ---
[20:41] Maria: oi! are we still on for saturday?
[20:42] me: yes, 10am at the park
[20:42] Maria: [photo: the new bike] (reactions: me ❤️)
[20:45] Maria (replying to me: "yes, 10am at the park"): can John come too?
[20:46] me: sure (edited)
[20:47] Maria: [message deleted]
--- Sat 11 Oct 2026 ---
[09:12] Maria: [voice message, 0:14]
[09:13] Maria: [document: route.pdf, 220 KB]
```

Groups get the group name in the header line and a name on every line:

```
Group "Family" (120363012345678901@g.us), 12 members. Times in America/Sao_Paulo.
--- Sat 11 Oct 2026 ---
[10:02] Ana: who's bringing the cake?
[10:03] John (replying to Ana: "who's bringing the cake?"): me 🎂 (reactions: Maria 👍, Ana 😂)
```

### JSON response

```json
{
  "chat": "5511999999999@s.whatsapp.net",
  "title": "Chat with Maria (+5511999999999)",
  "tz": "America/Sao_Paulo",
  "next_since": 1051,
  "truncated": 0,
  "approx_tokens": 143,
  "lines": [
    { "day": "2026-10-10" },
    { "seq": 1042, "id": "3EB0C767D26A1D0F5E2A", "at": "2026-10-10T20:42:00-03:00",
      "who": "Maria", "from_me": false, "type": "image", "text": "[photo: the new bike]",
      "reactions": [{ "who": "me", "emoji": "❤️" }] },
    { "seq": 1044, "id": "3EB0A1B2C3D4E5F60718", "at": "2026-10-10T20:45:00-03:00",
      "who": "Maria", "from_me": false, "type": "text", "text": "can John come too?",
      "reply_to": { "id": "3EB0D4C3B2A1F0E9D8C7", "who": "me", "excerpt": "yes, 10am at the park" } }
  ]
}
```

`id` is there so an agent can answer with `reply_to` or react without a second lookup.

## Rendering rules

| Stored message | Rendered as |
|---|---|
| `text` | the text. Newlines kept, but runs of blank lines collapsed |
| `image`, `video` | `[photo: caption]` / `[video: caption]`, or `[photo]` without a caption |
| `audio` | `[voice message, 0:14]` when the duration is in `raw` (`AudioMessage.Seconds`), else `[voice message]` |
| `sticker` | `[sticker]` (plus the emoji, if `raw` has one) |
| `document` | `[document: name.pdf, 220 KB]`, then the caption |
| `location` | `[location: -23.55,-46.63]` |
| `reaction` | not a line. Folded into the target as `(reactions: Maria 👍, me ❤️)`. Only the latest reaction per sender counts; an empty one removes it. Reactions to messages outside the window are dropped |
| `edited` | current text plus ` (edited)` |
| `deleted` | `[message deleted]`. Kept as a line because the conversation may refer to it |
| reply | `(replying to WHO: "excerpt")` after the name. Excerpt is the first 80 characters of the quoted message's current text, or its media description, or `[deleted message]` |

- **Names.** `me` for `from_me`. Otherwise `Bot.ChatName` for 1:1 chats and, in groups, the
  contact name for the sender JID, falling back to `push_name`, then `+number`. With `names=numbers`
  every person is `+number` and the header has no name.
- **Day markers** when the calendar day changes in `tz`. Times are `HH:MM`.
- **Sanitizing.** Strip control characters and bidi overrides (the TUI's `safe()` covers most of
  this; move the shared part down to `store` or a small `internal/text` package rather than copying
  it). Don't escape anything else: it's plain text for a model, not HTML.
- **Prompt injection.** The transcript is untrusted input written by strangers. The plain-text
  header doesn't try to "defend" against it, but each line starts with a fixed `[HH:MM] name:`
  pattern and a message can't forge a new line (newlines inside a message are indented by two
  spaces), so a client can tell messages apart. Document that callers should still wrap the
  transcript in their own delimiters.

## Budget and truncation

- `budget=N` is tokens, estimated as `ceil(chars / 4)`; `budget=Nc` is exact characters. No
  tokenizer dependency: the estimate is for sizing, and callers that need precision pass `c`.
- Fill from the newest message backwards, so the end of the conversation (what the agent answers)
  is always kept. The header line and day markers count against the budget.
- A single message longer than a quarter of the budget is clipped in the middle with
  `… [N chars cut] …`.
- When anything is dropped, the transcript starts with `[… N earlier messages not shown]` and
  `X-Wabot-Truncated` / `truncated` say how many.
- Reply excerpts stay at 80 characters regardless, since they're what keeps a clipped window
  understandable.
- Without `budget`, `limit` is the only cap.

## Cursor ("only what's new")

- `next_since` is the highest `seq` that was read, including reactions and messages dropped by the
  budget (so they're not offered again).
- With `since`, the endpoint returns the messages after it, oldest first, with the same rendering.
  Reactions that arrive later for an older message show up as their own line,
  `[20:50] Ana reacted 👍 to me: "sure"`, because the target isn't in the window.
- Edits and deletes don't create a new `seq`, so they're invisible to the cursor. For agents that
  care, add `changed=true`: also return messages in the chat whose `edited`/`deleted` changed after
  the cursor. That needs a `changed_seq` column (set from a global counter by `MarkEdited` /
  `MarkDeleted`). Leave it for phase 2; see open questions.

## Where the code goes

- `internal/store`: a `Store.ByIDs(chat string, ids []string) (map[string]Message, error)` for
  reply targets outside the window, so the endpoint does one extra query instead of one per reply.
  The window itself is `Store.Query(Filter{Chat: jid, ChatOnly: true, Since: since}, limit, since > 0)`,
  reversed when newest-first. Reactions are fetched by the same query (they're in the chat) and
  folded in memory.
- `internal/bot/context.go` (new): `Bot.Context(ctx, chat string, opt ContextOptions) (Transcript, error)`.
  Does name lookups (`ChatName`, contacts, group participants from the cache) and reads `raw` for
  audio duration. Returns a `Transcript` with `Lines []ContextLine`; rendering text vs JSON is a
  method on it, so the TUI or an MCP server can use it without HTTP.
- `internal/api/api.go`: `GET /chats/{jid}/context`, parse query, call `Bot.Context`, write text or
  JSON. Not audited (it's a read, like `/messages`).
- Docs: `docs/API.md`, `docs/openapi.yaml`, and a short "feeding a chat to an LLM" section in
  `docs/BOT.md`.

## MCP

An MCP server now exists ([MCP.md](../MCP.md)): `/mcp` on `serve` and `wabot mcp` on stdio, with
`list_chats`, `read_messages`, `search_messages` and send tools, using the same scopes and allowlists.
`Bot.Context` would add to it:

- **Resource** `wabot://chat/{jid}/context`: the plain-text transcript with default options.
  Resource list = `GET /chats` filtered by the token's allowlist.
- **Tool** `get_chat_context {chat, limit?, since?, budget?}` returning the text variant and
  `next_since`.
- `list_chats` and `send_message` already exist; once [drafts](DRAFTS-APPROVAL.md) exist, a
  draft-creating tool may be a safer default for agents than sending directly.

Both the HTTP and stdio servers live in `internal/mcpserver`, so a new tool there is available on both.
`wabot mcp` opens the DB and its own WhatsApp connection today; a thin stdio client of a running
`serve` remains an open idea.

## Privacy

- This endpoint exists to send private conversations to a third-party model. Say so plainly in
  the docs, and make the chat allowlist on the token the primary control.
- `names=numbers` for setups that shouldn't expose address-book names to the model. A future
  `redact=phones,emails` option could mask numbers and emails inside message text; not in v1.
- Don't log transcripts or query strings (already the rule). The request log line is enough.
- No caching of rendered transcripts on disk.
- Group transcripts include people who never agreed to an LLM reading them. Consider making
  groups opt-in per token (`allow_groups`), see open questions.

## Tests

- `internal/store`: `ByIDs` returns only rows in the given chat; deleted rows come back flagged.
- `internal/bot`: build messages by hand on a fresh DB and check the text rendering line by line:
  reactions folded (latest per sender, removal), reply excerpt inside and outside the window, edited
  and deleted, day marker across midnight in a fixed `tz`, group names, `names=numbers`, newline
  indentation (a message can't fake a `[12:00] me:` line), control characters stripped.
- Budget: newest kept, `truncated` count, long-message clipping, `c` suffix, header counted.
- Cursor: `next_since` covers dropped messages and reactions; late reaction rendered as its own line.
- `internal/api`: 404 for a chat outside the allowlist, `text` vs `json` content types and headers,
  bad `budget`/`tz` → 400.

## Effort and risks

- Endpoint, rendering, budget and cursor: 2–3 days with tests. MCP stdio client: another 1–2 days.
- Risks: name lookups per sender in large groups (cache per request); the 4-chars-per-token estimate
  is off for emoji-heavy or non-Latin text (document it, offer `c`); renderer drifting from the TUI's
  labels (share the type → label table).

## Open questions

- Should `changed=true` (edits/deletes after the cursor) be in v1? It needs a schema change.
- Groups opt-in per token, or same as 1:1 chats?
- Is `tz` worth a parameter, or is the server's zone enough?
- Should `budget` default to something (e.g. 4000 tokens) to protect clients that forget it?
- Include a short "participants" list for groups in the header, or only names as they appear?
