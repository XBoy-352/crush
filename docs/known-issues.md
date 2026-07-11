# Known Issues

## Right sidebar flickers during fast model output / multi-file edits

**Status**: Open (root cause in upstream deps; crush-side mitigations possible)
**Affected areas**: `internal/ui/model/ui.go` (`View`), `internal/ui/model/sidebar.go`;
upstream `charm.land/bubbletea/v2@v2.0.8` and `github.com/charmbracelet/ultraviolet`

The sidebar (Modified Files / LSP / MCP sections) visibly jumps or ghosts for a
frame while the model streams output or edits several files in a burst. It is
terminal-dependent: reproduces readily on terminals without synchronized-output
support (e.g. Termux, Apple Terminal), disappears on kitty/ghostty/WezTerm/
Alacritty where DEC mode 2026 gets negotiated.

**Root cause**: ultraviolet's ncurses-style hardware-scroll optimization
(`terminal_renderer_hardscroll.go`, called from `terminal_renderer.go:1230-1235`,
enabled on all non-Windows platforms at bubbletea `cursed_renderer.go:606`).

1. Before cell-diffing, the renderer hashes every terminal row — the hash spans
   the **full row width** (chat + sidebar cells) and only cell content, not
   style (`terminal_renderer_hashmap.go:6-14`).
2. While the chat pane auto-scrolls during streaming, rows whose sidebar column
   is blank match exactly under the shift and seed a scroll hunk
   (`hashmap.go:89-94`).
3. `growHunks`/`costEffective` (`hashmap.go:200-223, 235-276`) then extend the
   hunk into rows that only *approximately* match: a row whose chat portion
   moved but whose 30-col sidebar portion did not still wins the cost
   comparison — so rows containing sidebar text get swept into the scroll.
4. `scrolln` (`terminal_renderer_hardscroll.go:71-117`) performs the scroll
   with full-width terminal ops (`DECSTBM` + `\n`/`SU`/`DL`/`IL`). The
   terminal physically drags the sidebar cells along; `transformLine`
   repaints them back in the same frame.
5. Without DEC 2026 synchronized output the terminal may display the
   intermediate state (scrolled but not yet repaired) → visible flicker, up
   to 60×/sec.

**Why 2026 is often inactive**: bubbletea only enables it when the capability
query returns exactly `ModeReset` (`tea.go:786-793`) — a `ModeSet`/
`ModePermanentlySet` reply is ignored (upstream bug), the query is skipped
over SSH for unknown terminals (`tea.go:960-986`), and some terminals
(Termux) don't support it at all.

**Crush-side aggravators** (control trigger frequency, not the tearing itself):

- `View()` sets `tea.NewProgressBar(…, rand.Intn(100))` every frame while the
  agent is busy (`ui.go:~2698`), defeating the renderer's `viewEquals` no-op
  frame skip (`cursed_renderer.go:833-840`) → full flush path 60×/sec.
- Unbatched redraw flood during streaming: ~30 Hz debounced message deltas
  (`internal/message/message.go:16-21`), 20 Hz anim `StepMsg`
  (`internal/ui/anim/anim.go:22`), separate todo-spinner ticker; no
  coalescing anywhere in the pubsub→TUI path (`internal/app/app.go:552-575,
  636-666`).
- Every file edit publishes a history event (`internal/history/file.go:163`)
  whose handler re-diffs **all** session files from the DB
  (`internal/ui/model/session.go:112-159`) and reflows sidebar section
  heights (`sidebar.go` `getDynamicHeightLimits`) — so sidebar rows genuinely
  change while chat rows scroll. LSP diagnostics events are likewise
  unthrottled (`internal/lsp/handlers.go:104-123`).

**How to confirm**: run in a 2026-capable terminal (flicker vanishes);
`CRUSH_UI_DEBUG=true` to see repaint churn; or locally patch bubbletea to call
`SetScrollOptim(false)` — with hardware scroll off the sidebar physically
cannot move.

**Fix levers** (impact order):

1. Upstream ultraviolet/bubbletea: don't let scroll hunks sweep rows with a
   static vertical strip, or expose a public opt-out for scroll optimization.
2. Upstream bubbletea: accept `ModeSet`/`ModePermanentlySet` when enabling
   2026.
3. Crush mitigations: constant (non-random) progress-bar value so idle frames
   hit the `viewEquals` skip; debounce LSP-diagnostics and file-history
   events; cache `loadSessionFiles` diffs; gate anim ticks from forcing full
   redraws. These shrink exposure but can't fully eliminate tearing on
   non-2026 terminals.

## `/rename_session` ignored when sessions dialog is already open

**Status**: Open
**Affected file**: `internal/ui/model/ui.go` — `openSessionsDialogWithMode`

When the sessions dialog is already open (e.g. via `Ctrl+S`), running
`/rename_session` brings it to front but silently ignores `startInRenameMode`.
The dialog stays in normal mode instead of switching to rename mode.

**Root cause**: `openSessionsDialogWithMode` short-circuits with
`BringToFront` when the dialog already exists, without checking the
`startInRenameMode` flag.

**Suggested fix**: Close and reopen the dialog in rename mode when
`startInRenameMode` is `true` and the dialog is already open.
