# ito

A local issue tracker for humans and AI agents, built for the terminal.

**ito** — 糸 (*the thread that links the issues*) · 意図 (*intention*).

![The ito TUI showing the digest, issue details, and batch waves](./docs/demo.gif)

`ito` keeps project issues in a central SQLite database and exposes them through a small, scriptable CLI. There is no server, account, API key, or file added to your repository.

## Install

```sh
go install github.com/c3h/ito@latest
```

`ito` is a single binary with a pure-Go SQLite driver (no CGO).

## Quick start

```sh
cd your-project
ito init
ito new --title "Ship the login flow"
ito list
ito move PROJ-1 in_progress
ito show PROJ-1
ito                       # open the TUI
```

`ito init` derives the project name and issue prefix from the directory. It stores data in `~/.ito/` and never writes inside the repository.

Run `ito --help` or `ito <command> --help` to explore the full CLI.

## Built for agent workflows

- Every command supports `--json` and runs without interactive prompts.
- Projects resolve from the Git root, so all worktrees share the same issues.
- `ito list --ready` finds logically ready issues based on blockers and conflicts.
- Batches group related work; derived Waves show which issues can run in parallel.
- Mutations are transactional, and failures return stable exit codes with actionable messages.

The AI stays external: `ito` embeds no model and exposes no MCP server. Any agent that can run shell commands can use it.

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
