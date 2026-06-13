package tui

import (
	"strings"
	"testing"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func TestBatchesKeySwitchesSurfaceAndHeaderTabs(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-switch-app", "BSW", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "first-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Member issue", "todo", "medium", nil, "", "first-effort"); err != nil {
		t.Fatalf("create member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	batches := current.View()
	if !strings.Contains(batches, "ito · [1] digest · [2] batches") {
		t.Fatalf("expected batches header tab set, got:\n%s", batches)
	}
	if !strings.Contains(batches, "1 batches   batch-switch-app") {
		t.Fatalf("expected header right end to show Batch count and Project name, got:\n%s", batches)
	}
	if !strings.Contains(batches, "first-effort  (1)") {
		t.Fatalf("expected 2 to open the Batches surface, got:\n%s", batches)
	}

	current, _ = current.Update(keyMsg(t, "1"))
	digest := current.View()
	if !strings.Contains(digest, "1 issues   batch-switch-app") || !strings.Contains(digest, "h hide") {
		t.Fatalf("expected 1 to leave Batches for the Digest, got:\n%s", digest)
	}

	// The Board lives behind the : command line now; 2 still leaves it for the
	// Batches surface.
	current = openBoard(t, current)
	board := current.View()
	if !strings.Contains(board, "ito · board") || strings.Contains(board, "WAVE 1") {
		t.Fatalf("expected :board to open the Board, got:\n%s", board)
	}
	current, _ = current.Update(keyMsg(t, "2"))
	if view := current.View(); !strings.Contains(view, "first-effort  (1)") {
		t.Fatalf("expected 2 to open the Batches surface from the Board, got:\n%s", view)
	}
}

func TestBatchesRendersSectionsNewestFirstWithWaveGrouping(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-wave-app", "BWA", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "older-effort"); err != nil {
		t.Fatalf("create older batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Older member", "todo", "low", nil, "", "older-effort"); err != nil {
		t.Fatalf("create older member: %v", err)
	}
	if _, err := st.CreateBatch(project, "newer-effort"); err != nil {
		t.Fatalf("create newer batch: %v", err)
	}
	root, err := st.CreateIssueInBatch(project, "Extract config loader", "todo", "high", []string{"refactor"}, "", "newer-effort")
	if err != nil {
		t.Fatalf("create root member: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Introduce store interface", "todo", "medium", nil, "", "newer-effort"); err != nil {
		t.Fatalf("create independent member: %v", err)
	}
	blocked, err := st.CreateIssueInBatch(project, "Migrate writes", "todo", "medium", nil, "", "newer-effort")
	if err != nil {
		t.Fatalf("create blocked member: %v", err)
	}
	if _, err := st.Edit(project, blocked.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: root.ID}},
	}); err != nil {
		t.Fatalf("block member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()

	newerAt := strings.Index(view, "newer-effort  (3)")
	olderAt := strings.Index(view, "older-effort  (1)")
	if newerAt == -1 || olderAt == -1 || newerAt > olderAt {
		t.Fatalf("expected Batches newest-first, got:\n%s", view)
	}
	if !strings.Contains(view, "newer-effort  (3) · 0/3 done · wave 1/2  ") {
		t.Fatalf("expected heading to carry derived meta, got:\n%s", view)
	}
	if !strings.Contains(view, "    WAVE 1 · READY  (2)") || !strings.Contains(view, "    WAVE 2 · WAITING  (1)") {
		t.Fatalf("expected Wave sub-headings with READY on Wave 1 only, got:\n%s", view)
	}
	// The focused Batch's selected row wears the cursor; the rest sit two
	// columns right of Digest rows.
	if !strings.Contains(view, "    ▸ ▲ "+root.ID+" Extract config loader") ||
		!strings.Contains(view, "      ◆ ") {
		t.Fatalf("expected Digest-style rows indented under their Wave, got:\n%s", view)
	}
	if !strings.Contains(view, "⊘ "+root.ID) || !strings.Contains(view, "refactor") {
		t.Fatalf("expected rows to keep blocker markers and labels, got:\n%s", view)
	}

	batches, err := st.ListBatches(project)
	if err != nil {
		t.Fatalf("list batches: %v", err)
	}
	date := batches[0].Date()
	headingLine := ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "newer-effort") {
			headingLine = line
			break
		}
	}
	if !strings.Contains(headingLine, "─") || !strings.HasSuffix(headingLine, "  "+date+" ") {
		t.Fatalf("expected created date dim at the rule's right end, got %q", headingLine)
	}
}

