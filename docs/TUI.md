# TUI

```sh
./wabot tui                                    # terminal only
./wabot tui -addr 127.0.0.1:8080               # also serve the HTTP API from the same process
WABOT_TOKEN=change-me ./wabot tui -addr :8080  # other addresses need a token
```

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Bubbles](https://github.com/charmbracelet/bubbles) and
[Lip Gloss](https://github.com/charmbracelet/lipgloss). Designed for a desktop-sized terminal
(at least ~100 columns).

If the session isn't logged in yet, the main pane shows a QR code. Scan it, or type
`/pair +5511999999999` to get a pairing code instead.

## Layout

```
╭────────────────────────────────╮╭─────────────────────────────────────────────────────────╮
│ Chats  3                       ││ Maria   +5511999999999                                  │
│                                ││ ─────────────────────────────────────────────────────── │
│   All activity                 ││ ──────────────────── Yesterday ────────────────────     │
│   1 chat with new messages     ││                                                         │
│ ▌ Maria                  14:05 ││   oi, tudo bem?  21:40                                  │
│   bem também!                  ││                                                         │
│   👥 Family                  2 ││                                 tudo, e você?  21:41 ✓  │
│   📷 foto da festa             ││                                                         │
│   ~John              Yesterday ││ ────────────────────── Today ──────────────────────     │
│   ok, até amanhã               ││                                                         │
│                                ││   ┃ you: tudo, e você?                                  │
│                                ││   bem também!  │ 👍  14:05                              │
│                                ││                                                         │
│                                ││                    📄 notes.pdf · 84 KB  edited 14:06 ✓ │
╰────────────────────────────────╯╰─────────────────────────────────────────────────────────╯
╭───────────────────────────────────────────────────────────────────────────────────────────╮
│ › _                                                                                       │
╰───────────────────────────────────────────────────────────────────────────────────────────╯
 tab chats  ·  ↑ select msg  ·  ^J/^K switch  ·  ^D/^U scroll  ·  /help co… ● +5511988887777
```

The focused panel has a green border.

- **Chat list** (left): every conversation the bot has stored, most recent first, with the
  last message and a relative time (`14:05`, `Yesterday`, `Mon`, `02/10/26`). A green badge
  counts messages that arrived while you were looking at another chat; badges reset when you
  restart. `▌` marks the open chat, `👥` a group, and `~Name` means the person isn't in your
  address book and that's their WhatsApp profile name. **All activity** at the top is a live
  feed of every chat. Group names are refreshed from WhatsApp every 10 minutes.
- **Conversation** (right): the open chat's last 400 messages, updating live, with day
  separators. Only that chat's messages are shown, not the same person's messages in groups.
  Your messages are right-aligned and green, others on the left. In groups each sender's name
  is shown in its own color. Replies show the quoted message above them (`┃`), or
  `┃ ↩ reply to an earlier message` when the quoted message isn't among the loaded ones.
  Reactions appear after the message, set off by a `│` divider, before the time
  (`bora!  │ ❤️ 👍2  21:41 ✓`); removed reactions disappear along with the divider. Edited or
  deleted messages are marked. Media shows as a label: `📷 Photo`, `🎥 Video`, `🎤 Voice message`, `💟 Sticker`,
  `📄 file name · size`, `📍 location`.
- **Context bar** (above the input, only when needed): `↩ Replying to Maria …`, `✎ Editing …`, or
  the command hint bar while you type a command (see [Autocomplete](#autocomplete)).
- **Input**: plain text goes to the open chat; lines starting with `/` are commands.
- Text from other people (messages, names, file names) is shown with terminal escape codes,
  control characters and bidi overrides removed, so a message can't redraw your screen or
  disguise its text.
- **Footer**: key hints for the focused panel, or the last notice (`✓ saved …`, `✗ error`)
  for 5 seconds. On the right, a dot shows the connection: green with your number when
  connected and logged in, red with the state otherwise.

The chat list comes from messages stored in the database (`~/.config/wabot/wabot.db` by default), so it fills up as you use the bot.
Right after a fresh `wabot login`, WhatsApp also sends a batch of recent conversations, which
are imported automatically.

**Read receipts.** Opening a chat sends read receipts for its latest incoming messages (up to
20), and new messages that arrive in the open chat are marked as read right away. Your contacts
see blue ticks as if you'd read them on the phone.

## Keys

This is the `/help` panel, word for word:

```
Moving around
  Tab / Shift+Tab     cycle focus: input → chats → messages
  Ctrl+J / Ctrl+K     next / previous chat (from anywhere)
  ↑ (empty input)     select messages
  Ctrl+D / Ctrl+U     scroll half a page down / up
  PgUp / PgDn         scroll
  Esc                 cancel / close / back to the input

Chat list (focused)
  type                filter chats by name
  ↑ ↓  Enter          pick and open

Selected message
  j k  ↑ ↓           move               g  G   first / last
  r  reply           e  edit (yours, 20 min)
  d d  delete        s  save media to ~/Downloads
  1-6  react 👍 ❤️ 😂 😮 😢 🙏     0  remove reaction
  other letters      back to typing

Commands
  /to <name|number|jid>     open a chat
  /all                      live feed of every chat
  /contacts [search]        your address book
  /groups                   your groups
  /send <number|jid> <msg>  one-off message
  /file <path> [caption]    send a file to the open chat
  /read                     mark the open chat as read
  /pair <phone>             log in with a pairing code
  /help  /quit

Typing a command, Tab completes it (and names after /to, numbers after /send,
paths after /file), ↑ ↓ or Ctrl+J / Ctrl+K cycle the matches.
```

Details:

- **Focus.** `Tab` cycles input → chats → messages, `Shift+Tab` goes backwards. The messages
  panel is skipped when no chat is open or it has no messages.
- **Chat list.** Typing filters by name (or part of the JID); `Backspace` edits the filter, the
  first `Esc` clears it and the second goes back to the input. `↑` `↓` move the highlight and
  `Enter` opens the highlighted chat. Letters always go to the filter, so `j` / `k` don't move here.
- **Switching chats.** `Ctrl+J` / `Ctrl+K` open the chat after / before the open one from
  anywhere, without changing focus. When the chat list is focused they step from the highlighted
  chat instead (and move the highlight with it). Going up past the first chat opens **All
  activity**. While you're typing a command with completions, they cycle the completions instead
  (see [Autocomplete](#autocomplete)). Many terminals send `Ctrl+J` as a line feed; wabot tells it
  apart from `Enter`, so `Enter` still sends.
- **Scrolling.** `Ctrl+D` / `Ctrl+U` scroll the conversation (or an open panel) half a page down /
  up whatever is focused; `PgDn` / `PgUp` scroll a full page. They don't edit the input.
- **Selecting messages.** `↑` in an empty input selects the newest message. Move with `j` / `k`
  (or `↓` `↑`); `g` jumps to the first loaded message and `G` to the newest. `j` or `↓` past the
  newest message returns to the input. `Enter` also starts a reply.
  Any other letter jumps back to the input and is typed there, so you can just start writing.
- **Reply / edit.** `r` or `e` puts you back in the input with a context bar. For an edit the
  input is pre-filled with the current text. `Enter` sends, `Esc` cancels. Deleted messages
  can't be replied to. Only your own text messages can be edited, and only within 20 minutes of
  sending (WhatsApp's limit).
- **Delete.** Press `d` twice on the same message. It's deleted for everyone.
- **React.** `1`–`6` react with 👍 ❤️ 😂 😮 😢 🙏, `0` removes your reaction.
- **Save.** `s` downloads the photo, video, voice message, sticker or document to
  `~/Downloads` (or the current directory if that doesn't exist), readable only by you (mode
  0600). Existing files are never overwritten: the copy is saved as `name (1).ext`,
  `name (2).ext` and so on.
- **Panels.** `/help`, `/contacts` and `/groups` open in the right-hand pane. `↑` `↓` and
  `PgUp` `PgDn` scroll them, `Esc` closes them.
- `Ctrl+C` quits, as do `/quit` and `/q`.

## Autocomplete

While the input starts with `/`:

- A **hint bar** above the input lists the commands matching what you've typed so far, with
  their arguments and a short description (`/to <name|number>  open a chat · …`). An unknown
  command shows `no such command`. The bar goes away once you type a space after the command.
- The best match appears as grey ghost text after the cursor. **`Tab`** accepts it; `↑` / `↓`
  or `Ctrl+J` / `Ctrl+K` cycle through the other matches. If nothing matches, `Tab` does nothing (it doesn't move
  focus).
- Completion knows the arguments too:
  - `/to `: names of your chats and address-book contacts, the number for chats without a
    contact name, and group JIDs.
  - `/send `: numbers and group JIDs of your chats.
  - `/file `: files and folders on disk, relative to the current directory or starting with
    `~/`. Hidden files only show up once you type a `.`; names containing spaces are skipped
    (`/file` can't take them anyway).

Outside a command, `Tab` keeps its normal job of cycling focus.

Everything you send from the TUI goes through the same rate limiter as the API and is recorded
in the audit log with actor `tui` (including `/pair`).

## Commands

| Command | |
|---|---|
| `/to <number, JID or name>` | open a chat. A name works if exactly one contact matches; otherwise you get the matches |
| `/all` | go back to **All activity** |
| `/contacts [search]` | list your contacts, optionally filtered by name or part of the number |
| `/groups` | list your groups with their JIDs |
| `/send <number or JID> <text>` | one-off message without switching chats |
| `/file <path> [caption]` | send a file to the open chat (images go as photos, the rest as documents). `~/` expands to your home directory |
| `/read` | mark the latest 50 incoming messages in the open chat as read |
| `/pair <phone>` | pairing-code login |
| `/help` | keys and commands |
| `/quit`, `/q` | exit |

## Notes

- Only one process can use a session at a time. Stop `wabot serve` before starting the TUI,
  or run `wabot tui -addr 127.0.0.1:8080` to get both.
- Logs are silenced in the TUI so they don't mess up the screen. Pass `-log-file wabot.log` to
  keep the app logs (HTTP requests, actions, webhook deliveries). whatsmeow's own logs stay off;
  to see them, run `wabot serve -debug` with the same `-db` instead.
- File paths in `/file` can't contain spaces.
- New commands go in `tuiCommands`, `tuiHelp` (in `internal/tui/commands.go`) and this file, so autocomplete,
  the hint bar and `/help` stay in sync.
- WhatsApp sometimes identifies people by a hidden ID (`…@lid`) instead of their number,
  e.g. for messages you type on your phone. Until WhatsApp reveals which number that ID
  belongs to, the person can show up as a separate chat. The two merge automatically as soon
  as the link is known, usually with their next message.
