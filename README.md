# ito

A local issue tracker for humans and AI agents, built for the terminal.

**ito** — 糸 (*the thread that links the issues*) · 意図 (*intention*).

![The ito TUI showing the digest, issue details, and batch waves](./docs/demo.gif)

`ito` keeps project issues in a local SQLite database and exposes them through a small, scriptable CLI. There is no server, API key, or file added to your repository. Optionally, your machines can share one tracker by syncing through a Ledger you host.

## Install

```sh
go install github.com/c3h/ito@latest
```

`ito` is a single binary with a pure-Go SQLite driver (no CGO).

## Quick start

```sh
cd my-app
ito init
ito new --title "Ship the login flow"
ito list
ito move MYAPP-1 in_progress
ito show MYAPP-1
ito                       # open the TUI
```

`ito init` derives the project name and issue prefix from the directory (`my-app` → `MYAPP-1`). It stores data in `~/.ito/` and never writes inside the repository.

Run `ito --help` or `ito <command> --help` to explore the full CLI.

## Built for agent workflows

- Every command supports `--json` and runs without interactive prompts.
- Projects resolve from the Git root, so all worktrees share the same issues.
- `ito list --ready` finds logically ready issues based on blockers and conflicts.
- Batches group related work; derived Waves show which issues can run in parallel.
- Mutations are transactional, and failures return stable exit codes with actionable messages.

The AI stays external: `ito` embeds no model and exposes no MCP server. Any agent that can run shell commands can use it.

## Working from a second machine

Each machine (a Device) keeps its own complete store and works at local speed, offline. To share one tracker across machines, connect each Device to the same Ledger — an append-only change log hosted on a [Turso](https://turso.tech) database you own.

On the machine that already has issues:

```sh
ito ledger connect --url libsql://<db>.turso.io --token <token>
```

This creates the Ledger's tables and snapshots the local store into it. On the second machine:

```sh
ito ledger connect --url libsql://<db>.turso.io --token <token>   # pulls the whole tracker
ito list
```

Bare `ito` on a machine with no config offers the same connection once, interactively. From then on every command stays local; writing commands push their changes right after they commit, the TUI syncs in the background when it opens or refreshes, and `ito sync` pushes and pulls on demand. Only `ito new` needs the network, to reserve the next issue number from the Ledger so IDs stay sequential across machines. When both machines edited the same issue, the later edit wins. `ito config` shows which Ledger is connected; `ito ledger disconnect` forgets it and keeps every local row.

## Documentation

- [`SPEC.md`](./SPEC.md) — behavior and architecture
- [`CONTEXT.md`](./CONTEXT.md) — domain glossary
- [`docs/adr/`](./docs/adr/) — architecture decisions

## Development

```sh
go test ./...
go run . --help
```

## License

[MIT](./LICENSE)
