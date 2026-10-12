# Image previews in the TUI (proposal)

> **Deferred proposal.** Not built. Today the TUI shows media as text labels (`📷 Photo`,
> `💟 Sticker`) and `s` saves the file. See the [list of proposals](README.md).

## Goal

Show photos and stickers as small images inside the TUI, in terminals that can draw images, and
keep the current text labels everywhere else. No change for terminals without image support.

Non-goals: video or GIF playback, full-screen image viewer, image editing, previews in the HTTP API.

## Terminal support

| Protocol | Terminals |
|---|---|
| [Kitty graphics protocol](https://sw.kovidgoyal.net/kitty/graphics-protocol/) | Kitty, Ghostty, WezTerm (also Konsole, partially) |
| iTerm2 inline images (`OSC 1337`) | iTerm2, WezTerm |
| Sixel | xterm (with `-ti vt340`), foot, mlterm, Windows Terminal, recent tmux builds |
| none | Apple Terminal, most others: keep text labels |

Start with Kitty only. It's the best-specified protocol, supports placing, moving and deleting
images by ID, and covers the terminals most likely to be used with this TUI. iTerm2 and Sixel can
come later behind the same interface.

## Detection

1. Config override first: a flag or env var (`WAME_IMAGES=kitty|iterm|sixel|off`).
2. Environment hints: `TERM=xterm-kitty`, `TERM_PROGRAM=ghostty|WezTerm|iTerm.app`, `KITTY_WINDOW_ID`.
3. Query the terminal: send a Kitty graphics query (`ESC _Gi=1,a=q;… ESC \`) followed by a
   device-attributes request, and read the replies with a short timeout before Bubble Tea takes
   over the terminal. A reply to the graphics query means support.
4. Inside tmux or screen, default to off unless forced: passthrough needs extra escaping and
   placement is unreliable.

## Where it hooks in

- `internal/tui/view.go`, `bodyOf`/`renderChat`: for a photo or sticker message, reserve a fixed
  block of cells (e.g. 20×8) instead of the one-line label, and remember its position.
- Two possible scopes, in order of effort:
  1. **Selected message only**: when a media message is selected (`focusMessages`), show its
     preview in a box at the bottom of the conversation pane. Only one image on screen at a time.
  2. **Inline**: previews for every visible photo/sticker in the conversation.
- Scope 1 first. It avoids most of the scrolling problems below.

## Getting the image

- `bot.DownloadMedia(ctx, seq)` already fetches and decrypts, with the 100 MB cap. Only download
  for `image` and `sticker` types, and only when the declared size is under a much lower preview
  cap (e.g. 5 MB).
- Run it in a `tea.Cmd`, never in `Update`. Show the text label until the image arrives.
- WhatsApp messages carry a small JPEG thumbnail (`JPEGThumbnail`) in the protobuf (`raw`). Use it
  as an instant low-resolution preview without any download, and fetch the full image only on
  demand.
- Stickers are WebP (sometimes animated). Decode with `golang.org/x/image/webp` (first frame only).

## Caching

- In memory: an LRU of decoded, already-scaled images keyed by `seq` and target cell size
  (e.g. 50 entries).
- On disk (optional): scaled PNGs under `~/.cache/wame/previews/<seq>-<w>x<h>.png`, mode 0600,
  with a size cap and pruning. These are private images; never store them inside the repo and
  delete them with `wame logout`.
- Kitty can keep transmitted images by ID; transmit once (`a=t,i=<id>`), then place/delete
  (`a=p` / `a=d`) as the view changes, which is cheaper than re-sending.

## Bubble Tea challenges

- Bubble Tea and Lip Gloss think in cells and strings. Images are drawn by escape sequences that
  the renderer doesn't understand: it can't measure them, and its diff-based redraw may overwrite
  or skip them.
- Images must be placed after the frame is drawn, at absolute positions (cursor move + Kitty
  placement), and deleted before the next frame if the layout moved: on scroll, resize, focus
  change, opening another chat, panels, search results.
- Kitty's Unicode placeholders (`U+10EEEE` cells tied to an image ID) let the image live inside
  the normal text grid, so Bubble Tea's redraws move it naturally. This is the most promising way
  to stay compatible with the renderer; check that Lip Gloss width calculation handles the
  placeholder characters.
- Alt-screen, viewport clipping and the rounded borders must not cut an image in half: only show
  images that fit entirely in the visible region.

## Effort and risks

- Scope 1 with Kitty and thumbnails: a few days, mostly placement and clearing.
- Inline previews: significantly more, mostly in redraw bugs.
- Risks: flicker or ghost images left on screen; breaking the 24-line layout tests; a terminal that
  claims support but renders badly; decoding untrusted images (keep to the standard library and
  `x/image` decoders, cap pixel dimensions before decoding).
- Tests: render to a buffer and assert the escape sequences and placeholder cells; keep the
  text-label path as the default in tests.

## Open questions

- Is the embedded JPEG thumbnail good enough for most uses, so full downloads are never automatic?
- Selected-message preview only, or also inline? Is a toggle key (e.g. `p`) worth it?
- Should previews be off by default and enabled with a flag, at least at first?
- Support iTerm2 and Sixel, or only Kitty-compatible terminals?
- Does the disk cache belong in wame at all, given the privacy cost?
