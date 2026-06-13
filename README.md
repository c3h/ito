# ito

A local, solo issue tracker for the terminal — built to be driven by an AI agent through the command line.

**ito** — 糸 (*the thread that links the issues*) · 意図 (*intention*). The thread of intentions.

![The ito TUI: digest, issue detail and batches with derived waves](./docs/demo.gif)

## Why

Projects are born from the conversation with an AI, on the local machine — but tracking them still requires ceremony: configuring a SaaS project before you can file the first task breaks the flow. `ito` gives you traceability with zero ceremony: the agent plans the work, files the issues through the CLI, and you watch the board in the terminal. No server, no account, no API key.

## Install

```sh
go install github.com/c3h/ito@latest
```

A single static binary: pure-Go SQLite (no CGO), instant cold start.

## Quick start

```sh
cd your-project
ito init                  # registers the project; name and prefix derive from the folder
ito new --title "Ship the login flow" --priority high --label feature
ito list                  # issues of the current project
ito move PROJ-1 in_progress
ito show PROJ-1
ito                       # bare ito opens the TUI
```

`ito init` never asks anything and never writes inside your repository — the store is a central SQLite database in `~/.ito/`.

## Agent-native by design

The AI is external: `ito` does not expose MCP and does not embed an LLM. The agent you already use drives the CLI through the shell, so the integration costs zero tokens until a command runs. The whole surface is built for that:

- `--json` on every command — raw data, no envelope.
- A small, stable exit-code taxonomy (`0` ok, `2` usage, `3` not found, `4` project not initialized).
- No interactive prompts anywhere, so an agent never stalls.
- Errors are actionable sentences on stderr (`no Project registered for the current directory. run 'ito init' in this Project or use --project <name>.`) — the message is what makes the agent take the right next action.
- `ito list --ready` computes the frontier of issues whose blockers are all done (and whose `conflicts_with` partners are idle) — the set an agent can safely fan out, one git worktree per issue.
- `ito batch` groups a coherent effort as a named set of issues; `ito batch show` slices it into **Waves** — the link-graph generations safe to run in parallel, derived at read time so they never contradict the links.
- `--help` is the guide: the root help orients to the non-obvious model so a first run explains itself.

## How it works

- **Central store, zero repo footprint.** One SQLite database in `~/.ito/` (overridable with `ITO_HOME`). The CLI never writes inside the repository — there is not a single `ito` file to configure or ignore.
- **Project identity comes from the git root.** Every `git worktree` shares the same project; moving or renaming the repo never loses issues (`ito init --reattach <name>` re-points it).
- **The CLI is the only writer.** Every mutation is a transaction; IDs (`PROJ-12`) are minted from a monotonic per-project counter and never reused.
- **The TUI calls the core in-process.** Running bare `ito` opens the digest and batches surfaces over the same store — no daemon.

Issues are flat in v1: statuses `backlog → todo → in_progress → in_review → done`, priorities, a fixed label vocabulary, and typed links (`blocked_by`, `relates_to`, `conflicts_with`). A **Batch** groups issues planned together as one effort; its **Waves** are derived from the link graph at read time, never stored.

## Documentation

- [`SPEC.md`](./SPEC.md) — the full spec, with every architecture decision and its why.
- [`CONTEXT.md`](./CONTEXT.md) — the domain glossary.
- [`docs/adr/`](./docs/adr/) — architecture decision records.

## License

[MIT](./LICENSE)
