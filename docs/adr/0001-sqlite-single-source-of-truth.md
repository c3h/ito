# Central SQLite as the single source of truth (reverts markdown-per-project)

**Status:** accepted · 2026-05-24 · reverts original decisions #3, #4, #5 and #9.

## Context

The original design stored each issue as a `.md` file with YAML frontmatter (the source of truth), inside a per-project, gitignored `.ito/`. This created three problems that only surfaced once the real usage flow was detailed:

1. **Worktrees can't see the issues.** Since `.ito/` is gitignored, it isn't carried into a new `git worktree` — and it sits in a *sibling* directory, not an *ancestor* one. The flow "I plan in the main worktree, I execute in parallel across worktrees" (the author's core use) broke: the agent in the worktree ran `ito list` and saw nothing.
2. **Double writing = a class of bugs.** CLI + Obsidian + agent all writing the same files forced workarounds (recency via `mtime` instead of a field, `last_id` reconciliation, `validate` to detect file↔frontmatter drift).
3. **Hand-rolled integrity.** `flock` + atomic writes + a counter in YAML is fragile next to ready-made transactional guarantees.

## Decision

The source of truth becomes a **single, central SQLite database** in `~/.ito/`. The **CLI is the only writer** (single writer). Project identity is resolved by the **git repository root** (all worktrees share the same `.git` → they land in the same project); outside git, it matches the cwd against the registered roots. The CLI **never writes inside the repository** — only in `~/.ito/`.

## Considered Options

- **Per-project markdown (original).** Rejected: the worktree problem + double writing.
- **Central markdown, keyed by project.** Would solve the worktree issue, but keeps the hand-rolled integrity fragility and the YAML schema pollution as it evolves.
- **Markdown as truth + SQLite as a rebuildable cache.** Cheaper, but preserves double writing and "export" becomes a dead snapshot.
- **NoSQL (document/KV).** Rejected: an issue tracker is relational (filters, `parent` tree, `blocked_by` graph) — joins are SQLite's strength.

## Consequences

- **Gains:** ACID integrity battle-tested for 20+ years; single writer eliminates the desync class of bug; access from any cwd; schema with migrations (clean evolution); worktrees share the issues for free.
- **Loses (accepted with eyes open):** editing in **Obsidian** is gone; the agent **no longer edits the file directly** with Write/Edit — everything goes through the CLI (which reinforces decision #1, "the agent drives via bash"). The issue body becomes a TEXT column (markdown preserved inside it).
- **Markdown export** remains a future, opt-in feature (snapshot).
- **Simplifies:** recency (`updated`) goes back to being a reliable column; the ID counter becomes transactional (no reconciliation); broken links can be blocked by FK on write; the footprint invariant becomes trivial (the CLI only touches `~/.ito/`, never the repo).
