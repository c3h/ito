# Optional cloud backend via Turso, one active backend at a time

**Status:** partially superseded by [decision 0005](./0005-local-first-store-with-change-log-sync.md) · 2026-07-29 · refines decision 0001 (the store stays a single source of truth; only its location becomes configurable). The per-statement remote backend and the "never synced" rule are retired by 0005; the Turso connection, config file and token handling survive as the sync transport.

## Context

All ito usage happens through agents that already require internet access, so the offline advantage of a purely local database is not load-bearing. What is becoming load-bearing is the opposite scenario: agents running in the cloud need to reach the same store as agents running on the author's machine. A local `~/.ito/ito.db` file cannot serve that.

The store architecture is already one step removed from the file: every query goes through a `*sql.DB` handle, the dialect is plain SQLite with `?` placeholders, and the only place that knows where the database lives is `store.OpenDefault()`.

## Decision

ito gains an **optional cloud backend** on [Turso](https://turso.tech) (libSQL), chosen because libSQL is a SQLite fork: the entire query layer, schema, and migrations work unchanged on both backends. Exactly **one backend is active at a time** — local *or* cloud, never both, never synced.

- A config file at `~/.ito/config.json` (honoring `ITO_HOME`) records the active backend and, in cloud mode, the database URL and auth token. A missing file means `local`, preserving today's behavior. The token lives **only** in this file (written with `0600` permissions) — no environment-variable channel, since ito is an installed personal tool, not a packaged deployment.
- `store.OpenDefault()` dispatches on the config: local mode keeps the current file-DSN path untouched; cloud mode opens the remote database through the pure-Go libSQL driver. Local-only DSN pragmas (WAL, `busy_timeout`, `_txlock`) do not apply to the remote connection.
- Switching backends is a data operation, so it gets its own command pair: **`ito migrate cloud --url <url> --token <token>`** and **`ito migrate local`**. `ito config` remains read-only display (active backend, local path, remote URL — never the token).

The migration, symmetric in both directions because the dialect is shared:

1. Open source and destination; run the schema `Migrate()` on the destination.
2. Refuse a non-empty destination unless `--force` is given.
3. Copy tables in dependency order, in batches.
4. Validate by comparing per-table row counts; any mismatch aborts before the switch.
5. Only then write the new backend to the config. Flipping the config file is the last, atomic step — any earlier failure leaves the previous backend active and intact.

The local `ito.db` is **never deleted** after migrating to cloud; it stays in place as a natural backup (a later `ito migrate local` therefore needs `--force`, since its destination is non-empty).

## Considered Options

- **Neon (Postgres).** Rejected: a different SQL dialect would force a rewrite of the query layer for no benefit over libSQL's drop-in compatibility.
- **Sync between local and cloud (embedded replicas, Litestream).** Rejected for now: two writable locations reintroduce the desync/conflict class of bug that decision 0001 eliminated. Single-owner is simpler and honest.
- **A backend service in front of the database.** Unnecessary: the CLI can hold the connection itself; a middle tier would only pay off with third-party users or authorization rules, neither of which exists.

## Consequences

- **Gains:** cloud agents and local agents share one store; the switch is reversible; the query layer, schema, and tests stay backend-agnostic for free.
- **Loses (accepted with eyes open):** every command in cloud mode pays a network round-trip (plus Turso free-tier cold starts); the token on disk means any process with filesystem access can write to the remote store — acceptable for a personal tool.
- **Caveat:** interactive transactions over the libSQL HTTP driver are limited; the migration relies on batched inserts plus post-copy validation instead of one large transaction. Normal CLI usage (short transactions) is unaffected.
- Decision 0001 is refined, not reverted: still a single SQLite(-compatible) database, still the CLI as the only writer — only its location becomes configurable.