func TestBatchesCountsDoneInHeadingAndCollapsesFullyDoneBatch(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-done-app", "BDN", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "mixed-effort"); err != nil {
		t.Fatalf("create mixed batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Open member", "todo", "medium", nil, "", "mixed-effort"); err != nil {
		t.Fatalf("create open member: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Finished member", "done", "low", nil, "", "mixed-effort"); err != nil {
		t.Fatalf("create done member: %v", err)
	}
	if _, err := st.CreateBatch(project, "shipped-effort"); err != nil {
		t.Fatalf("create shipped batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Shipped member", "done", "low", nil, "", "shipped-effort"); err != nil {
		t.Fatalf("create shipped member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()

	if !strings.Contains(view, "mixed-effort  (2) · 1/2 done · wave 1/1") {
		t.Fatalf("expected done members counted in the heading meta, got:\n%s", view)
	}
	if strings.Contains(view, "Finished member") {
		t.Fatalf("expected done members never listed under Waves, got:\n%s", view)
	}
	if !strings.Contains(view, "▸ shipped-effort  (1) · done · h to show") {
		t.Fatalf("expected fully-done Batch to start collapsed, got:\n%s", view)
	}
	if strings.Contains(view, "Shipped member") {
		t.Fatalf("expected collapsed Batch to hide its rows, got:\n%s", view)
	}
}

func TestBatchesRowsShowConflictPartnerAsSecondMarker(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-conflict-app", "BCF", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "conflict-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	winner, err := st.CreateIssueInBatch(project, "Conflict winner", "todo", "high", nil, "", "conflict-effort")
	if err != nil {
		t.Fatalf("create winner: %v", err)
	}
	loser, err := st.CreateIssueInBatch(project, "Conflict loser", "todo", "low", nil, "", "conflict-effort")
	if err != nil {
		t.Fatalf("create loser: %v", err)
	}
	if _, err := st.Edit(project, loser.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "conflicts_with", Action: "add", Target: winner.ID}},
	}); err != nil {
		t.Fatalf("link conflict: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()

	if !strings.Contains(view, "WAVE 1 · READY  (1)") || !strings.Contains(view, "WAVE 2 · WAITING  (1)") {
		t.Fatalf("expected conflicts_with to split the pair across Waves, got:\n%s", view)
	}
	if !strings.Contains(view, "⊘ "+winner.ID) || !strings.Contains(view, "⊘ "+loser.ID) {
		t.Fatalf("expected both rows to show the ⊘ conflict partner marker, got:\n%s", view)
	}
}

func TestBatchesCyclicBatchRendersCycleLineInsteadOfWaves(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-cycle-app", "BCY", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "cyclic-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	first, err := st.CreateIssueInBatch(project, "Cycle head", "todo", "medium", nil, "", "cyclic-effort")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := st.CreateIssueInBatch(project, "Cycle tail", "todo", "medium", nil, "", "cyclic-effort")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := st.Edit(project, first.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: second.ID}},
	}); err != nil {
		t.Fatalf("link first: %v", err)
	}
	if _, err := st.Edit(project, second.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: first.ID}},
	}); err != nil {
		t.Fatalf("link second: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()

	if !strings.Contains(view, "cyclic-effort  (2) · 0/2 done") {
		t.Fatalf("expected cyclic Batch heading to keep its progress meta, got:\n%s", view)
	}
	if !strings.Contains(view, "    ⊘ blocked_by cycle among "+first.ID+", "+second.ID) {
		t.Fatalf("expected a cycle line naming the Issues, got:\n%s", view)
	}
	if strings.Contains(view, "WAVE") {
		t.Fatalf("expected cyclic Batch to render no Waves, got:\n%s", view)
	}
}

func TestBatchesEmptyProjectShowsActionableHint(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-empty-app", "BEM", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()

	if !strings.Contains(view, "0 batches   batch-empty-app") {
		t.Fatalf("expected empty Batches header, got:\n%s", view)
	}
	if !strings.Contains(view, "no Batches yet") || !strings.Contains(view, "run ito batch new <name> to plan one") {
		t.Fatalf("expected an actionable ito batch new hint, got:\n%s", view)
	}
	if !strings.Contains(view, "r refresh   : cmd   q quit") {
		t.Fatalf("expected the empty state to trim the bottom bar to live keys, got:\n%s", view)
	}
	if strings.Contains(view, "tab focus") || strings.Contains(view, "⏎ open") {
		t.Fatalf("expected no row-bound shortcuts on the empty surface, got:\n%s", view)
	}
}

func TestBatchesWindowsRowsToTerminalHeight(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-window-app", "BWN", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "tall-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	for i := 1; i <= 8; i++ {
		if _, err := st.CreateIssueInBatch(project, "Member "+string(rune('0'+i)), "todo", "medium", nil, "", "tall-effort"); err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 12})
	current, _ = current.Update(keyMsg(t, "2"))
	small := current.View()
	if !strings.Contains(small, "↓ ") || !strings.Contains(small, " more") {
		t.Fatalf("expected small Batches viewport to window rows with an overflow indicator, got:\n%s", small)
	}
	if strings.Contains(small, "BWN-8 Member 8") {
		t.Fatalf("expected small Batches viewport to omit the last member, got:\n%s", small)
	}

	current, _ = current.Update(tea.WindowSizeMsg{Width: 88, Height: 30})
	tall := current.View()
	if !strings.Contains(tall, "BWN-8 Member 8") || strings.Contains(tall, " more") {
		t.Fatalf("expected taller Batches viewport to show every member, got:\n%s", tall)
	}
}

