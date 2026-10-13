# Proposed features

Ideas we may build later. **Nothing here exists yet**: these are plans, kept so the thinking isn't
lost and so anyone picking one up starts from the same notes. Each proposal says what it would do,
how it fits into the current code, what's missing, and the open questions. When one is built, its
user-facing parts move into the regular docs (`docs/*.md`) and the proposal is deleted or trimmed.

| Proposal | Summary |
|---|---|
| [ADMIN-PANEL.md](ADMIN-PANEL.md) | A small web admin panel served by `wame serve`: status and QR login, chats, send form, audit log, webhook and rate-limit health. Also documents the logging and audit structure that exists today. The terminal `wame admin` and the server routes it uses (`/stats`, `/settings`, `/webhook/*`) are built; the web panel isn't |
| [IMAGE-PREVIEWS.md](IMAGE-PREVIEWS.md) | Inline photo and sticker previews in the TUI using the Kitty graphics protocol (and fallbacks), instead of text labels |
| [LLM-CONTEXT.md](LLM-CONTEXT.md) | `GET /chats/{jid}/context`: a chat as a compact, prompt-ready transcript (names, inline replies, media described, reactions folded) with a size budget and a "what's new" cursor; also backs an MCP tool/resource |
| [DRAFTS-APPROVAL.md](DRAFTS-APPROVAL.md) | Agents propose replies with `POST /drafts`; a human approves, edits or rejects them in the TUI or API. Per-chat and per-token policy, expiry, staleness checks, no self-approval; sending reuses `Bot.SendText` |
| [RULES-AND-STATE.md](RULES-AND-STATE.md) | Declarative routing rules (match chat, sender, type, regex, mentions, voice notes → webhook, agent stream, react, drop) with dry runs, plus per-chat key-value state with TTL and CAS; SQLite by default, Redis optional |
| [BACKLOG.md](BACKLOG.md) | Smaller ideas: scheduled backups inside wame, a Docker health check, usage and failure history that survives restarts, retrying failed queue items, a batch view, keeping incoming media on disk (today it's fetched from WhatsApp on every request and expires), a starter n8n workflow, ntfy alerts, token expiry warnings |
| [QUEUE-FILES.md](QUEUE-FILES.md) | Images and documents in the send queue: upload once (`POST /queue/files`, deduped by sha256), reference the id from `POST /queue`; files on disk 0600 with quotas and pruning, one WhatsApp upload reused for many recipients within a short TTL, MIME sniffing, never served back |
| [CONTACTS.md](CONTACTS.md) | **Phase 1 is built** (own names, tags and notes, the one name rule, `/contacts` routes, the Contacts tab, `/name` and `/tag`); phases 2–3 below are still plans. wame's own contact list (`wame_contacts`: name, tags, notes, language, time zone) and one display-name function for every view; a per-person profile (`GET /contacts/{number}`) with a cached WhatsApp lookup (admin `refresh=1` only) and wame's own stats; a Contacts tab in `wame admin`; `to=tag:` and `{name}` templates in the queue, plus `-dedupe` and `-skip-sent` |
