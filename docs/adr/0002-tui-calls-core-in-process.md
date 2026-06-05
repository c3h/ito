# The TUI calls the core in-process (refines "single writer")

**Status:** accepted · 2026-06-02 · refines decisions #4 and #9 and ADR-0001 (single writer).

## Context

The v2 TUI (a Bubble Tea board) sits "on top of the same core" (SPEC §7). But at the time of this decision there is no separable core: the whole of v1 lives in `package main`, with the data-access functions unexported and tangled with CLI concerns (printing, `--json`, and exit codes — e.g. project resolution returns an exit code in the middle of its result).

So before any TUI code can exist, two things must be settled: **how the TUI reaches the data**, and **what "single writer" means once a second surface can mutate issues.**

SPEC §2.4 states the write model as "the CLI is the only writer" and "every create/edit goes through a command (`new`, `move`, `edit`, …)." Read literally, that forbids a TUI from writing at all. The integrity guarantee it protects, however, comes from SQLite (single-process-or-not, writes are serialized by the write lock; WAL lets a long-lived reader coexist with an external writer), not from the prose "via a command."

## Decision

The TUI **calls the core in-process** — it does not shell out to the `ito` binary and parse `--json`. The data layer is extracted into an importable package and shared by both consumers:

- `internal/store` — a `Store` struct encapsulating the `*sql.DB`; exported `Issue`/`Project` types (carrying the §6.2 JSON shape); verb methods (`ListIssues`, `FindIssue`, `Move`, `Edit`, …), project resolution, and migration. It speaks **typed Go errors** (`ErrNotRegistered`, `ErrDetached`, `ErrNotFound`), never exit codes.
- `internal/tui` — the Bubble Tea program, holding a `*store.Store`. It knows no SQL and no exit codes.
- `package main` — flag parsing, command dispatch, human/`--json` output, the `error → exit code + actionable sentence` mapping (SPEC §6.1), and the bare-`ito` launch path into `tui.Run`.

"**Single writer**" is restated: **the `ito` binary is the only writer; the CLI and the TUI share the same `store` layer.** There are now two write *entry points* in one binary, but a single write *implementation*. Integrity is upheld by SQLite serialization, not by routing through argv.

## Considered Options

- **Shell out to the CLI (TUI runs `ito move`, `ito list --json`, …).** Rejected: a binary shelling out to itself is awkward; it forces every TUI action to be expressible as flags and re-serializes data already held in memory. Its only real draw — keeping the literal "every edit goes through a command" — protects prose, not integrity.
- **In-process, but keep free functions taking `*sql.DB` (no `Store` struct).** Rejected as the API shape: a long-lived TUI wants one owned connection and verb methods; the struct is the natural "one core, two consumers" surface.
- **In-process via a `Store` struct (chosen).**

## Consequences

- **Gains:** one definition of the domain types and the write logic; the TUI and CLI cannot drift. The exit-code leak in project resolution dissolves — the store returns typed errors and each consumer presents failure its own way (CLI → exit code + sentence; TUI → toast).
- **Error mapping is many-to-one, and `ErrDetached` carries data.** Today `resolveProject` returns both "no Project for this directory" and "a Project exists but is detached from it" as the same exit `4` with *different* actionable sentences (the detached one names the Project, for `ito init --reattach <name>`). So `ErrDetached` is a **structured** error carrying that name — not a bare sentinel — and the actionable wording is built at the CLI boundary, not inside the store. The store→exit-code mapping is many-to-one (`ErrNotRegistered` and `ErrDetached` both → `4`); this is the spot the extraction most easily breaks.
- **Costs:** the v1 refactor (extract `internal/store`, export types, replace the exit-code return with typed errors) must precede TUI work.
- **Refines, not reverts:** SPEC §2.4's intent (no double writing, no desync) stands. What changes is the literal claim "every edit goes through a command" — superseded by "every edit goes through the shared `store`, in a serialized SQLite transaction."
