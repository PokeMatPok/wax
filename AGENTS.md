# AGENTS.md — Wax (ytm-client)

Instructions and hard-won environment facts for AI agents working in this
repo. Windows is the primary platform; keep macOS/Linux compiling (build-tag
twins for every `*_windows.go` file).

## Commands

- Build (GUI binary, no console window): `build.cmd` builds BOTH
  `ytm-client.exe` (ldflags `-s -w -H=windowsgui`) and the SMTC helper
  `wax-smtc.exe` (`go build -ldflags "-s -w" -o wax-smtc.exe ./cmd/smtc-helper`).
  The helper must sit next to the main executable at runtime.
- Vet: `go vet -unsafeptr=false ./...` — the `-unsafeptr=false` is REQUIRED
  for `cmd/smtc-helper` (its uintptr->unsafe.Pointer COM calls are the
  standard Go-on-Windows COM pattern and trip the unsafeptr check by design).
- Tests: `go test ./...` — `internal/ytm`, `internal/extract`,
  `internal/player` and root e2e tests are LIVE (network, bin/mpv needed);
  they skip when dependencies are missing. Allow ~2.5 s for player tests.
- Deploy while the app runs (user launches it via shortcuts):
  `taskkill //IM Wax.exe //F` then copy BOTH `ytm-client.exe` and
  `wax-smtc.exe` into `$LOCALAPPDATA/Programs/Wax/`
  then `cd "$LOCALAPPDATA/Programs/Wax" && cmd //c "start Wax.exe"`.
- Child processes (yt-dlp, mpv, taskkill, tray) must never flash a console:
  every `exec.Command` needs `hideChildConsole(cmd)` (SysProcAttr HideWindow
  + CREATE_NO_WINDOW). Twins: `hidewin_windows.go` / `hidewin_other.go`.
  Always run yt-dlp/taskkill etc. through that helper, never bare.

## Architecture

- `main.go` holds the whole Gio app (~2,200 lines): tabs, search, player,
  queue deck, settings, keyboard shortcuts, vinyl overlay wiring.
  Deliberately not yet split; keep new tab code in methods on `appUI`.
- `theme.go`: colors, text sizes, icons, easing curves. Keep the design
  coherent/minimal; new colors belong here, not inline.
- UI talks only to `internal/extract.Extractor` and `internal/player.Engine`
  interfaces. Backends are swappable; don't leak mpv/yt-dlp types into UI
  code (note: the resolved stream type is `*extract.Stream`).
- State: `internal/prefs` (settings.json, includes persisted queue),
  `internal/history` (listen history — recorded from `playingTrack`, not
  the optimistic `nowTrack`, in `%APPDATA%\ytm-client\`).
- Config dir stays `%APPDATA%\ytm-client` even though the product is named
  Wax; module name is still `ytm-client`. Don't rename casually — it
  migrates user data paths.

## Gio v0.10.3 facts (API differs from older examples online)

- `layout.Context` event source: `gtx.Event(filters...)` both registers and
  polls; one shared `gtx.Event(...)` call with all `key.Filter`s, nil-tags
  for window-wide focus.
- Focus a widget: `gtx.Execute(key.FocusCmd{Tag: &editor})`.
- `ModShortcut` = Ctrl on Windows.
- External goroutines changing state MUST trigger a redraw (invalidate);
  gio only repaints on events. Lyrics/timers from other threads follow this.
- Keys typed into the editor are suppressed for global handlers by checking
  `gtx.Focused(&ui.editor)` first (`handleShortcuts`).

## Windows facts that cost debugging time

- `os.ReadDir(`\\.\pipe\`)` FAILS on this machine (as does syscall
  FindFirstFile on the pipe namespace). mpv orphan cleanup intentionally
  uses a PID file (`%APPDATA%\ytm-client\mpv.pid`), not pipe enumeration.
  PID file kills check the image name is `mpv.exe` first (PID reuse safety).
- mpv lifetime: `bindToLifetime` (kill-on-close job object) in
  `lifetime_windows.go`; `SetInformationJobObject` returns two values
  (`(_, err)`) — don't "fix" it back to one.
- Transparent always-on-top overlays: `internal/vinylwin` (record) and
  `internal/lyricswin` (lyrics bubble) hand-roll win32 layered windows;
  per-pixel alpha needs premultiplied BGRA via UpdateLayeredWindow,
  `WS_EX_TOOLWINDOW` (no taskbar button), `WM_NCHITTEST → HTTRANSPARENT`
  outside the disc for click-through, tray icon via Shell_NotifyIcon.
  Geometry: record is pushed toward top/left beyond the 1/4 corner peek;
  hover state also shifts it (see user-approved offsets in main.go).
- SMTC (Windows media integration) runs OUT-OF-PROCESS by necessity: raw
  WinRT COM interop inside the gio UI process silently killed the app
  (exit 0xB00, 0-40 s in, no panic / no WER event / nothing a VEH sees).
  Layout: `internal/smtc` is a thin client shim with the public API
  (`Start`, `SetTrack`, `SetPlayback`, ...); `cmd/smtc-helper` holds the
  COM core and speaks JSON lines over stdio
  (`{"c":"track"/"cover"/"play"/"pause"/"stop"/"timeline"/"enable"}` in,
  `{"e":"button","btn":N}` / `{"e":"ready"}` out). The helper exits when
  stdin closes (app death = no stray process/session); the shim respawns a
  dead helper (max 3 fast failures) and replays cached state. The helper
  claims AUMID `Wax`; `HKCU\Software\Classes\AppUserModelId\Wax`
  (DisplayName + IconUri) makes the flyout show the app name/icon — set
  during deploy. Linux/macOS port: MPRIS over godbus behind the same shim
  API (a D-Bus service instead of a helper process).
- Git Bash (agent shell) mangles backslash device paths and converts
  arguments: MSYS path conversion breaks `\\.\pipe\`, `dir \\.\pipe\`,
  single `/c`-style flags. Use `//` for cmd switches (`taskkill //IM ...`)
  and prefer Go probes or .cmd batch files over inline PowerShell quoting.
- Icon: `../icongen/main.go` regenerates `assets/icon.ico`;
  `rsrc_windows_amd64.syso` at repo root embeds it (rebuild after regen).
- `/c` etc. in cmd from this shell are risky; verify with `tasklist | grep -i wax`.

## Conventions

- Minimal diffs; match existing style; build-tag twins
  (`*_windows.go` / `*_other.go`) for anything OS-specific; no cgo, ever.
- Do not add console logging paths for the GUI app; log to
  `%APPDATA%\ytm-client\vinylwin.log` (1 MB cap) from overlay code.
- Live tests hit the network; keep them skippy rather than failing.
- Do not kill processes blindly: the user may be listening. Check parentage
  (e.g. `Get-CimInstance Win32_Process` CommandLine shows pipe = owner PID)
  before killing stray mpv instances.
- The user values reversibility: note how to revert significant UI geometry
  changes; pre-change backups live in `~/.vibe/scratch/*.deck-baseline`.
