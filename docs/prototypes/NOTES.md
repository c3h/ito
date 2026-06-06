# v2 TUI prototype — verdict

**Question it answered:** what should the v2 terminal board look and feel like, before building `internal/tui`? (Bubble Tea / Lip Gloss over the same core, in-process — see `SPEC.md §7` and `docs/adr/0002`.)

Artifact: `v2-tui.prototype.html` — a throwaway ASCII mock. Delete it once the decisions below land in `internal/tui`.

## Decisions

- **Surfaces:** two primary views plus an issue detail. **Digest** (rows grouped by status, sliding window per section) and **Board** (the five status columns). Switched with number keys `[1]`/`[2]`, shown as header tabs. The numbers stay bracketed with a constant colour; only the active view *name* (`digest`/`board`) changes colour — switching never reflows the header.

- **Issue detail:** read-only (fields + body + links). Its header uses the same shape as the view headers — `ito · ITO-14 · <title>`, `…`-truncated on overflow — and the frame spans the full width like the other views. The body prose keeps a narrower, readable measure inside that frame (not edge-to-edge).

- **Top-right = project name, not the cwd path.** A real working directory would overflow the header. All three views show the project name there (the count sits beside it on Digest/Board).

- **Shortcuts:** `s` status, `h` hide, `/` filter, `:` command are the always-visible hot keys. `p` priority and `l` labels appear only in the issue view and the `:` palette — they are rarer edits and don't earn a permanent slot. Not every action needs a key; the long tail lives only in `:`.

- **Inline command line.** `/` (filter) and `:` (command) do **not** open a separate screen. The view stays put; the bottom shortcut bar becomes a text input. `/` narrows the issues in place as you type; `:` filters the action list, rendered as a divider-to-edge list (like a section header), not a boxed block. `esc` leaves the field and the shortcuts return.

- **`h hide` generalises the old `d done`.** In the Digest, `h` hides/reveals whichever section is focused; `done` starts hidden. A collapsed section shows `▸ NAME (n) · h to show`, which you Tab to and reveal. The **Board always shows all five columns** — no hide there, because revealing a hidden board column would need a config/reveal surface we're not building now. *Deferred to v3:* persisting which sections are hidden, and hiding columns on the Board.

- **Viewport sizing is responsive, not a fixed cap.** The Board and Digest size the number of visible items to the terminal height, via Bubble Tea's `tea.WindowSizeMsg` (recomputed on every resize). The sliding window (`↑ N more` / `↓ N more`) is the *overflow fallback* for when content doesn't fit — not a hard limit. The prototype's fixed window of 5 is a mock artifact (an HTML page can't measure a terminal); it only exists to demonstrate the small-screen behaviour.

## Follow-ups before deleting this prototype

- `SPEC.md §7` has been rewritten to absorb every decision above (two surfaces over one state with the Digest primary; `h` generalising `done`; responsive sizing; the `/` filter and the closed `:` command line; the Digest-first build order with the Board's escape hatch to v3). The decision-log row #13 was updated to match. `CONTEXT.md` was left untouched on purpose — the surfaces render Issues (not "cards") and "column"/"row"/"section" stay UI vocabulary, so the glossary's `_Avoid_` lists hold without an exception.
- Fold the above into `internal/tui`, then delete `v2-tui.prototype.html` and this file.
