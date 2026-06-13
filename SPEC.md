# `ito` — Spec / PRD

> **Status:** v1 implemented · v2 (TUI) implemented · v2 Batch/Wave extension settled · **Date:** 2026-06-13
> **Name:** **ito** — 糸 (*the thread that links the issues*) · 意図 (*intention/purpose*). "The thread of intentions." Binary: `ito`.

A **local, solo, "full local"** issue tracker, Linear-style, for the terminal — with the twist of being **AI-driven through the command line**.

> **Architectural turn (2026-05-24):** the source of truth moved from markdown-per-project to a **central SQLite**. See [`docs/adr/0001`](./docs/adr/0001-sqlite-single-source-of-truth.md).

---

## 1. Motivation

Today, starting to track a project requires ceremony: in Linear you *break your flow* to go configure a project before you can use it. But projects **are born from the conversation with the AI**, on the local machine. The goal is **traceability with zero ceremony**: the project emerges, and traceability appears with almost no manual step.

The AI already makes it trivial to have an idea and plan it — what's missing is the layer that records, visualizes and tracks the issues of that plan, without leaving the terminal and without configuring a SaaS for every new project.

### Non-goals (explicit)
- It is **not** an AI agent orchestrator (≠ [contrabass](https://github.com/junhoyeo/contrabass), which runs multiple agents to execute work).
- It is **not** multi-user / team / cloud sync (for now).
- It does **not** embed an LLM or an API key.
- It does **not** keep long-term history — issues are ephemeral.

---

## 2. Architecture decisions (with the why)

### 2.1 AI integration — *the root of everything*
The AI is **external**. The tool does **not** expose MCP and does **not** embed an LLM. The agent the user already uses (Claude Code) **drives the CLI via bash**.

- **Why:** MCP loads schemas into the context (a fixed token cost, even when unused); bash costs zero until it's called, and the agent already knows how to run commands.
- **Consequence:** the CLI must be **agent-native**:
  - LLM-readable `--help`;
  - `--json` on every command;
  - consistent exit codes;
  - **no interactive prompts** that would stall an agent.
- Since the CLI is the **only writer** (see §2.4), "driving via bash" isn't just the recommended way — it's the **only** way to touch the issues. That reinforces the principle rather than contradicting it.

### 2.2 How the agent learns to drive the CLI
- LLM-optimized `--help` — including the root `--help`, which briefly orients to the non-obvious model (central store, git-root resolution, Prefix) so a first run explains itself. There is no separate `guide` command; `--help` is the guide.

### 2.3 Topology & storage
- **Single central store:** a **SQLite** database in `~/.ito/` (overridable by env, e.g., `ITO_HOME`). **There is no `.ito/` inside the repo.**
- **Project identity ≠ path.** A Project has a **stable internal id**, a **name** (lowercase slug, default = the normalized folder name, **renameable** and **unique** within the store), a **prefix** and a **root_path** (a **mutable** pointer, which may be null). `ito init --name <name>` lets you choose the initial name; an invalid format or a collision fails with exit `2` and does not auto-suffix. The valid format is `[a-z0-9][a-z0-9-]{1,62}`; the folder-derived default is normalized to that format, but a manual `--name` must already be valid.
- **Implicit resolution (zero ceremony):** from inside the repo, it resolves the **main worktree root** (`git rev-parse --path-format=absolute --git-common-dir`, without the `/.git`) → finds the Project by `root_path`. The `root_path` recorded by `init` follows the same rule: it is the canonical git root shared by the worktrees, **not** the literal cwd where the command ran. All `git worktree`s share the `.git`, so they land in the **same Project**. The agent **never needs to name** the project in the normal case. Outside git, it matches the cwd against the registered `root_path`s (nearest ancestor); `ito init` outside git records the absolute cwd as `root_path`. If that folder later becomes a git repo, the Project can be re-pointed with `ito init --reattach <name>` to the canonical git root.
- **`--project <name>`** is an **optional** override (address from anywhere); `--all-projects` = all projects. They are not the default path.
- **Moved/renamed repo:** the `root_path` stops matching → exit `4`, **but the Project and its issues remain**. `ito init` never asks anything; when there is a compatible detached Project, it prints an actionable sentence pointing to `ito init --reattach <name>`. Re-pointing is explicit and deterministic by `name` → zero loss with no interactive prompt.
- **Why central:** issues accessible from **any cwd**; it solves the worktree problem (see ADR-0001) without versioning anything or scattering files.
- Issues remain **local and ephemeral** — they don't go into git. (Trade-off: switching machines doesn't carry the issues.)

### 2.4 Source of truth & write model
- **SQLite is the source of truth.** Each issue is a **row**; the markdown body lives in a **TEXT column** (markdown preserved, not lost).
- **Single writer:** the **CLI is the only writer**. Every create/edit goes through a command (`new`, `move`, `edit`, …). No Obsidian, no direct file editing.
- Concurrent writes are serialized by SQLite itself. There is no async queue, daemon or job table: each command waits for the write lock, writes in a transaction and returns the real result. The store opens with a 5s `busy_timeout`, so a command blocked on the write lock waits up to that long before erroring; beyond it (or sooner) cancellation is done by the calling process (Ctrl-C/kill/external timeout).
- **Why:** ready-made, tested **ACID** guarantees; single writer **eliminates the desync class of bug** that double writing created; a schema with migrations gives clean evolution (no frontmatter polluted over time).
- **Accepted trade-off (ADR-0001):** you lose Obsidian and file editing by the agent. **Markdown export** stays as a future feature (snapshot).

### 2.5 Identifiers
- **Prefix** per project, **unique within the store**, chosen at `init` (overridable with `--prefix`). The default is derived from the folder name by the **"strip-and-cap"** rule: transliterate unicode→ASCII, uppercase, keep only `[A-Z0-9]`, **cap at ~6 chars**, ensure it starts with a letter (fallback `ITO`). E.g.: `my-cool-project` → `MYCOOL`. Since `init` **cannot ask** (agent-native), on a collision it **auto-suffixes** (`API`→`API2`) and prints the chosen one. In v1, the Prefix is immutable after `init`; `ito rename <name>` changes only the `Project name`. A manual prefix (`--prefix`) must be valid as `[A-Z][A-Z0-9]{1,7}` (2 to 8 chars); if it collides, it fails with exit `2`. Auto-suffixing applies only to a generated default, never to a manual prefix.
- **Monotonic counter** per project, **transactional** in the database. **Never reused**: deleting `AUTH-12` does not make the next one go back to 12. Single writer + transaction make this trivial even with concurrent commands — **no reconciliation**.
- Since the Prefix is unique within the store, the textual ID (`AUTH-12`) identifies a single Issue globally. Commands that take a full ID (`show`, `move`, `edit`, `rm`) resolve the Project by the Prefix of the ID and work from any cwd. If `--project` is passed alongside and points to another Project, the command fails with exit `2`. v1 does not accept short IDs (`12`); every Issue reference in a command uses the full `<PREFIX>-<n>` format.

### 2.6 Footprint invariant
> **The CLI never writes inside the repository — only in `~/.ito/`.**

`.gitignore`, docs and user-orientation files stay out of `ito`. Since the store is central and identity comes from git, **there isn't even a single `ito` file in the repo** to configure or ignore. (The invariant ended up stronger than in the original design.)

---

## 3. Data model

### 3.1 Relations (v1 = flat, no hierarchy)
- **In v1, every issue is flat.** **No `parent`, no `type`, no epic.** (Scope decision: the author doesn't use epics; YAGNI. See §9.)
- **Flat typed links** (non-hierarchical): `blocked_by`, `relates_to`. These stay. `blocked_by` is directional: the source issue is blocked by the target issue. `relates_to` is symmetric: the CLI normalizes the pair so it doesn't duplicate `A relates_to B` and `B relates_to A`. **`conflicts_with` (v2)** is symmetric like `relates_to` (same pair normalization): the two Issues must not be worked **in parallel** — mutual exclusion for scheduling, not order; neither blocks the other. Links are always intra-Project in v1; links crossing Projects fail write validation. Broken links are **blocked by FK on write**. `blocked_by` does not prevent Status transitions; it is information for reading and traceability, not a workflow rule. It does feed a derived **ready frontier** (`ito list --ready`, §6): the Issues that can be started now — `backlog`/`todo` with every `blocked_by` target `done` (no-blocker Issues included). From v2 the frontier also honours `conflicts_with`: an Issue leaves the frontier while a conflict partner is in-flight, and between two otherwise-ready conflicting Issues only the deterministic winner (Priority, then ID) stays — so the independence property below survives. A useful property: any two Issues in the frontier are mutually independent (if A blocked B, B would not be ready until A is `done`, by which point A has left the frontier), so the frontier is exactly the set safe to fan out across git worktrees *logically* — physical file overlap is the agent's call, not ito's. This stays a read-time view, not a transition rule (`move` still accepts any target), and ito never creates or manages the worktrees (it informs the frontier; git + the AI act on it — the "not an orchestrator" non-goal holds).
- **Traceability (v1)** = following the links. The `parent`/epic tree is left for a future evolution.
- **Evolution is cheap (the SQLite dividend):** adding hierarchy later is a non-destructive migration (`ALTER TABLE issues ADD COLUMN parent …`); old issues become `parent = NULL`. That's why the **v1 core already embeds a migration mechanism** (`schema_version` + ordered migrations) — it's what makes that evolution a versioned, clean step. It **does not become an ADR** because it's trivial to revert (the opposite of "hard to revert").

### 3.2 Status (fixed)
```
backlog → todo → in_progress → in_review → done
```
> **The arrows are a recommended happy path, not a mandatory rail.** `move` accepts **any source → any target** (skip `todo`, reopen a `done`, send back from `in_review`). It validates **only the target**: the target must be a known status (otherwise exit `2`) and the ID must exist (otherwise exit `3`); moving to the current status is a no-op (exit `0`) and doesn't change `updated`. Enforcing flow discipline is not a goal of `ito` (solo/local/AI); if it ever is, it becomes opt-in.
> There is also no WIP limit: multiple Issues can be in `in_progress` at the same time.

Each status belongs to a **category** (internal meaning, independent of the label):

| Category   | Status                    |
|------------|---------------------------|
| open       | `backlog`, `todo`         |
| in-flight  | `in_progress`, `in_review`|
| closed     | `done`                    |

Semantics of the early stages: `backlog` = work that's mapped or still subject to review; `todo` = work selected for execution; `in_progress` = work that's active right now.

`in_review` fits the AI flow: the agent does → you review → done. Statuses are fixed in v1; there is no per-Project customization.

### 3.3 Issue fields
| Field        | Type            | Notes                                            |
|--------------|-----------------|--------------------------------------------------|
| `id`         | text            | `<PREFIX>-<n>`, e.g.: `AUTH-12`                  |
| `title`      | text            | required and non-empty                            |
| `status`     | enum            | `backlog\|todo\|in_progress\|in_review\|done`            |
| `priority`   | enum            | default `low`; `low\|medium\|high\|urgent`       |
| `blocked_by` | refs            | typed links                                      |
| `relates_to` | refs            | typed links                                      |
| `conflicts_with` | refs        | typed links (v2): mutual exclusion, "not in parallel" |
| `batch`      | text \| null    | slug of the owning Batch (v2); `null` = no Batch |
| `labels`     | list of enum    | zero or many; `feature\|bug\|docs\|tests\|refactor\|chore\|research\|infra` |
| `body`       | TEXT            | free-form **markdown** body; optional             |
| `created`    | timestamp       | UTC/RFC3339; immutable, written once at `new`    |
| `updated`    | timestamp       | UTC/RFC3339; written **transactionally** on every real mutation |

> No `owner` (solo). `updated` is now a reliable column — the old hack of deriving recency from `mtime` died together with double writing (ADR-0001).

### 3.4 Lifecycle
- `done` is **just a status**. Nothing is deleted or archived automatically.
- **Deleting is a manual act:** `ito prune --status done` (in bulk) or `ito rm <ID>`.
- Removal is **destructive** and has no trash. `rm` deletes the Issue and its Links/Labels in the same transaction; links from other Issues that point to the removed Issue are also deleted. No other Issue is removed by cascade. `prune` requires an explicit filter (e.g. `--status done`) and explicit confirmation by flag (`--yes`); without both, it fails with exit `2`. There is no interactive prompt.
- Removed IDs are **never reused**; the Project counter stays monotonic.
- Active views filter out `done` by default.

### 3.5 Batches & Waves (v2)
- A **Batch** is a named set of Issues planned together as one coherent effort (a feature, a refactor, a fix). Identity follows the Project pattern: a unique lowercase slug `name` (same format as a Project name, renameable) plus an immutable `created` timestamp. Listings order Batches by creation date, newest first — **date is chronology, never identity** (two efforts born the same day stay separate and nameable).
- **Membership is at most one Batch per Issue** (a nullable column, §4); `NULL` = outside any Batch. Work shared across efforts is expressed through Links (`blocked_by` pointing at the common Issue), never double membership — progress and ownership stay unambiguous.
- A **Wave** is **derived, never stored**: the topological generations of the Batch's members under the Link graph. Wave 1 = members with no pending blockers; Wave 2 = the ones unblocked once Wave 1 is `done`; and so on. `conflicts_with` pushes mutually exclusive members into different Waves (tie-break: Priority, then ID). Blockers **outside the Batch count** — one definition of "ready", shared with the frontier (§3.1). Issues in the same Wave are mutually independent: the set safe to fan out across git worktrees in parallel. `ito list --batch <name> --ready` is the current Wave.
- **Why derived:** a stored plan can contradict the graph the moment a link changes — the desync class of bug again (§2.4). Recomputing at read time makes the contradiction impossible and keeps the Links the single source of truth. See [`docs/adr/0003`](./docs/adr/0003-derived-waves-not-stored-cycles.md) for the rejected alternatives. A dependency **cycle** among members makes wave derivation fail with a clear read-time error naming the Issues involved (a cycle is a graph error, never a wave — see `CONTEXT.md`).
- **Batch completion is derived too:** a Batch is complete when every member Issue is `done`; adding open work reopens it by definition. No stored batch status, no close command; abandoning an effort is expressed by the member Issues' fate (`rm`, `done`, or leaving the Batch).
- ito still **never creates or manages worktrees** — Batches and Waves inform the fan-out; git + the AI act on it (the "not an orchestrator" non-goal holds).

---

## 4. Storage

```
~/.ito/
  ito.db        # SQLite: projects, issues, links, prefix counters
```
(overridable by env, e.g., `ITO_HOME`.)

Schema sketch (indicative, not final):

```sql
CREATE TABLE schema_version (version INTEGER NOT NULL);  -- ordered migrations

CREATE TABLE projects (
  id        INTEGER PRIMARY KEY,    -- durable identity
  name      TEXT UNIQUE NOT NULL,   -- lowercase slug, renameable
  root_path TEXT UNIQUE,            -- mutable pointer; NULL = "detached" (repo moved)
  prefix    TEXT UNIQUE NOT NULL,
  last_id   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE issues (              -- v1: flat issue (parent/type arrive via a future migration)
  row_id     INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  id         TEXT NOT NULL,          -- AUTH-12
  title      TEXT NOT NULL,
  status     TEXT NOT NULL,
  priority   TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  batch_id   INTEGER REFERENCES batches(id) ON DELETE SET NULL,  -- v2; NULL = no Batch
  created    TEXT NOT NULL,
  updated    TEXT NOT NULL,
  UNIQUE (project_id, id)
);

CREATE TABLE batches (             -- v2
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  name       TEXT NOT NULL,         -- lowercase slug, renameable
  created    TEXT NOT NULL,         -- immutable; listings group by this date
  UNIQUE (project_id, name)
);
-- No status column: Batch completion and Waves are derived at read time (§3.5).

CREATE VIRTUAL TABLE issues_fts USING fts5(
  title,
  body,
  content='issues',
  content_rowid='row_id',
  tokenize='unicode61',
  prefix='2 3 4'
);
-- The CLI updates issues_fts in the same transaction as new/edit/rm/prune.

CREATE TABLE issue_links (
  project_id INTEGER NOT NULL,
  source_id  TEXT NOT NULL,
  target_id  TEXT NOT NULL,
  kind       TEXT NOT NULL,          -- blocked_by | relates_to | conflicts_with (v2)
  PRIMARY KEY (project_id, source_id, target_id, kind),
  FOREIGN KEY (project_id, source_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  FOREIGN KEY (project_id, target_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  CHECK (source_id != target_id)
);
-- Links are intra-Project by design: source_id and target_id use the same project_id.
-- For relates_to and conflicts_with, the CLI orders source_id/target_id before writing.

CREATE TABLE issue_labels (
  project_id INTEGER NOT NULL,
  issue_id   TEXT NOT NULL,
  label      TEXT NOT NULL,          -- feature | bug | docs | tests | refactor | chore | research | infra
  PRIMARY KEY (project_id, issue_id, label),
  FOREIGN KEY (project_id, issue_id) REFERENCES issues(project_id, id) ON DELETE CASCADE
);
-- Labels are global and fixed in v1; the CLI validates the vocabulary before writing.
```

---

## 5. Stack

- **Go** + the **Charm** ecosystem (Bubble Tea / Lip Gloss / Bubbles) for the TUI.
- **SQLite** via a Go driver (e.g.: `modernc.org/sqlite`, pure-Go → keeps the single static binary, no CGO).
- **Why:** a single static binary, instant cold start (it matters because the agent invokes it a lot), heavily-tested SQLite, the best TUI stack on the market for phase 2.

---

## 6. Command surface (v1)

> v1 surface:

| Command             | Does                                                          |
|---------------------|--------------------------------------------------------------|
| `ito init`          | Registers the current project (git root or cwd) in the central store: creates `name` (`--name` or default = folder name) + `prefix` (`--prefix` or derived default). Never asks anything; if there's a compatible "detached" Project, it prints the explicit action `ito init --reattach <name>`. **Doesn't touch the repo.** |
| `ito init --reattach <name>` | Re-points the `root_path` of an existing Project to the current root. It's the deterministic path for a moved/renamed repo. |
| `ito rename <name>` | Renames the Project (the `id`, the `prefix`, the `root_path` and the issues stay intact). |
| `ito new`           | Mints an ID + creates the issue. Requires a non-empty `--title`; accepts `--status` (default `backlog`), `--priority` (default `low`), a repeatable `--label` for initial Labels, `--body "text"` or `--body -` to read stdin. Without `--body`, the body is empty. Prints the ID. |
| `ito edit <ID>`     | Edits an issue's fields/body (`--title`, `--priority`, `--body "text"` or `--body -`; the body always replaces the whole text) and applies repeatable incremental operations (`--add-label`, `--remove-label`, `--block`, `--unblock`, `--relate`, `--unrelate`). Requires at least one change; without changes it fails with exit `2`. Redundant operations are idempotent (exit `0`) and only stamp `updated` when the final state changes. |
| `ito list`          | Lists issues of the current project (hides `done` by default; use `--status done` to see completed ones). Separate axes: **scope** `--all-projects`; **status** `--status <s>` (filters). Plus `--label`, `--priority`, `--search`, `--ready`, `--json`. `--ready` narrows to the **ready frontier** (§3.1): `backlog`/`todo` Issues whose `blocked_by` are all `done` — the set safe to fan out across worktrees. Default ordering: Status in the flow (`backlog`, `todo`, `in_progress`, `in_review`, `done` when filtered), Priority (`urgent`, `high`, `medium`, `low`), then `updated` desc. With `--search`, it orders by textual ranking and uses the normal ordering as a tie-breaker. With `--all-projects`, human output groups by Project; JSON returns a flat array ordered by `project`, then the normal ordering. |
| `ito show <ID>`     | Shows an issue (fields + body + links). `--json`. |
| `ito move <ID> <status>` | Status transition (validates only the target, §3.2); stamps `updated` only when the Status changes. |
| `ito rm <ID>` / `ito prune` | Destructively deletes an issue / deletes in bulk with a required filter and `--yes` (e.g.: `--status done --yes`). |

Principles: every v1 command has `--json`; no command blocks waiting for interactive input. Commands with a full ID resolve the Project by the Prefix of the ID; commands without a full ID resolve by the cwd, unless `--project` or `--all-projects`. v1 full-text search uses SQLite FTS5 over `title` + `body`, not over ID or metadata. `--search <text>` treats the input as a simple search: the CLI splits it into terms and searches by prefix (`login oauth` → `login* oauth*`), without exposing the advanced FTS5 syntax. `list` filters combine with AND. Repeated `--label` flags are also AND: `--label feature --label infra` returns Issues that have both Labels. `--ready` is a computed filter (it evaluates each Issue's blockers, counting a blocker as satisfied only when `done`); it AND-combines with the rest, so a contradictory combination like `--ready --status in_progress` returns an empty set, not an error.

### 6.1 Result contract (agent-native)
The agent has no special protocol with the CLI: it runs in the shell and receives **stdout**, **stderr** and an **exit code**. Hence the contract:

- **Success:** exit `0`. With `--json` → **raw data** (array/object, no envelope). Without `--json` → table/human text.
- Human write output is minimal: `new` prints only the ID; `edit`/`move` print a short sentence (`AUTH-12 updated.` or `AUTH-12 unchanged.`); `rm` prints `AUTH-12 deleted.`; `prune` prints the deleted count.
- **Failure:** exit **≠ 0** + a **human, actionable message on stderr** (in both modes). The actionable sentence is the product — e.g.: `this directory is not an ito project. run 'ito init' to get started.` It's what makes the agent take the right action (it sustains the "zero ceremony" flow).
- **With `--json`:** the error also comes out as an object on stderr (`{"error","code","hint"}`), for programmatic consumers.
- **Exit codes** (a small, stable taxonomy):

  | Code | Meaning                |
  |------|------------------------|
  | `0`  | success                |
  | `1`  | generic error          |
  | `2`  | usage / bad arguments  |
  | `3`  | not found (ID)         |
  | `4`  | project not initialized (cwd not registered) |

Philosophy: **the actionable sentence is the product; the exit code is the bonus for automation.**

### 6.2 Shape of `--json` (success)
- **Per command:** `list` → an **array** of issue objects; `show` → **one** object; `new`/`move`/`edit` → the **resulting** object (the agent gets the ID/new state without an extra `show`); `rm` → a summary (`{"deleted":1,"id":"AUTH-12"}`); `prune` → a summary (`{"deleted":3}`); `init`/`rename` → a Project object (`{"id":1,"name":"ito","prefix":"ITO","root_path":"/abs/path"}`). In a Project, `root_path` can be `null` when detached.
- An empty list is a success: without `--json`, it prints a short actionable sentence (e.g.: `no open issues.`); with `--json`, it prints `[]`.
- **Canonical issue object:**

  ```json
  {
    "id": "AUTH-13",
    "project": "myproject",
    "title": "Login via OAuth",
    "status": "todo",
    "priority": "high",
    "labels": ["feature", "infra"],
    "blocked_by": ["AUTH-9"],
    "relates_to": [],
    "conflicts_with": [],
    "body": "## Context\n...",
    "created": "2026-05-24T14:30:00Z",
    "updated": "2026-05-24T14:32:10Z"
  }
  ```

- **Shape rules:** `snake_case` keys; **stable shape** (empty arrays always present, never omitted → no null-check for the agent); timestamps in **ISO 8601 UTC**; **`body` is dropped from `list`** (token-lean) and **kept in `show`**; when empty, `body` is an empty string (`""`), never `null`; **`project` always present** (identical shape with/without `--all-projects`).

### 6.3 Batch surface (v2)
The Batch CRUD is the CLI's **first noun namespace** — the top level stays Issue verbs; `ito batch <verb>` administers the container.

| Command                      | Does                                                          |
|------------------------------|---------------------------------------------------------------|
| `ito batch new <name>`       | Creates a Batch in the current Project. The slug is validated like a Project name; a collision fails with exit `2`. |
| `ito batch list`             | Lists the Project's Batches newest-first, each with its `created` date and derived progress (members `done`/total, current Wave). |
| `ito batch show <name>`      | The Batch's members grouped by **derived Waves**, plus progress. A dependency cycle among members is a clear error naming the Issues involved. |
| `ito batch rename <old> <new>` | Renames the Batch; membership and `created` stay intact.    |
| `ito batch rm <name>`        | Deletes the Batch and clears membership — **never deletes Issues**. |

Membership travels on the existing Issue commands: `ito new --batch <name>`, `ito edit <ID> --batch <name>` (and `--batch ""` to leave), `ito list --batch <name>` (AND-combines with the other filters; `--batch <name> --ready` = the current Wave). `--block`/`--unblock`/`--relate`/`--unrelate` gain `--conflict`/`--unconflict` siblings for the new link type. Everything keeps `--json`; the canonical issue object (§6.2) gains `"conflicts_with": []` and `"batch": "<name>" | null` (the stable-shape rule holds: always present, never omitted).

---

## 7. Build phases

**v1 — usable by the agent (vertical slices)**
Core (SQLite schema + migrations, project resolution via git, ID minting) + `init / rename / new / edit / list / show / move / rm / prune` + `--json`. → *The agent can already work.*

**v2 — TUI**
A navigable TUI (Bubble Tea) **on top of the same core** — primarily an accompaniment surface (you watch what the agent plants). See [`docs/adr/0002`](./docs/adr/0002-tui-calls-core-in-process.md) for how it reaches the core (in-process, not by shelling out).
- **Two surfaces over one state, plus an issue view.** **Digest** (the primary surface) renders the Issues as full-width rows grouped by Status; **Board** renders the same Issues as a five-Status kanban. They share everything but the layout — the read path, the selection/focus model, the edits, the filter — so the Board is a second renderer over the state the Digest already builds, not a second feature. *Why Digest is primary:* grouped rows read better at a glance and fit a tall terminal, which is exactly the "watch what the agent plants" purpose; a five-column kanban truncates titles and splits attention. The header tabs are `[1]` (digest) / `[2]` (batches); the **Board sits behind the `:` command line** (`board` action, `esc` returns to the Digest), demoted from the tabs on 2026-06-12 — in practice the Digest and Batches carry the daily flow, and a tab earns its number key by being daily.
- **Launch:** bare `ito` opens the **Digest** when both stdin and stdout are a TTY; otherwise it prints the root help and exits `0` (so an agent driving via bash never stalls). There is no `tui` subcommand; explicit subcommands always follow the CLI path.
- **Scope & switching:** scoped to the cwd-resolved Project, with an in-TUI switcher across Projects (reached through the `:` command line); when the cwd has no Project, it opens a picker of existing Projects (or the `ito init` hint when the store is empty).
- **Hide / reveal (`h`):** in the Digest, `h` hides or reveals the focused Status section; `done` starts hidden, so `h` generalises the old "`done` hidden by default". The **Board always shows all five Statuses** — revealing a hidden Board column would need a config/reveal surface we are not building now. *Deferred to v3:* persisting which sections are hidden, and hiding columns on the Board.
- **Sizing:** the Digest and Board size the number of visible items to the terminal height (Bubble Tea's `tea.WindowSizeMsg`, recomputed on resize). The sliding window (`↑ N more` / `↓ N more`) is the overflow fallback when content doesn't fit, not a fixed cap.
- **Read:** the rows/columns render Issues (one line each, `…`-truncated) plus a read-only detail view (fields + body + links). This is the cheap, high-value half — it reuses the core's read path entirely.
- **Edit (minimal):** Status (move), Priority (cycle), Labels (toggle). Nothing else. Reached through the always-visible keys (`s`) and, for the rarer edits (`p`, `l`), the issue view and the `:` command line.
- **Filter & command line (inline, no separate screen):** `/` narrows the current surface to matching Issues live as you type (read-only); `:` is a closed launcher over the **v2 action set only** — Status/Priority/Labels, open the Board, switch Project, refresh, quit — never create or edit title/body/links (those are v3). Both turn the bottom shortcut bar into a text input; `esc` leaves the field.
- **Refresh:** manual, via a key (`r`). The TUI's own edits reload immediately; `r` pulls in what the agent wrote from another process. No polling, no file-watching.
- **Batches `[2]` (extension, settled 2026-06-12):** a surface beside the Digest in the header tabs — **one screen, no drill-in**. Each Batch renders as a Digest-style section (focus bar, name, derived progress, its `created` date dim at the right end of the rule), ordered newest-first; its open members group under quiet **Wave** sub-headings (`WAVE n · READY/WAITING`), done members live in the heading's progress count, and a fully-done Batch starts collapsed. Rows are Digest rows (priority mark, id, title, `⊘` group and labels right-aligned); a `conflicts_with` partner shows as a second `⊘` in its own colour next to the blocked marker. Same selection/focus model as the Digest (`tab` focuses a Batch, `↑↓` selects, `h` hides the focused Batch), same minimal edits (`s`, `p`, `l`), same `/` filter; `enter` opens the Issue detail. It reuses the section machinery the Digest already pays for; the genuinely new cost is the wave derivation, which lives in the core and also feeds the CLI (§6.3). Assigning Issues to Batches stays **out of the v2 TUI** (CLI only) — membership editing joins title/body/links in v3. (Static mock: `batches-prototype.html` at the repo root.)
- **Build order & escape hatch:** the **Digest ships first** (it is the default and pays for the shared core); the **Board follows within v2** as the second renderer. Its only genuinely new cost is the responsive horizontal layout (budgeting column widths to the terminal, sliding across columns when the five don't fit). If that layout proves costly, the **Board slips to v3** — the shared core is already built either way.

> "Column", "row" and "section" are UI rendering vocabulary, never a synonym for **Status** in the domain (see `CONTEXT.md`); the surfaces render **Issues**, not "cards".

**v3 — traceability + export + (maybe) hierarchy + richer TUI editing**
- Traceability views over the links: `blocked_by` graph, and the **visual ready frontier** (ready vs. blocked) in the TUI. The CLI primitive that feeds it — `ito list --ready` (§3.1/§6) — is a standalone query over existing data and ships ahead of the visual, independent of the v2 TUI work.
- TUI editing deferred from v2: title, body, links, Batch membership, create (`new`) and delete (`rm`/`prune`). Links land naturally alongside the traceability views.
- TUI state deferred from v2: persisting which Digest sections are hidden, and hiding columns on the Board.
- **Hierarchy (`parent`/epic)** — if missed — arrives here via migration.
- `ito export` to markdown (snapshot).

---

## 8. Target flow (how it all comes together)

1. A project is born from the conversation with the AI.
2. The agent detects that the project is not initialized → runs `ito init` (zero ceremony).
3. The agent generates the issues: runs `ito new` N times (body via `--body`/stdin). **This is where traceability begins.**
4. You visualize it in the terminal (`list`/TUI) — from any worktree of the project.
5. As you finish, you mark `done`; whenever you want, `prune`.

---

## 9. Closed ends

- [x] ~~The tool's definitive name~~ → **`ito`** (糸/意図).
- [x] ~~Source of truth~~ → **central SQLite** (ADR-0001).
- [x] ~~`--json` format~~ → raw data; canonical issue object, stable shape, `body` only in `show`. (See §6.2.)
- [x] ~~Ambiguous `--all`~~ → separate axes: scope `--all-projects`; status `--status <s>`. (See §6.)
- [x] ~~Rules for `parent`/`type`/epic~~ → **out of v1**: every issue is flat (YAGNI). Only flat `blocked_by`/`relates_to` links kept. Hierarchy arrives later via a non-destructive migration. (See §3.1.)
- [x] ~~Final schema of the labels table~~ → global, fixed Labels in v1: `feature`, `bug`, `docs`, `tests`, `refactor`, `chore`, `research`, `infra`. (See §3.3/§4.)
- [x] ~~Project resolution outside git / moved repo~~ → durable identity (`id`+unique `name`), `root_path` = mutable pointer; moved → re-point via `ito init --reattach <name>` with no prompt. (See §2.3.)
- [x] ~~Per-project status customization~~ → out of v1; fixed statuses are enough.
- [x] ~~Grouping work for parallel agent fan-out (sprint-like cycles?)~~ → **Batch + derived Waves** (v2, settled 2026-06-12): a Batch is a named set of Issues (slug + immutable `created`; date = chronology, never identity; ≤1 Batch per Issue; completion derived). Waves are the topological generations of the Link graph — **never stored**, so they can't contradict the links. Mutual exclusion gets its own link type, `conflicts_with`. (See §3.5, §6.3.)

---

## Appendix — Decision log

| # | Decision | Choice |
|---|----------|--------|
| 0 | Name | `ito` (糸 thread / 意図 intention). |
| 1 | Where the AI lives | External, drives the CLI via bash. No MCP, no embedded LLM. |
| 2 | Agent guide | LLM-optimized `--help` (also orients to the non-obvious model; no separate `guide` command). |
| 3 | Topology | **Central SQLite in `~/.ito/`**; durable identity (`id`+unique `name`), `root_path` = mutable pointer resolved via the git root (worktrees share it). *(ADR-0001 — reverts per-project `.ito/`.)* |
| 4 | Source of truth | **SQLite, single writer.** Body in a TEXT column. *(ADR-0001 — reverts markdown/frontmatter.)* |
| 5 | Hierarchy | **v1 = flat issues, no `parent`/`type`/epic** (YAGNI). Only flat `blocked_by`/`relates_to` links. Hierarchy later via migration. |
| 6 | Status | Fixed: backlog/todo/in_progress/in_review/done + categories. `move` validates only the target. |
| 7 | ID | Prefix at init + **transactional** monotonic counter. Never reused, no reconciliation. |
| 8 | Fields | `title`/`status`/`priority`/`labels`/`body` + links. No `owner`, no `type`/`parent` (v1). `created`/`updated` = timestamp columns. |
| 9 | Writing | **Single writer: the CLI only.** No Obsidian/file editing. *(ADR-0001 — reverts double writing.)* |
| 10 | Footprint | The CLI **never writes in the repo** — only in `~/.ito/`. |
| 11 | Lifecycle | `done` is just a status; deleting is manual (`rm`/`prune`). |
| 12 | Stack | Go + Charm + pure-Go SQLite (no CGO). |
| 13 | Human interface | A full TUI (Bubble Tea), v2, on top of the commands. Header tabs **Digest** (primary, grouped rows) `[1]` + **Batches** (waves) `[2]`, plus the **Board** (kanban) behind the `:` command line and a read-only issue view; `/` filter and a closed `:` command line. |
| 14 | Build order | Vertical slices: usable commands (v1) → TUI (v2) → traceability+export (v3). |
| 15 | Recency | `created`/`updated` = reliable timestamp columns (the mtime hack dropped along with double writing). |
| 16 | Result contract | Success: exit 0 + raw data (`--json`). Failure: exit ≠ 0 + an actionable sentence on stderr (+ an error object in `--json`). Fixed taxonomy of exit codes. |
| 17 | Parallel fan-out grouping | **Batch** (named set of Issues; slug + immutable `created`; ≤1 per Issue; completion derived) + **Wave** (derived topological generations of the Link graph; never stored). v2 extension. |
| 18 | Mutual exclusion | Third link type **`conflicts_with`** (symmetric): "not in parallel". Honoured by Waves and by `--ready` (deterministic winner — Priority, then ID — preserves the frontier's independence property). |
| 19 | Batch CLI | First noun namespace: `ito batch new/list/show/rename/rm`; membership via `--batch` on `new`/`edit`/`list`; `batch rm` never deletes Issues. |
| 20 | Batch TUI | Second tab `[2]`, one screen: each Batch a Digest-style section (newest first, `created` at the rule's right end), members grouped by Wave sub-headings; no drill-in. Membership editing deferred to v3. |
