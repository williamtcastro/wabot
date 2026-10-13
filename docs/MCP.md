# MCP server

wame speaks the [Model Context Protocol](https://modelcontextprotocol.io), so AI agents (Claude Code,
Claude Desktop, other MCP clients) can read and send WhatsApp messages through it. It's built on the
official Go SDK, `github.com/modelcontextprotocol/go-sdk` v1.8.0.

The MCP tools call the same `Bot` methods as the HTTP API. The global rate limits, the audit log, scopes
and chat allowlists all apply. The audit actor is the token's name, or `mcp` for a stdio server that
isn't using a token.

## Two ways to connect

### 1. HTTP at `/mcp` on a running server (recommended)

`wame serve` (and `wame tui -addr …`) also serves MCP over streamable HTTP at `/mcp`:

```sh
WAME_TOKEN=$(openssl rand -hex 24) ./wame serve      # or use tokens only, see below
./wame token create claude -scopes read -chats 5511999999999 -expires 720h
```

- Each request carries `Authorization: Bearer <token>`. That token's scopes and chat allowlist are
  checked on **every tool call**, so give each agent its own token (see
  [API.md → Tokens and scopes](API.md#tokens-and-scopes)).
- An MCP session is pinned to the token that opened it; another token can't take it over.
- A revoked or expired token fails on its next request.
- Before any token exists and without `-token`, `/mcp` works like the rest of the API in token-less
  mode: same-origin requests over a loopback connection to a loopback `Host` only, acting as the owner.
- It shares the process with the API and TUI, so there's only one WhatsApp connection.

Failed logins on `/mcp` count toward the same per-IP block as the rest of the API, and the send tools
charge the token's own per-token rate (one budget shared with its HTTP calls). The global limits apply
too.

### 2. stdio: `wame mcp`

```sh
wame mcp                     # read-only: status, list_chats, read_messages, search_messages, find_contact
wame mcp -allow-send         # also send_message, react, mark_read
wame mcp -mcp-token wame_…  # act as that API token (its scopes and chat allowlist)
```

- `-mcp-token` (or `WAME_MCP_TOKEN`) must be a token created with `wame token create` or `POST /tokens`.
  The owner secret doesn't work there. Every tool call re-checks the token, so revoking it or letting it
  expire takes effect at the next call, even in a `wame mcp` that's already running.
- It holds **its own WhatsApp connection**, so it can't run alongside `wame serve` or `wame tui` on the
  same database: they'd kick each other off. If you already run the server, use `/mcp` instead.
- It needs an existing login (`wame login`).
- stdout carries the protocol. App logs go to stderr (or `-log-file`), and whatsmeow's logs are off.
- All global flags apply (`-db`, `-rate`, …). Auto-replies stay on unless you pass `-autoreply=false`.

## Tools

| Tool | Scope | Input | Output |
|---|---|---|---|
| `status` | read | none | `connected`, `logged_in`, `account` (your JID; empty for tokens limited to chats), `state` |
| `list_chats` | read | `limit` (default 30, max 200), `unread_only` | `chats[]`: `chat`, `is_group`, `unread`, `last_at`, `last_from` (`me`/`them`), `untrusted.name`, `untrusted.last_text` |
| `read_messages` | read | `chat` (JID or number), `limit` (default 50, max 200), `since_seq` | `messages[]` oldest first, `next_seq`, `note`. Without `since_seq`, the latest messages; pass `next_seq` as `since_seq` to page forward |
| `search_messages` | read | `query`, `chat` (optional), `limit` (default 30, max 200) | `messages[]` newest first; same matching as `q=` in the API |
| `find_contact` | read, **unrestricted tokens only** | `query` (part of a name or number) | `contacts[]` (max 50): `chat`, `phone`, `untrusted.name`, `untrusted.push_name` |
| `send_message` | send | `to`, `text`, `reply_to` (optional) | `seq`, `id`, `chat` |
| `react` | send | `chat`, `message_id`, `emoji` (empty removes) | `seq`, `id`, `chat` |
| `mark_read` | send | `chat`, `message_ids` (optional; omit for the whole chat) | `marked` |

Each message in `messages[]` has `seq`, `id`, `chat`, `from` (`me`/`them`), `sender`, `type`,
`reply_to`, `edited`, `deleted`, `has_media`, `timestamp`, and `untrusted.text`,
`untrusted.push_name`, `untrusted.file_name`.

Read tools are annotated read-only. `send_message` is marked destructive and open-world, and
`react` / `mark_read` open-world. A restricted token gets "this connection can't access that chat" for
chats outside its allowlist. A rate-limited send comes back as "rate limited: wait N seconds before
trying again".

## Safety

- **Untrusted text.** Message text, names, captions and file names were written by other people.
  They're returned only under `untrusted`, each list carries a `note` saying so, and the server
  instructions tell the model to treat them as data, never as instructions.
- **Cleaning.** Untrusted fields have every invisible character removed: control characters, format
  characters (zero-width spaces and joiners, bidi marks and overrides, soft hyphens, the invisible
  "tag" characters used to smuggle hidden text) and variation selectors. They're capped at 4000
  characters. Message ids, which the sender's app chooses, are cleaned too and capped at 128 bytes.
- **Server instructions** also say: only send, react or mark read when the user asked, and wait
  instead of looping when rate limited.
- **Limits and audit.** Global send and lookup limits apply. Every send, reaction and read is in
  `GET /audit` under the token's name, or `mcp` for stdio without a token.
- **Approvals.** Configure your client to ask before each `send_message`, `react` and `mark_read`
  call. Prompt injection through a message can't send anything if a human approves every send.
- Prefer a `read`-only token with a chat allowlist, and add `send` only for agents that need it.

## Client setup

### Claude Code

HTTP (server already running):

```sh
claude mcp add --transport http wame http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer $WAME_TOKEN"
```

Use a token made for the agent (`wame token create claude …`) rather than the owner secret.

stdio:

```sh
claude mcp add wame -- wame mcp                  # read-only
claude mcp add wame -- wame mcp -allow-send
```

### Claude Desktop

stdio, in `claude_desktop_config.json`. Use the absolute path to the binary, since Desktop doesn't
use your shell's `PATH`:

```json
{
  "mcpServers": {
    "wame": {
      "command": "/usr/local/bin/wame",
      "args": ["mcp"],
      "env": { "WAME_DB": "/Users/maria/.config/wame/wame.db" }
    }
  }
}
```

To reach `/mcp` on a running server from Desktop, bridge it with
[`mcp-remote`](https://www.npmjs.com/package/mcp-remote):

```json
{
  "mcpServers": {
    "wame": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:8080/mcp", "--header", "Authorization: Bearer ${WAME_AGENT_TOKEN}"],
      "env": { "WAME_AGENT_TOKEN": "wame_…" }
    }
  }
}
```
