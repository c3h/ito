# ito

Local, solo, "full local" issue tracker for the terminal, driven by AI through the command line. This glossary fixes the domain language; decisions and trade-offs live in `SPEC.md` and in `docs/adr/`.

## Language

**Issue**: The trackable unit of work — a **row** in the central SQLite store. The markdown body lives in a column; it is not a file. In v1 it is **flat** (no sub-types or hierarchy); "task" is an informal synonym. It can have **Links**. _Avoid_: ticket, card, item, `.md` file.

**Project**: The scope that owns a set of Issues and a **Prefix**. It has **durable** identity (internal id + a unique, renameable `name`); the `name` is a lowercase slug and the external identifier in commands. The repo path is just a **mutable pointer** (`root_path`) to the canonical git root shared by the worktrees, not to the literal cwd. Resolved implicitly by the git repo root (worktrees share it → same Project); moving/renaming the repo does not lose Issues (you just re-point). _Avoid_: repo, folder, workspace.

**Link**: A typed, **flat** (non-hierarchical), intra-Project relation between Issues. `blocked_by` is directional: the source Issue is blocked by the target Issue. `relates_to` is symmetric: the relation exists between the two Issues with no dominant side. `conflicts_with` is symmetric: the two Issues must not be worked **in parallel** (mutual exclusion) even though neither blocks the other — it constrains scheduling, not order. It is what constitutes **Traceability** in v1. _Avoid_: dependency, reference, parent (hierarchy does not exist in v1).

**Batch**: A named set of Issues within a Project, planned together as one coherent effort (a feature, a refactor, a fix). Its identity follows the Project pattern: a unique lowercase slug `name` plus an immutable `created` timestamp; date is how listings order Batches chronologically, never the identity. An Issue belongs to **at most one** Batch (membership is optional); work shared across efforts is expressed through Links, never through double membership. A Batch scopes reading and views — it does not change how its Issues behave. Its completion is **derived**, never stored: a Batch is complete when every member Issue is in the _closed_ Category; adding open work reopens it by definition. _Avoid_: sprint, milestone, epic, group, cycle.

**Wave**: A **derived** slice of a Batch — never stored. Waves are the topological generations of the Batch's Issues under the Link graph: Wave 1 holds the Issues with no pending blockers; Wave 2, the ones unblocked once Wave 1 is `done`; and so on. `conflicts_with` pushes mutually exclusive Issues into different Waves. Issues in the same Wave are mutually independent — the set safe to fan out across git worktrees in parallel. Recomputed at read time, so it can never contradict the Links. _Avoid_: cycle (that's a dependency-graph error), sprint, phase, iteration.

**Status**: The stage of an Issue in the fixed flow: `backlog`, `todo`, `in_progress`, `in_review`, `done`. `backlog` holds work that is mapped or still subject to review; `todo` holds work selected for execution; `in_progress` holds the work that is active now. Changed via `ito move`, which validates only the target. _Avoid_: state, phase, column.

**Priority**: The relative urgency of an Issue: `low`, `medium`, `high`, `urgent`. It is independent of **Status**, **Label** and **Link**. _Avoid_: severity, blocking.

**Category**: The internal meaning of a group of **Status** values, independent of the label: _open_ (`backlog`, `todo`), _in-flight_ (`in_progress`, `in_review`), _closed_ (`done`). It is what the views use to filter. _Avoid_: group, status type.

**Label**: A global, fixed classification of the nature of an Issue. In v1 all Projects share the same vocabulary: `feature`, `bug`, `docs`, `tests`, `refactor`, `chore`, `research`, `infra`. An Issue can have zero or many Labels. _Avoid_: tag, custom label, category.

**Prefix**: The textual identifier of a **Project**, chosen at `ito init`, that composes the ID of every Issue (e.g., `AUTH` in `AUTH-12`). One Prefix per Project; in v1 it does not change after `init`. _Avoid_: namespace, scope, project code.

**Created**: The birth timestamp of an Issue. An **immutable** column, written once by `ito new`. _Avoid_: opening date.

**Updated**: The timestamp of the last mutation of an Issue. A column written **transactionally** by the CLI on every change (the CLI is the only writer, so it never desyncs). _Avoid_: modified, mtime, last_modified.

## Example dialogue

> **Dev:** "Where does an issue live?"
> **Domain:** "It's a row in the central SQLite, in `~/.ito/`. The markdown body is a column. There's no `.md` file scattered around the repo."
> **Dev:** "And if I open a worktree of the project?"
> **Domain:** "Same Project. Identity comes from the git root, and worktrees share the `.git` — so you see the same Issues from any worktree."
> **Dev:** "When the priority changes, does it stamp updated?"
> **Domain:** "It does. The CLI is the only writer, so every mutation updates the `updated` column in the same transaction. `created` doesn't — that one is fixed at `new`."
> **Dev:** "Can I edit the issue in Obsidian?"
> **Domain:** "No. Single writer: everything goes through the CLI (`ito edit`, `ito move`). Markdown export is a future thing, and read-only."