func TestBatchesTabCyclesFocusAcrossBatches(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-focus-app", "BFO", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, name := range []string{"older-effort", "newer-effort"} {
		if _, err := st.CreateBatch(project, name); err != nil {
			t.Fatalf("create batch %s: %v", name, err)
		}
		if _, err := st.CreateIssueInBatch(project, "Member of "+name, "todo", "medium", nil, "", name); err != nil {
			t.Fatalf("create member of %s: %v", name, err)
		}
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	view := current.View()
	if !strings.Contains(view, " ▌▾ newer-effort") || strings.Contains(view, " ▌▾ older-effort") {
		t.Fatalf("expected the newest Batch to wear the initial focus bar, got:\n%s", view)
	}
	if !strings.Contains(view, "tab focus   ↑↓ select") || !strings.Contains(view, "h hide") {
		t.Fatalf("expected the surface key set in the bottom bar, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "tab"))
	if view := current.View(); !strings.Contains(view, " ▌▾ older-effort") || strings.Contains(view, " ▌▾ newer-effort") {
		t.Fatalf("expected Tab to move focus to the next Batch, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "tab"))
	if view := current.View(); !strings.Contains(view, " ▌▾ newer-effort") {
		t.Fatalf("expected Tab to wrap focus back to the first Batch, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "shift+tab"))
	if view := current.View(); !strings.Contains(view, " ▌▾ older-effort") {
		t.Fatalf("expected Shift+Tab to cycle focus backward, got:\n%s", view)
	}
}

func TestBatchesUpDownMovesSelectionAcrossWavesClamped(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-select-app", "BSL", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "wave-walk"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	first, err := st.CreateIssueInBatch(project, "First ready", "todo", "high", nil, "", "wave-walk")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := st.CreateIssueInBatch(project, "Second ready", "todo", "medium", nil, "", "wave-walk")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	tail, err := st.CreateIssueInBatch(project, "Blocked tail", "todo", "medium", nil, "", "wave-walk")
	if err != nil {
		t.Fatalf("create tail: %v", err)
	}
	if _, err := st.Edit(project, tail.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: first.ID}},
	}); err != nil {
		t.Fatalf("block tail: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	if view := current.View(); !strings.Contains(view, "▸ ▲ "+first.ID) {
		t.Fatalf("expected the selection cursor on the first listed row, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ ◆ "+second.ID) {
		t.Fatalf("expected Down to select the next row, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ ◆ "+tail.ID) {
		t.Fatalf("expected Down to cross the Wave sub-heading transparently, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ ◆ "+tail.ID) {
		t.Fatalf("expected Down past the last row to clamp, got:\n%s", view)
	}

	for range 3 {
		current, _ = current.Update(keyMsg(t, "up"))
	}
	if view := current.View(); !strings.Contains(view, "▸ ▲ "+first.ID) {
		t.Fatalf("expected Up past the first row to clamp, got:\n%s", view)
	}
}

func TestBatchesSelectionFlowsAcrossBatchBoundaries(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-flow-app", "BFW", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Surface order is newest-first: open-head, shipped-middle (done, collapsed,
	// rowless), open-tail. The cursor flows head → tail and stops on every Batch
	// in turn, including the collapsed one, so h can reveal it from there.
	if _, err := st.CreateBatch(project, "open-tail"); err != nil {
		t.Fatalf("create tail batch: %v", err)
	}
	tail, err := st.CreateIssueInBatch(project, "Tail member", "todo", "low", nil, "", "open-tail")
	if err != nil {
		t.Fatalf("create tail member: %v", err)
	}
	if _, err := st.CreateBatch(project, "shipped-middle"); err != nil {
		t.Fatalf("create middle batch: %v", err)
	}
	shipped, err := st.CreateIssueInBatch(project, "Shipped member", "done", "low", nil, "", "shipped-middle")
	if err != nil {
		t.Fatalf("create shipped member: %v", err)
	}
	if _, err := st.CreateBatch(project, "open-head"); err != nil {
		t.Fatalf("create head batch: %v", err)
	}
	first, err := st.CreateIssueInBatch(project, "Head first", "todo", "high", nil, "", "open-head")
	if err != nil {
		t.Fatalf("create head first: %v", err)
	}
	second, err := st.CreateIssueInBatch(project, "Head second", "todo", "medium", nil, "", "open-head")
	if err != nil {
		t.Fatalf("create head second: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	if view := current.View(); !strings.Contains(view, "▸ ▲ "+first.ID) {
		t.Fatalf("expected the cursor on the newest Batch's first row, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ ◆ "+second.ID) {
		t.Fatalf("expected Down to select the next row, got:\n%s", view)
	}

	// Down past the head's last row stops on the collapsed shipped-middle
	// heading — focused, no row selected, its done member off-screen.
	current, _ = current.Update(keyMsg(t, "down"))
	view := current.View()
	if !strings.Contains(view, " ▌▸ shipped-middle") || !strings.Contains(view, "h to show") {
		t.Fatalf("expected Down to land focus on the collapsed Batch, got:\n%s", view)
	}
	if strings.Contains(view, "▸ ◆ "+second.ID) || strings.Contains(view, shipped.ID) {
		t.Fatalf("expected no row cursor on a collapsed Batch, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	view = current.View()
	if !strings.Contains(view, "▸ · "+tail.ID) || !strings.Contains(view, " ▌▾ open-tail") {
		t.Fatalf("expected Down to flow into the last open Batch, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ · "+tail.ID) {
		t.Fatalf("expected Down at the surface's end to stay put, got:\n%s", view)
	}

	// Up retraces the same stops: the collapsed Batch, then the head's last row.
	current, _ = current.Update(keyMsg(t, "up"))
	if view := current.View(); !strings.Contains(view, " ▌▸ shipped-middle") {
		t.Fatalf("expected Up to land back on the collapsed Batch, got:\n%s", view)
	}
	current, _ = current.Update(keyMsg(t, "up"))
	view = current.View()
	if !strings.Contains(view, "▸ ◆ "+second.ID) || !strings.Contains(view, " ▌▾ open-head") {
		t.Fatalf("expected Up to flow back onto the newest Batch's last row, got:\n%s", view)
	}
}

func TestBatchesSelectionLandsOnCollapsedBatchToReveal(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-land-app", "BLD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "older-effort"); err != nil {
		t.Fatalf("create older batch: %v", err)
	}
	older, err := st.CreateIssueInBatch(project, "Older member", "todo", "low", nil, "", "older-effort")
	if err != nil {
		t.Fatalf("create older member: %v", err)
	}
	if _, err := st.CreateBatch(project, "newer-effort"); err != nil {
		t.Fatalf("create newer batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Newer member", "todo", "high", nil, "", "newer-effort"); err != nil {
		t.Fatalf("create newer member: %v", err)
	}

	// Collapse the older Batch, then return focus to the newer one.
	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "h"))
	current, _ = current.Update(keyMsg(t, "tab"))

	// Down past the newer Batch's last row lands focus on the collapsed older
	// Batch's heading — no row selected, ready for h to reveal it.
	current, _ = current.Update(keyMsg(t, "down"))
	view := current.View()
	if !strings.Contains(view, " ▌▸ older-effort") || !strings.Contains(view, "h to show") {
		t.Fatalf("expected Down to land focus on the collapsed Batch, got:\n%s", view)
	}
	if strings.Contains(view, older.ID) {
		t.Fatalf("expected the collapsed Batch's rows to stay off-screen, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "h"))
	view = current.View()
	if !strings.Contains(view, " ▌▾ older-effort") || !strings.Contains(view, "▸ · "+older.ID) {
		t.Fatalf("expected h to reveal the Batch with the cursor on its member, got:\n%s", view)
	}
}

func TestBatchesSelectionSurvivesRefreshWhenIssueStillRenders(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-refresh-app", "BRF", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "refresh-keep"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	alpha, err := st.CreateIssueInBatch(project, "Keep alpha", "todo", "high", nil, "", "refresh-keep")
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	beta, err := st.CreateIssueInBatch(project, "Keep beta", "todo", "medium", nil, "", "refresh-keep")
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "▸ ◆ "+beta.ID) {
		t.Fatalf("expected the selection on beta before the refresh, got:\n%s", view)
	}

	// The agent finishes alpha from another process; r pulls the change in.
	if _, err := st.Move(project, alpha.ID, "done"); err != nil {
		t.Fatalf("move alpha done: %v", err)
	}
	current, _ = current.Update(keyMsg(t, "r"))
	view := current.View()
	if strings.Contains(view, alpha.ID) {
		t.Fatalf("expected the done member to leave the rows, got:\n%s", view)
	}
	if !strings.Contains(view, "refresh-keep  (2) · 1/2 done") {
		t.Fatalf("expected the heading progress to update, got:\n%s", view)
	}
	if !strings.Contains(view, "▸ ◆ "+beta.ID) {
		t.Fatalf("expected the selection to stay on beta by ID, got:\n%s", view)
	}
}

func TestBatchesHideTogglesFocusedBatchAndInteropsWithDefaultCollapse(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-hide-app", "BHD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "done-effort"); err != nil {
		t.Fatalf("create done batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Finished member", "done", "low", nil, "", "done-effort"); err != nil {
		t.Fatalf("create done member: %v", err)
	}
	if _, err := st.CreateBatch(project, "open-effort"); err != nil {
		t.Fatalf("create open batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Open member", "todo", "medium", nil, "", "open-effort"); err != nil {
		t.Fatalf("create open member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "h"))
	view := current.View()
	if !strings.Contains(view, "▸ open-effort  (1)") || !strings.Contains(view, "h to show") {
		t.Fatalf("expected h to collapse the focused Batch, got:\n%s", view)
	}
	if strings.Contains(view, "Open member") {
		t.Fatalf("expected a collapsed Batch to skip its rows, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "h"))
	if view := current.View(); !strings.Contains(view, "▾ open-effort  (1)") || !strings.Contains(view, "Open member") {
		t.Fatalf("expected h to reveal the Batch again, got:\n%s", view)
	}

	// The fully-done Batch starts collapsed by default; a manual reveal both
	// works and survives a refresh.
	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "h"))
	if view := current.View(); !strings.Contains(view, "▾ done-effort  (1) · done") || strings.Contains(view, "▸ done-effort") {
		t.Fatalf("expected h to reveal the fully-done Batch, got:\n%s", view)
	}
	current, _ = current.Update(keyMsg(t, "r"))
	if view := current.View(); !strings.Contains(view, "▾ done-effort  (1) · done") {
		t.Fatalf("expected the manual reveal to survive a refresh, got:\n%s", view)
	}
}

func TestBatchesEnterOpensIssueDetailAndEscReturnsInPlace(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-detail-app", "BDT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "detail-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Open me first", "todo", "medium", nil, "", "detail-effort"); err != nil {
		t.Fatalf("create first member: %v", err)
	}
	second, err := st.CreateIssueInBatch(project, "Open me second", "todo", "low", nil, "", "detail-effort")
	if err != nil {
		t.Fatalf("create second member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "down"))
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "ito · "+second.ID+" · Open me second") {
		t.Fatalf("expected Enter to open the selected member's detail, got:\n%s", view)
	}

	// A detail edit re-derives the surface it returns to: p cycles low → medium.
	current, _ = current.Update(keyMsg(t, "p"))
	current, _ = current.Update(keyMsg(t, "esc"))
	view := current.View()
	if !strings.Contains(view, " ▌▾ detail-effort") || !strings.Contains(view, "WAVE 1") {
		t.Fatalf("expected Esc to return to the Batches surface, got:\n%s", view)
	}
	if !strings.Contains(view, "▸ ◆ "+second.ID) {
		t.Fatalf("expected the edited member selected with its new priority mark, got:\n%s", view)
	}
	edited, err := st.FindIssue(project, second.ID)
	if err != nil {
		t.Fatalf("find edited member: %v", err)
	}
	if edited.Priority != "medium" {
		t.Fatalf("expected the detail edit to reach the store, got priority %q", edited.Priority)
	}
}

func TestBatchesStatusKeyRederivesWavesAndCollapsesCompletedBatch(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-edit-app", "BED", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "ship-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	core, err := st.CreateIssueInBatch(project, "Ship core", "in_review", "high", nil, "", "ship-effort")
	if err != nil {
		t.Fatalf("create core: %v", err)
	}
	docs, err := st.CreateIssueInBatch(project, "Ship docs", "todo", "medium", nil, "", "ship-effort")
	if err != nil {
		t.Fatalf("create docs: %v", err)
	}
	if _, err := st.Edit(project, docs.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: core.ID}},
	}); err != nil {
		t.Fatalf("block docs: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	if view := current.View(); !strings.Contains(view, "WAVE 2 · WAITING") {
		t.Fatalf("expected the blocked member on Wave 2 before the edit, got:\n%s", view)
	}

	// s moves the selected member in_review → done; the waves re-derive on the
	// spot: the done member leaves the rows and the blocked one becomes Wave 1.
	current, _ = current.Update(keyMsg(t, "s"))
	view := current.View()
	if strings.Contains(view, "Ship core") || strings.Contains(view, "WAVE 2") {
		t.Fatalf("expected the done member to dissolve its wave, got:\n%s", view)
	}
	if !strings.Contains(view, "ship-effort  (2) · 1/2 done · wave 1/1") {
		t.Fatalf("expected the heading progress to update, got:\n%s", view)
	}
	if !strings.Contains(view, "WAVE 1 · READY  (1)") || !strings.Contains(view, "▸ ◆ "+docs.ID) {
		t.Fatalf("expected the unblocked member selected on Wave 1, got:\n%s", view)
	}
	moved, err := st.FindIssue(project, core.ID)
	if err != nil {
		t.Fatalf("find core: %v", err)
	}
	if moved.Status != "done" {
		t.Fatalf("expected s to move the member through the store, got %q", moved.Status)
	}

	// Walking the last member to done completes the Batch and collapses it.
	for range 3 {
		current, _ = current.Update(keyMsg(t, "s"))
	}
	if view := current.View(); !strings.Contains(view, "▸ ship-effort  (2) · done · h to show") {
		t.Fatalf("expected the completed Batch to collapse on the spot, got:\n%s", view)
	}
}

func TestBatchesCommandLineRunsPriorityAndLabelsOnSelectedMember(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-cmd-app", "BCM", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "cmd-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	member, err := st.CreateIssueInBatch(project, "Tune me", "todo", "medium", nil, "", "cmd-effort")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, ":"))
	for _, r := range "priority" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "▸ ▲ "+member.ID) || strings.Contains(view, "esc cancel") {
		t.Fatalf("expected :priority to cycle the selected member and close the bar, got:\n%s", view)
	}
	cycled, err := st.FindIssue(project, member.ID)
	if err != nil {
		t.Fatalf("find member: %v", err)
	}
	if cycled.Priority != "high" {
		t.Fatalf("expected :priority to reach the store, got %q", cycled.Priority)
	}

	current, _ = current.Update(keyMsg(t, ":"))
	for _, r := range "labels" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "[ ] feature") {
		t.Fatalf("expected :labels to open the picker for the selected member, got:\n%s", view)
	}
	current, _ = current.Update(keyMsg(t, "enter")) // toggle feature
	current, _ = current.Update(keyMsg(t, "esc"))   // picker → detail
	current, _ = current.Update(keyMsg(t, "esc"))   // detail → batches
	view := current.View()
	if !strings.Contains(view, " ▌▾ cmd-effort") || !strings.Contains(view, "feature") {
		t.Fatalf("expected the toggled label back on the Batches row, got:\n%s", view)
	}
	labeled, err := st.FindIssue(project, member.ID)
	if err != nil {
		t.Fatalf("find labeled member: %v", err)
	}
	if len(labeled.Labels) != 1 || labeled.Labels[0] != "feature" {
		t.Fatalf("expected the label toggle to reach the store, got %v", labeled.Labels)
	}
}

func TestBatchesInlineFilterNarrowsRowsWithCounts(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-filter-app", "BFL", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "noise-effort"); err != nil {
		t.Fatalf("create noise batch: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Unrelated chore", "todo", "low", nil, "", "noise-effort"); err != nil {
		t.Fatalf("create noise member: %v", err)
	}
	if _, err := st.CreateBatch(project, "signal-effort"); err != nil {
		t.Fatalf("create signal batch: %v", err)
	}
	match, err := st.CreateIssueInBatch(project, "Fix parser bug", "todo", "high", []string{"bug"}, "", "signal-effort")
	if err != nil {
		t.Fatalf("create matching member: %v", err)
	}
	if _, err := st.CreateIssueInBatch(project, "Write docs page", "todo", "low", nil, "", "signal-effort"); err != nil {
		t.Fatalf("create other member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "/"))
	for _, r := range "bug" {
		current, _ = current.Update(runeMsg(r))
	}
	view := current.View()
	if !strings.Contains(view, " / bug▏") || !strings.Contains(view, "1 of 3 issues · esc to clear") {
		t.Fatalf("expected the filter input with matched/total counts, got:\n%s", view)
	}
	if !strings.Contains(view, match.ID+" Fix parser bug") {
		t.Fatalf("expected the matching row to stay listed, got:\n%s", view)
	}
	if strings.Contains(view, "Write docs page") || strings.Contains(view, "noise-effort") {
		t.Fatalf("expected non-matching rows and matchless Batches hidden, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	restored := current.View()
	if !strings.Contains(restored, "Write docs page") || !strings.Contains(restored, "noise-effort") {
		t.Fatalf("expected Esc to leave the filter and restore the rows, got:\n%s", restored)
	}
	if !strings.Contains(restored, "tab focus   ↑↓ select") {
		t.Fatalf("expected Esc to restore the shortcut bar, got:\n%s", restored)
	}
}
