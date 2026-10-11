# TUI

```sh
./wabot tui              # terminal only
./wabot tui -addr :8080  # also serve the HTTP API from the same process
```

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Bubbles](https://github.com/charmbracelet/bubbles) and
[Lip Gloss](https://github.com/charmbracelet/lipgloss). Designed for a desktop-sized terminal
(at least ~100 columns).

If the session isn't logged in yet, the main pane shows a QR code. Scan it, or type
`/pair +5511999999999` to get a pairing code instead.

## Layout

```
 wabot · 5511988887777@s.whatsapp.net · connected
  All activity                  │Maria Silva 552187654321@s.whatsapp.net
  3 chats                       │Oct 10 19:46 Maria Silva: oi, tudo bem?
  👥 Family               22:16 │Oct 10 20:46 me: tudo e vc
  [image] foto da festa         │
▌ Maria Silva             20:46 │
  you: tudo e vc                │
  ~John                      2  │
  a very long message that sho… │
> _
```

- **Header**: your account and the connection state.
- **Chat list** (left): every conversation the bot has stored, most recent first, with the
  last message and its time. A green badge counts messages that arrived while you were looking
  at another chat. `▌` marks the chat that's open. `~Name` means the person isn't in your
  address book and that's their WhatsApp profile name.
- **Main pane** (right): the open chat's history (last 300 messages), updating live.
  **All activity** is a live feed of every chat plus command output.
- **Input** (bottom): plain text goes to the open chat; lines starting with `/` are commands.

The chat list comes from messages stored in `wabot.db`, so it fills up as you use the bot.
Right after a fresh `wabot login`, WhatsApp also sends a batch of recent conversations, which
are imported automatically.

## Keys

| Key | |
|---|---|
| `Tab` | move focus between the chat list and the input |
| `↑` `↓` / `k` `j` | (chat list focused) move the highlight |
| `Enter` | (chat list focused) open the highlighted chat |
| `Ctrl+N` / `Ctrl+P` | open the next / previous chat without leaving the input |
| `Esc` | go back to **All activity** |
| `↑` `↓`, `PgUp` `PgDn` | (input focused) scroll messages |
| `Ctrl+C` | quit |

## Commands

| Command | |
|---|---|
| `/contacts [search]` | list your contacts, optionally filtered by name or part of the number |
| `/to <number, JID or name>` | open a chat. A name works if exactly one contact matches; otherwise you get the matches |
| `/all` | go back to **All activity** |
| `/send <number or JID> <text>` | one-off message without switching chats |
| `/file <path> [caption]` | send a file to the open chat (images go as photos) |
| `/groups` | list your groups with their JIDs |
| `/pair <phone>` | pairing-code login |
| `/clear` | clear the current pane (the chat history is still in the database) |
| `/help` | keys and commands |
| `/quit` | exit |

## Notes

- Only one process can use a session at a time. Stop `wabot serve` before starting the TUI,
  or run `wabot tui -addr :8080` to get both.
- whatsmeow logs are silenced in the TUI so they don't mess up the screen. If something
  isn't working, run `wabot serve -debug` with the same `-db` to see them.
- File paths in `/file` can't contain spaces.
- WhatsApp sometimes identifies people by a hidden ID (`…@lid`) instead of their number,
  e.g. for messages you type on your phone. Until WhatsApp reveals which number that ID
  belongs to, the person can show up as a separate chat. The two merge automatically as soon
  as the link is known, usually with their next message.
