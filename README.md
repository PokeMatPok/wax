# Wax

Custom YouTube Music client for desktop (Windows first, macOS/Linux targets
kept compiling), written in Go with the Gio GUI toolkit — no cgo, no
Chromium, no Electron. The name refers to vinyl: the signature UI element is
a screen-corner record that spins while music plays.

## Current state

- Search (YouTube Music InnerTube, client `WEB_REMIX`, no API key)
- Playback via mpv (JSON IPC), transport controls, seek, volume, mute
- Five tabs, keyboard shortcuts (Space, arrows, Ctrl+1..5, Ctrl+F, ...)
- Queue with deck overlay (record-stack fan-out, click to jump), persisted
  across restarts
- Recommendations mixed from listen history + autoplay ("pick next")
- Synced lyrics (lrclib.net) in a glassmorphism bubble + on the vinyl
- Vinyl overlay: transparent, always-on-top, screen-corner record with the
  cover art; spins when playing; hover = scale-up + play/pause glyph +
  title/artist; hover on its left edge = queue deck; auto-hides after an
  idle timeout (configurable); click-through outside the record; tray icon
- Listen history (recorded on actual playback, respects "collect data"
  setting), cover art cache, settings page (clear data, collection toggle,
  record visibility timeout, keyboard reference)
- Optimistic UI: clicked track appears in the player immediately while
  extracting; rollback on failure; extraction retries with backoff
- OOBE-free install: Desktop + Start Menu shortcuts, embedded icon

## Build / run

```
build.cmd          # Windows GUI binary: -ldflags "-s -w -H=windowsgui"
go run .           # development (console window appears; fine for dev)
```

Deployed location: `%LOCALAPPDATA%\Programs\Wax\Wax.exe`.
Config + data: `%APPDATA%\ytm-client\` (settings.json, history.json,
queue in settings.json, vinylwin.log).

`bin/` (gitignored) holds `yt-dlp.exe` and `mpv.exe`; both are also found
on PATH. The repo was set up with zhongfly/mpv-winbuild.

## Architecture

```
main.go                  app UI: tabs, search, player, queue, settings (~2200 lines)
theme.go                 colors, typography, icons, easing
internal/ytm/            InnerTube search + radio/autoplay continuation
internal/extract/        Extractor interface; yt-dlp-backed stream resolver
internal/player/         Engine interface; mpv JSON-IPC engine, job-object
                         lifetime binding, orphan sweep (pid file), hidden
                         child consoles, named-pipe IPC (go-winio)
internal/lyrics/         lrclib.net client, LRC parsing + word timing
internal/lyricswin/      transparent lyrics bubble overlay (win32)
internal/vinylwin/       transparent vinyl overlay + tray icon (win32)
internal/cover/          cover-art fetch + in-memory cache
internal/history/        listen history store (JSON)
internal/prefs/          settings store (JSON) incl. persisted queue
icongen (../icongen)     generates assets/icon.ico; embedded via
                         rsrc_windows_amd64.syso at the repo root
```

The UI only talks to the small `Extractor` / `Engine` interfaces, so backends
can be swapped (pure-Go extractor, different engine) without touching UI code.

## How it talks to YouTube

- **Search / radio**: InnerTube `WEB_REMIX` web API (same family NewPipe and
  yt-dlp use). No key, no quota.
- **Stream resolution**: `yt-dlp -J --no-playlist -f bestaudio` on the watch
  URL; retried 3x with backoff on failure. If playback suddenly stops
  working, update yt-dlp first (`bin\yt-dlp.exe -U`).
- **Playback**: mpv spawned idle, controlled over JSON IPC (named pipe on
  Windows via go-winio, unix socket elsewhere). Observed properties
  (`time-pos`, `duration`, `pause`, `eof-reached`) drive the UI.
- **Lyrics**: lrclib.net (keyed/artist/title/duration match), plain text as
  fallback when no synced LRC exists.

## mpv lifecycle

Wax binds mpv into a kill-on-close job object and records the mpv PID to
`%APPDATA%\ytm-client\mpv.pid`. Taskkilling/dying Wax therefore cannot leave
mpv behind; on the next start any recorded PID that is still a live
`mpv.exe` is terminated (PID-reuse safe — the image name is checked before
killing). Note: Go cannot enumerate `\\.\pipe\` on Windows, so the sweep
deliberately does not use pipe listing.

## Keyboard shortcuts

| Keys | Action |
|---|---|
| Space | play / pause |
| Left / Right | seek -/+ 10 s |
| Up / Down | volume -/+ 5 |
| Ctrl+M | mute / unmute |
| Ctrl+Left / Ctrl+Right | restart or previous / next in queue |
| Ctrl+F | focus search |
| Ctrl+1..5 | tabs (search / queue / discover / history / settings) |
| Ctrl+Tab / Shift+Tab | next / previous tab |
| Esc | blur input |

## Tests

| package | what it proves |
|---|---|
| `internal/ytm` | live InnerTube search, parsing, radio continuation |
| `internal/extract` | yt-dlp resolves a real stream URL |
| `internal/player` | mpv IPC play/pause/volume/stop state machine |
| internal/lyrics, internal/history | LRCLIB fetch/parse, history add/clear |
| root (`e2e_test.go`) | resolve real stream, play >1 s through mpv |

Live tests skip gracefully when network/binaries are missing.
The old 7z helper binary is gone; only `mpv`, `mpv.com`, `yt-dlp` remain.

## Known limits / next ideas

- `main.go` is one large file; splitting the tab views out is planned.
- Recommendations are a simple mix of recent history + radio seeds.
- Vinyl overlay is Windows-only (`internal/vinylwin`, `lyricswin` have no
  non-Windows twins yet; macOS/Linux need a similar layered-window story).
- Some search-result shelf variants still show no album/duration.
- Playlist import/export, sleep timer, mini mode: not started.
