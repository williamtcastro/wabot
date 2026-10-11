# Proposed features

Ideas we may build later. **Nothing here exists yet**: these are plans, kept so the thinking isn't
lost and so anyone picking one up starts from the same notes. Each proposal says what it would do,
how it fits into the current code, what's missing, and the open questions. When one is built, its
user-facing parts move into the regular docs (`docs/*.md`) and the proposal is deleted or trimmed.

| Proposal | Summary |
|---|---|
| [ADMIN-PANEL.md](ADMIN-PANEL.md) | A small web admin panel served by `wabot serve`: status and QR login, chats, send form, audit log, webhook and rate-limit health. Also documents the logging and audit structure that exists today |
| [IMAGE-PREVIEWS.md](IMAGE-PREVIEWS.md) | Inline photo and sticker previews in the TUI using the Kitty graphics protocol (and fallbacks), instead of text labels |
