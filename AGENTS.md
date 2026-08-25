## Issue tracking

This repo's issues live in **ito** — the local issue tracker this very repo builds (we dogfood it). Create, read, and update issues through the `ito` CLI; it is the only writer. The command surface lives in `ito --help` and `ito <cmd> --help`. If the repo doesn't have a project yet, run `ito init`.

When a Ledger is connected (`ito config`), `ito new` needs network to reserve its number; reads, edits and moves work offline.

Every Issue reserves its branch name at creation: right after `ito new`, run `ito edit <ID> --branch <prefix>-<n>-<slug>` — the lowercase ID plus a short slug from the title (e.g. `ito-42-batch-wave-sync`). Reserving is not creating: the branch comes into existence only when implementation starts, by checking out the recorded name. No type prefixes (`feat/`, `fix/`) — the type already lives in `category` and the PR title. If the field is empty when you start work, derive the name then and record it before proceeding.
