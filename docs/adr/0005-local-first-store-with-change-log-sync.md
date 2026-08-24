# Local-first store, synced between machines through a change log

**Status:** accepted · 2026-08-24 · supersedes the "one active backend, never synced" half of decision 0004; keeps its Turso connection as the sync transport.

## Context

Decision 0004 made the store's location configurable and accepted that cloud mode pays a network round-trip per statement. In practice that cost is what made cloud mode unusable: the Turso database lives in `aws-us-east-2`, the TCP round-trip from São Paulo measures ~390 ms, and a TUI refresh issues several statements, so every interaction takes seconds. Reducing the number of round-trips (done in the `perf(store)` series) hit its floor; the remaining cost is geography.

The real requirement behind cloud mode is sharper than "a store in the cloud": one person working from a Mac in Brazil and a VPS in the US, never on both at the same moment, sometimes on networks that block VPNs but not plain HTTPS. No centralized database satisfies that from both continents at local speed — and the alternatives investigated ([Cloudflare D1](../research/cloudflare-d1-remote-backend.md), Turso in a South American region, the new pure-Go Turso engine) each fail on a hard constraint: no interactive transactions, no region, or no FTS5.

## Decision

Every machine works against its own local SQLite — the query layer, schema, migrations and FTS5 run unchanged and at local speed. Machines share state by exchanging **Changes** through a **Ledger** hosted on the existing Turso database, in batches, during **Sync**. The cloud stops being a backend and becomes a transport; the per-statement remote backend of decision 0004 is retired rather than kept as a legacy mode.

- **Change log.** Every mutation in the store also appends a Change to a local log inside the same transaction. A Change carries the row's full new state (or a tombstone), the row's `updated` timestamp and the originating **Device**. Deletion commands still exist, so tombstones are part of the format from day one even though in normal use Issues are closed, never deleted.
- **Sync.** `push` sends local Changes not yet in the Ledger; `pull` fetches Ledger Changes not yet applied locally and applies them. Conflicts are resolved **per row by last-writer-wins on `updated`** — sufficient for a single person who never writes on two machines at the same moment, and cheap enough to reason about. The Ledger holds the complete history from a snapshot of the current data forward, so an empty machine bootstraps itself with a single `pull`.
- **Issue numbers are allocated by the Ledger.** `ito new` reserves the next number for the Project remotely (one round-trip per creation, ~0.4 s), so numbering stays globally sequential and an ID never changes after it is used in a branch name or a commit. The accepted cost: creating an Issue needs network; everything else (list, edit, move, search, batches) is local and works offline.
- **When it runs.** Manually via `ito sync`; automatically in the background when the TUI opens; and as a short, bounded push at the end of each writing command, where a network failure is a warning, never an error — the Change stays pending for the next Sync.
- **New machine.** Running `ito` with no configuration asks, interactively, whether to start empty or connect to an existing Ledger; non-interactive callers (agents) get an error naming the explicit command instead of a prompt.
- **Transport is a detail.** The Ledger sits behind a two-method interface (append, read after position). Turso wins now because its driver, config and token handling already exist; Cloudflare R2 or a git repository could replace it without touching the store.

## Considered Options

- **Cloudflare D1.** Rejected: auto-commit only (no `BEGIN`, ito uses `sql.Tx` in ~14 places), FTS5 databases cannot be exported and have bricked on export attempts, no South American primary, and no usable Go driver. Details in `docs/research/cloudflare-d1-remote-backend.md`.
- **Turso relocated to a Brazilian region.** Not available: Turso's regions are all outside South America. Even if it were, a US VPS would then pay the same latency in reverse.
- **Turso embedded replicas / the new pure-Go `tursogo` engine with native sync.** Rejected: the Go embedded-replica path needs CGO, and `tursogo` replaced FTS5 with a different search engine, forcing a search rewrite and two dialects between local and remote.
- **Per-device ID ranges (Mac 1–999, VPS 1000–1999) or renumbering on Sync.** Rejected in favour of Ledger allocation: ranges break the sequential numbering; renumbering changes an ID that branch names and commits already reference.
- **SSH into the VPS and run `ito` there.** Kept as the zero-effort workaround for a project that lives only on the VPS; it does not give a unified view from the Mac.

## Consequences

- The "two writable locations" objection in decision 0004 is answered, not ignored: it rejected sync without a merge rule. This decision defines the rule (row-level LWW on `updated`, tombstones, Ledger-allocated IDs), and on each machine the local SQLite remains the single source of truth of decision 0001.
- `Updated` becomes load-bearing beyond display: it is the conflict-resolution key, so every mutation must stamp it (already the case) and clocks on the machines must be roughly right.
- Latency of the remote becomes irrelevant to interactive use; the free-tier cold start is paid once per Sync, not per statement.
- `ito migrate cloud` / `ito migrate local` lose their meaning and are replaced by the Ledger connection commands; the schema-cache optimization for the remote backend goes with them.
