# Waves are derived from the link graph, never stored

**Status:** accepted · 2026-06-12 · adds decisions #17–#20; extends the link model of §3.1.

## Context

Solo development with AI agents makes parallel execution cheap: independent Issues can be fanned out across git worktrees, one agent per Issue. What was missing was a way to *plan* that fan-out — to look at a coherent effort (a feature, a refactor) and see "these can run together now; these come next". The instinctive design — every mainstream tracker's design — is to store that plan: number the cycles, assign each Issue to one.

But ito already stores the information the plan is made of. `blocked_by` encodes order, and the ready frontier (`ito list --ready`) already proves the key property: Issues whose blockers are all `done` are mutually independent and safe to parallelize. A stored cycle assignment would be a second representation of the same facts.

## Decision

The persisted concept is the **Batch** — a named set of Issues planned together (slug + immutable `created`; at most one Batch per Issue; completion derived). The execution plan is **not** persisted: **Waves** are the topological generations of the Batch's members under the Link graph, recomputed at read time (SPEC §3.5). Wave 1 is the members with no pending blockers; Wave 2 is what they unblock; and so on.

Mutual exclusion — "these two must not run in parallel, though neither blocks the other" (e.g. agents would collide on the same files) — does not get a manual override or a pinned wave number. It becomes a third link type, **`conflicts_with`** (symmetric, like `relates_to`): the constraint goes *into* the graph, and the derivation pushes conflicting members into different Waves (tie-break: Priority, then ID). `--ready` honours it the same way, preserving the frontier's documented independence guarantee.

## Considered Options

- **Stored cycle assignment (issue → cycle N).** Rejected: the plan and the graph can contradict each other the moment a link changes (put an Issue in cycle 2, later add a `blocked_by` to something in cycle 3 — which one does the agent obey?). This is the same desync-between-two-representations class of bug that ADR-0001 killed by making SQLite the single source of truth.
- **Hybrid: derived by default, manual pin per Issue.** Rejected: wave numbers are moving targets (membership changes recompute the boundaries), so a pin either loses meaning or contradicts the graph. Every honest use case for the pin turned out to be mutual exclusion — which `conflicts_with` expresses inside the graph instead of around it.
- **Derived waves over the link graph (chosen).**

## Consequences

- **Gains:** Waves can never disagree with the Links; finishing an Issue advances the plan automatically (the frontier rolls forward); one definition of "ready" shared by `--ready`, `batch show`, and the TUI.
- **Costs:** the plan is only as good as the links — planning discipline means authoring `blocked_by`/`conflicts_with` when the Issues are created (in practice the agent does this during breakdown). A dependency cycle among members makes wave derivation fail; that is surfaced as a clear read-time error naming the Issues — a cycle is a graph error, never a wave (which is why the concept is named *Wave*, not *cycle*).
- **Reversal cost:** moving to stored assignments later would be a schema migration *plus* a semantic break (plans would stop self-advancing); this ADR exists so that choice is revisited deliberately, not "fixed" casually.
