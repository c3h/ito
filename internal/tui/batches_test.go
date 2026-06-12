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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
	batches := current.View()
	if !strings.Contains(batches, "ito · [1] digest · [2] board · [3] batches") {
		t.Fatalf("expected batches header tab set, got:\n%s", batches)
	}
	if !strings.Contains(batches, "1 batches   batch-switch-app") {
		t.Fatalf("expected header right end to show Batch count and Project name, got:\n%s", batches)
	}
	if !strings.Contains(batches, "first-effort  (1)") {
		t.Fatalf("expected 3 to open the Batches surface, got:\n%s", batches)
	}

	current, _ = current.Update(keyMsg(t, "1"))
	digest := current.View()
	if !strings.Contains(digest, "1 issues   batch-switch-app") || !strings.Contains(digest, "h hide") {
		t.Fatalf("expected 1 to leave Batches for the Digest, got:\n%s", digest)
	}

	current, _ = current.Update(keyMsg(t, "3"))
	current, _ = current.Update(keyMsg(t, "2"))
	board := current.View()
	if !strings.Contains(board, "IN PROGRESS  (0)") || strings.Contains(board, "WAVE 1") {
		t.Fatalf("expected 2 to leave Batches for the Board, got:\n%s", board)
	}

	current, _ = current.Update(keyMsg(t, "3"))
	if view := current.View(); !strings.Contains(view, "first-effort  (1)") {
		t.Fatalf("expected 3 to open the Batches surface from the Board, got:\n%s", view)
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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
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
	if !strings.Contains(view, "      ▲ "+root.ID+" Extract config loader") {
		t.Fatalf("expected Digest-style rows indented under their Wave, got:\n%s", view)
	}
	if !strings.Contains(view, "⊘ "+root.ID) || !strings.Contains(view, "refactor") {
		t.Fatalf("expected rows to keep blocker markers and labels, got:\n%s", view)
	}

	batches, err := st.ListBatches(project)
	if err != nil {
		t.Fatalf("list batches: %v", err)
	}
	date := batchCreatedDate(batches[0].Created)
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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
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

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
	view := current.View()

	if !strings.Contains(view, "0 batches   batch-empty-app") {
		t.Fatalf("expected empty Batches header, got:\n%s", view)
	}
	if !strings.Contains(view, "no Batches yet") || !strings.Contains(view, "run ito batch new <name> to plan one") {
		t.Fatalf("expected an actionable ito batch new hint, got:\n%s", view)
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
	current, _ = current.Update(keyMsg(t, "3"))
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

func TestBatchesSurfaceIgnoresEditAndSelectionKeysReadOnly(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("batch-readonly-app", "BRO", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateBatch(project, "quiet-effort"); err != nil {
		t.Fatalf("create batch: %v", err)
	}
	member, err := st.CreateIssueInBatch(project, "Stay untouched", "todo", "medium", nil, "", "quiet-effort")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "3"))
	for _, key := range []string{"down", "up", "h", "s", "enter", ":"} {
		current, _ = current.Update(keyMsg(t, key))
	}
	view := current.View()
	if !strings.Contains(view, "quiet-effort  (1)") || strings.Contains(view, "esc cancel") {
		t.Fatalf("expected read-only Batches surface to stay put, got:\n%s", view)
	}
	unchanged, err := st.FindIssue(project, member.ID)
	if err != nil {
		t.Fatalf("find member: %v", err)
	}
	if unchanged.Status != "todo" {
		t.Fatalf("expected Batches keys to leave the store untouched, got status %q", unchanged.Status)
	}
}
