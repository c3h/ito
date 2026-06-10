package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

func TestDigestRendersIssuesGroupedByStatus(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("digest-app", "DIG", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	otherProject, err := st.CreateProject("other-app", "OTH", t.TempDir())
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog research", "backlog", "medium", []string{"research"}, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	blocker, err := st.CreateIssue(project, "Unblock the TUI", "todo", "high", []string{"feature"}, "")
	if err != nil {
		t.Fatalf("create blocker issue: %v", err)
	}
	blocked, err := st.CreateIssue(project, "Blocked Digest row", "in_progress", "urgent", []string{"feature", "tests"}, "")
	if err != nil {
		t.Fatalf("create blocked issue: %v", err)
	}
	if _, err := st.Edit(project, blocked.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{{Kind: "blocked_by", Action: "add", Target: blocker.ID}},
	}); err != nil {
		t.Fatalf("block issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Review polish", "in_review", "low", []string{"chore"}, ""); err != nil {
		t.Fatalf("create in review issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done still appears", "done", "low", []string{"docs"}, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}
	if _, err := st.CreateIssue(otherProject, "Other project issue", "todo", "urgent", []string{"bug"}, ""); err != nil {
		t.Fatalf("create other project issue: %v", err)
	}

	view := newModel(st, project).View()
	for _, want := range []string{
		"ito · [1] digest · [2] board",
		"5 issues   digest-app",
		"BACKLOG  (1)",
		"TODO  (1)",
		"IN PROGRESS  (1)",
		"IN REVIEW  (1)",
		"▸ DONE  (1) · h to show",
		"◆ DIG-1 Backlog research",
		"▲ DIG-2 Unblock the TUI",
		"● DIG-3 Blocked Digest row",
		"feature tests",
		"⊘ DIG-2",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected Digest to contain %q, got %q", want, view)
		}
	}
	if strings.Contains(view, "DIG-5 Done still appears") {
		t.Fatalf("Digest should start with done collapsed, got:\n%s", view)
	}
	if strings.Contains(view, "Other project issue") {
		t.Fatalf("Digest included an Issue from another Project:\n%s", view)
	}
}

func TestNumberKeysSwitchBetweenDigestAndBoard(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-switch-app", "BSW", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Visible on both surfaces", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	board := current.View()
	if !strings.Contains(board, "ito · [1] digest · [2] board") || !strings.Contains(board, "TODO  (1)") {
		t.Fatalf("expected [2] to switch to the Board, got:\n%s", board)
	}
	if strings.Contains(board, "h hide") {
		t.Fatalf("expected Board shortcuts to omit Digest-only hide action, got:\n%s", board)
	}

	current, _ = current.Update(keyMsg(t, "1"))
	digest := current.View()
	if !strings.Contains(digest, "ito · [1] digest · [2] board") || !strings.Contains(digest, "h hide") {
		t.Fatalf("expected [1] to switch back to the Digest, got:\n%s", digest)
	}
}

func TestBoardRenderDoesNotRevealHiddenDigestSections(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-hidden-app", "BHD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog can hide", "backlog", "medium", nil, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done stays hidden", "done", "low", nil, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "h"))
	hiddenDigest := current.View()
	if !strings.Contains(hiddenDigest, "▌▸ BACKLOG  (1) · h to show") ||
		!strings.Contains(hiddenDigest, "▸ DONE  (1) · h to show") {
		t.Fatalf("expected Digest setup to have hidden BACKLOG and DONE, got:\n%s", hiddenDigest)
	}

	current, _ = current.Update(keyMsg(t, "2"))
	board := current.View()
	if !strings.Contains(board, "BACKLOG  (1)") || !strings.Contains(board, "DONE  (1)") {
		t.Fatalf("expected Board to render all statuses, got:\n%s", board)
	}

	current, _ = current.Update(keyMsg(t, "1"))
	digest := current.View()
	if !strings.Contains(digest, "▌▸ BACKLOG  (1) · h to show") ||
		!strings.Contains(digest, "▸ DONE  (1) · h to show") {
		t.Fatalf("expected Digest hidden sections to survive rendered Board switch, got:\n%s", digest)
	}
}

func TestBoardRendersAllStatusesAsColumns(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-app", "BRD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog research", "backlog", "medium", []string{"research"}, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Todo feature with a title long enough to truncate in the board column", "todo", "high", []string{"feature"}, ""); err != nil {
		t.Fatalf("create todo issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Active refactor", "in_progress", "urgent", []string{"refactor"}, ""); err != nil {
		t.Fatalf("create in progress issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Review docs", "in_review", "low", []string{"docs"}, ""); err != nil {
		t.Fatalf("create in review issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done still visible on Board", "done", "low", []string{"tests"}, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 140, Height: 24})
	current, _ = current.Update(keyMsg(t, "2"))
	board := current.View()

	for _, want := range []string{
		"▌BACKLOG  (1)",
		"TODO  (1)",
		"IN PROGRESS  (1)",
		"IN REVIEW  (1)",
		"DONE  (1)",
		"▸ ◆ BRD-1 Backlog",
		"▲ BRD-2 Todo feature",
		"● BRD-3 Active",
		"· BRD-4 Review docs",
		"· BRD-5 Done still",
	} {
		if !strings.Contains(board, want) {
			t.Fatalf("expected Board to contain %q, got:\n%s", want, board)
		}
	}
	if strings.Contains(board, "h to show") || strings.Contains(board, "▸ DONE") {
		t.Fatalf("expected Board to always show DONE instead of Digest hide chrome, got:\n%s", board)
	}
}

func TestBoardSlidesHorizontallyToKeepFocusedStatusVisible(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-slide-app", "BSL", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, status := range store.Statuses {
		if _, err := st.CreateIssue(project, "Issue in "+status, status, "medium", nil, ""); err != nil {
			t.Fatalf("create %s issue: %v", status, err)
		}
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 20})
	current, _ = current.Update(keyMsg(t, "2"))
	firstWindow := current.View()
	if !strings.Contains(firstWindow, "BACKLOG  (1)") || !strings.Contains(firstWindow, "TODO  (1)") || !strings.Contains(firstWindow, "IN PROGRESS  (1)") {
		t.Fatalf("expected narrow Board to start on the first visible statuses, got:\n%s", firstWindow)
	}
	if !strings.Contains(firstWindow, "›") || strings.Contains(firstWindow, "‹") || strings.Contains(firstWindow, "DONE  (1)") {
		t.Fatalf("expected narrow Board to expose only the right overflow at first, got:\n%s", firstWindow)
	}

	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "tab"))
	middleWindow := current.View()
	if !strings.Contains(middleWindow, "‹") || !strings.Contains(middleWindow, "›") || !strings.Contains(middleWindow, "▌IN PROGRESS  (1)") {
		t.Fatalf("expected Board to slide around the focused IN PROGRESS column, got:\n%s", middleWindow)
	}
	if strings.Contains(middleWindow, "BACKLOG  (1)") || strings.Contains(middleWindow, "DONE  (1)") {
		t.Fatalf("expected middle Board window to omit off-track statuses, got:\n%s", middleWindow)
	}

	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "tab"))
	lastWindow := current.View()
	if !strings.Contains(lastWindow, "‹") || strings.Contains(lastWindow, "›") || !strings.Contains(lastWindow, "▌DONE  (1)") {
		t.Fatalf("expected Board to slide to the final column without right overflow, got:\n%s", lastWindow)
	}
}

func TestBoardSharesInlineFilterAndStatusEditWithDigestState(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-shared-app", "BSH", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	target, err := st.CreateIssue(project, "Move this Board Issue", "backlog", "medium", []string{"feature"}, "")
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done docs match filter", "done", "low", []string{"docs"}, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	current, _ = current.Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "/"))
	for _, r := range "docs" {
		current, _ = current.Update(runeMsg(r))
	}
	filtered := current.View()
	if !strings.Contains(filtered, " / docs▏") || !strings.Contains(filtered, "1 of 2 issues · esc to clear") {
		t.Fatalf("expected Board filter input and counts, got:\n%s", filtered)
	}
	if !strings.Contains(filtered, "DONE  (1)") || !strings.Contains(filtered, "BSH-2") || strings.Contains(filtered, "BSH-1") {
		t.Fatalf("expected Board filter to reveal matching DONE Issue and hide non-matches, got:\n%s", filtered)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	current, _ = current.Update(keyMsg(t, "s"))
	moved, err := st.FindIssue(project, target.ID)
	if err != nil {
		t.Fatalf("find moved issue: %v", err)
	}
	if moved.Status != "todo" {
		t.Fatalf("expected Board status edit to move Issue through the store, got %q", moved.Status)
	}
	view := current.View()
	if !strings.Contains(view, "BACKLOG  (0)") || !strings.Contains(view, "TODO  (1)") || !strings.Contains(view, "▌TODO  (1)") {
		t.Fatalf("expected Board to reload counts and focus the moved Issue, got:\n%s", view)
	}
	if !strings.Contains(view, "▸ ◆ "+target.ID) {
		t.Fatalf("expected moved Issue to stay selected on the Board, got:\n%s", view)
	}
}

func TestIssueDetailReturnsToBoardWhenOpenedFromBoard(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-detail-app", "BDT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Open detail from Board", "backlog", "medium", nil, "body")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "ito · "+issue.ID+" · Open detail from Board") {
		t.Fatalf("expected Board selection to open Issue detail, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	board := current.View()
	if !strings.Contains(board, "BACKLOG  (1)") || !strings.Contains(board, "▸ ◆ "+issue.ID) {
		t.Fatalf("expected Esc to return to the Board selection, got:\n%s", board)
	}
	if strings.Contains(board, "h hide") {
		t.Fatalf("expected Esc from Board detail to return to Board shortcuts, got:\n%s", board)
	}
}

func TestDigestQuitsOnQAndCtrlC(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("quit-app", "QUT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			_, cmd := newModel(st, project).Update(keyMsg(t, key))
			if cmd == nil {
				t.Fatalf("expected %s to return a quit command", key)
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("expected %s to return tea.QuitMsg, got %T", key, cmd())
			}
		})
	}
}

func TestDigestNavigationMovesFocusAndSelection(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("nav-app", "NAV", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog issue", "backlog", "medium", []string{"research"}, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "First todo", "todo", "high", []string{"feature"}, ""); err != nil {
		t.Fatalf("create first todo: %v", err)
	}
	if _, err := st.CreateIssue(project, "Second todo", "todo", "low", []string{"tests"}, ""); err != nil {
		t.Fatalf("create second todo: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "down"))
	view := current.View()

	if !strings.Contains(view, " ▌▾ TODO  (2)") {
		t.Fatalf("expected TODO section to be focused after Tab, got:\n%s", view)
	}
	if !strings.Contains(view, "   ▲ NAV-2 First todo") {
		t.Fatalf("expected first TODO Issue to no longer be selected after Down, got:\n%s", view)
	}
	if !strings.Contains(view, " ▸ · NAV-3 Second todo") {
		t.Fatalf("expected second TODO Issue to be selected after Down, got:\n%s", view)
	}
}

func TestDigestHidesAndRevealsFocusedSection(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("hide-app", "HID", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog can hide", "backlog", "medium", nil, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done can reveal", "done", "low", nil, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "h"))
	hiddenBacklog := current.View()
	if !strings.Contains(hiddenBacklog, "▌▸ BACKLOG  (1) · h to show") {
		t.Fatalf("expected h to collapse the focused BACKLOG section, got:\n%s", hiddenBacklog)
	}
	if strings.Contains(hiddenBacklog, "HID-1 Backlog can hide") {
		t.Fatalf("expected collapsed BACKLOG to hide its Issue row, got:\n%s", hiddenBacklog)
	}
	hiddenBacklogLines := strings.Split(hiddenBacklog, "\n")
	collapsedBacklogLine := slices.IndexFunc(hiddenBacklogLines, func(line string) bool {
		return strings.Contains(line, "BACKLOG") && strings.Contains(line, "h to show")
	})
	if collapsedBacklogLine == -1 || collapsedBacklogLine+2 >= len(hiddenBacklogLines) ||
		hiddenBacklogLines[collapsedBacklogLine+1] != "" ||
		!strings.Contains(hiddenBacklogLines[collapsedBacklogLine+2], "▾ TODO") {
		t.Fatalf("expected collapsed BACKLOG to preserve section spacing before TODO, got:\n%s", hiddenBacklog)
	}

	current, _ = current.Update(keyMsg(t, "h"))
	revealedBacklog := current.View()
	if !strings.Contains(revealedBacklog, "▌▾ BACKLOG  (1)") || !strings.Contains(revealedBacklog, "HID-1 Backlog can hide") {
		t.Fatalf("expected h to reveal the focused BACKLOG section, got:\n%s", revealedBacklog)
	}

	for i := 0; i < 4; i++ {
		current, _ = current.Update(keyMsg(t, "tab"))
	}
	focusedDone := current.View()
	if !strings.Contains(focusedDone, "▌▸ DONE  (1) · h to show") {
		t.Fatalf("expected collapsed DONE section to be focusable, got:\n%s", focusedDone)
	}

	current, _ = current.Update(keyMsg(t, "h"))
	revealedDone := current.View()
	if !strings.Contains(revealedDone, "▌▾ DONE  (1)") || !strings.Contains(revealedDone, "HID-2 Done can reveal") {
		t.Fatalf("expected h to reveal focused DONE section, got:\n%s", revealedDone)
	}
}

func TestDigestSlashOpensInlineFilterAndEscRestoresShortcuts(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("filter-app", "FLT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Keep the surface visible", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "/"))
	filtering := current.View()
	if !strings.Contains(filtering, "TODO  (1)") || !strings.Contains(filtering, "FLT-1 Keep the surface visible") {
		t.Fatalf("expected / to keep the Digest surface visible, got:\n%s", filtering)
	}
	if !strings.Contains(filtering, " / ▏") || !strings.Contains(filtering, "1 of 1 issues · esc to clear") {
		t.Fatalf("expected / to replace shortcuts with inline filter input, got:\n%s", filtering)
	}
	if strings.Contains(filtering, "tab focus") {
		t.Fatalf("expected filter input to replace the shortcut bar, got:\n%s", filtering)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	restored := current.View()
	if !strings.Contains(restored, "tab focus   ↑↓ select") {
		t.Fatalf("expected Esc to restore the shortcut bar, got:\n%s", restored)
	}
	if strings.Contains(restored, "esc to clear") {
		t.Fatalf("expected Esc to leave the filter input, got:\n%s", restored)
	}
}

func TestDigestInlineFilterNarrowsLiveByIDTitleAndLabels(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("filter-match-app", "FMT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Render digest rows", "todo", "medium", []string{"feature"}, ""); err != nil {
		t.Fatalf("create digest issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Fix search matching", "todo", "high", []string{"bug"}, ""); err != nil {
		t.Fatalf("create search issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Write docs", "in_progress", "low", []string{"docs"}, ""); err != nil {
		t.Fatalf("create docs issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "/"))
	for _, r := range "bug" {
		current, _ = current.Update(runeMsg(r))
	}
	labelMatch := current.View()
	if !strings.Contains(labelMatch, " / bug▏") || !strings.Contains(labelMatch, "1 of 3 issues · esc to clear") {
		t.Fatalf("expected typed query to appear in filter input, got:\n%s", labelMatch)
	}
	if !strings.Contains(labelMatch, "▲ FMT-2 Fix search matching") || strings.Contains(labelMatch, "FMT-1 Render digest rows") || strings.Contains(labelMatch, "FMT-3 Write docs") {
		t.Fatalf("expected query to narrow by Label, got:\n%s", labelMatch)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	current, _ = current.Update(keyMsg(t, "/"))
	for _, r := range "FMT-3" {
		current, _ = current.Update(runeMsg(r))
	}
	idMatch := current.View()
	if !strings.Contains(idMatch, "· FMT-3 Write docs") || strings.Contains(idMatch, "FMT-1 Render digest rows") || strings.Contains(idMatch, "FMT-2 Fix search matching") {
		t.Fatalf("expected query to narrow by ID, got:\n%s", idMatch)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	current, _ = current.Update(keyMsg(t, "/"))
	for _, r := range "digest" {
		current, _ = current.Update(runeMsg(r))
	}
	titleMatch := current.View()
	if !strings.Contains(titleMatch, "◆ FMT-1 Render digest rows") || strings.Contains(titleMatch, "FMT-2 Fix search matching") || strings.Contains(titleMatch, "FMT-3 Write docs") {
		t.Fatalf("expected query to narrow by Title, got:\n%s", titleMatch)
	}

	current, _ = current.Update(keyMsg(t, "s"))
	unchanged, err := st.FindIssue(project, "FMT-1")
	if err != nil {
		t.Fatalf("find issue after typing s: %v", err)
	}
	if unchanged.Status != "todo" {
		t.Fatalf("expected typing in the filter to be read-only, got status %q", unchanged.Status)
	}
}

func TestDigestInlineFilterAcceptsSpaceKey(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("filter-space-app", "FSP", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Two word title", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create matching issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Two unrelated", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create non-matching issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "/"))
	for _, r := range "Two" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "space"))
	for _, r := range "word" {
		current, _ = current.Update(runeMsg(r))
	}

	view := current.View()
	if !strings.Contains(view, " / Two word▏") {
		t.Fatalf("expected Space to appear in the filter query, got:\n%s", view)
	}
	if !strings.Contains(view, "FSP-1 Two word title") || strings.Contains(view, "FSP-2 Two unrelated") {
		t.Fatalf("expected spaced query to narrow by Title, got:\n%s", view)
	}
}

func TestDigestInlineFilterRevealsMatchesInHiddenSections(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("filter-hidden-app", "FHD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Open issue", "todo", "medium", []string{"feature"}, ""); err != nil {
		t.Fatalf("create open issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Completed docs", "done", "low", []string{"docs"}, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "/"))
	for _, r := range "docs" {
		current, _ = current.Update(runeMsg(r))
	}
	view := current.View()
	if !strings.Contains(view, "▾ DONE  (1)") || !strings.Contains(view, "· FHD-2 Completed docs") {
		t.Fatalf("expected filter to reveal matching Issues in hidden DONE, got:\n%s", view)
	}
	if strings.Contains(view, "▸ DONE  (1) · h to show") {
		t.Fatalf("expected matching hidden section to be expanded while filtering, got:\n%s", view)
	}
	if strings.Contains(view, "FHD-1 Open issue") {
		t.Fatalf("expected filter to keep non-matching Issues hidden, got:\n%s", view)
	}
	if strings.Contains(view, "TODO") || strings.Contains(view, "no Issues") {
		t.Fatalf("expected filter to omit sections with no matches, got:\n%s", view)
	}
}

func TestCommandLineOpensInlineAndEscRestoresShortcuts(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("command-app", "CMD", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Keep Digest under command line", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, ":"))
	commanding := current.View()
	if !strings.Contains(commanding, "TODO  (1)") || !strings.Contains(commanding, "CMD-1 Keep Digest under command line") {
		t.Fatalf("expected : to keep the Digest surface visible, got:\n%s", commanding)
	}
	if !strings.Contains(commanding, " : ▏") || !strings.Contains(commanding, "esc cancel") {
		t.Fatalf("expected : to replace shortcuts with inline command input, got:\n%s", commanding)
	}
	if strings.Contains(commanding, "tab focus") {
		t.Fatalf("expected command input to replace the shortcut bar, got:\n%s", commanding)
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	restored := current.View()
	if !strings.Contains(restored, "tab focus   ↑↓ select") {
		t.Fatalf("expected Esc to restore the shortcut bar, got:\n%s", restored)
	}
	if strings.Contains(restored, "esc cancel") {
		t.Fatalf("expected Esc to leave the command input, got:\n%s", restored)
	}
}

func TestCommandLineFiltersClosedV2ActionSet(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("command-actions-app", "CMA", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Command target", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, ":"))
	actions := current.View()
	for _, want := range []string{
		"─ actions ",
		"s  status",
		"p  priority",
		"l  labels",
		"switch project",
		"r  refresh",
		"q  quit",
	} {
		if !strings.Contains(actions, want) {
			t.Fatalf("expected command action list to contain %q, got:\n%s", want, actions)
		}
	}
	for _, forbidden := range []string{"create", "title", "body", "links"} {
		if strings.Contains(strings.ToLower(actions), forbidden) {
			t.Fatalf("expected command action list to exclude v3 action %q, got:\n%s", forbidden, actions)
		}
	}

	for _, r := range "lab" {
		current, _ = current.Update(runeMsg(r))
	}
	filtered := current.View()
	if !strings.Contains(filtered, " : lab▏") || !strings.Contains(filtered, "l  labels") {
		t.Fatalf("expected command query to filter to Labels, got:\n%s", filtered)
	}
	if strings.Contains(filtered, "s  status") || strings.Contains(filtered, "p  priority") || strings.Contains(filtered, "q  quit") {
		t.Fatalf("expected command filter to hide non-matching actions, got:\n%s", filtered)
	}
}

func TestCommandLineSelectionRunsActions(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("command-run-app", "CMR", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Run selected command", "backlog", "medium", nil, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, ":"))
	for _, r := range "quit" {
		current, _ = current.Update(runeMsg(r))
	}
	_, cmd := current.Update(keyMsg(t, "enter"))
	if cmd == nil {
		t.Fatalf("expected selected quit command to return a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected selected quit command to return tea.QuitMsg, got %T", cmd())
	}

	current, _ = newModel(st, project).Update(keyMsg(t, ":"))
	for _, r := range "status" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	moved, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find moved issue: %v", err)
	}
	if moved.Status != "todo" {
		t.Fatalf("expected selected status command to move Issue to todo through the store, got %q", moved.Status)
	}
	if strings.Contains(current.View(), "esc cancel") {
		t.Fatalf("expected selected status command to close the command line, got:\n%s", current.View())
	}

	current, _ = newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, ":"))
	for _, r := range "priority" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	prioritized, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find prioritized issue: %v", err)
	}
	if prioritized.Priority != "high" {
		t.Fatalf("expected selected priority command to cycle Issue priority, got %q", prioritized.Priority)
	}

	current, _ = newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, ":"))
	for _, r := range "labels" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "[ ] "+store.Labels[0]) || !strings.Contains(view, "⏎ toggle") {
		t.Fatalf("expected selected labels command to open the label picker for the selected Issue, got:\n%s", view)
	}
}

func TestSwitchProjectCommandOpensPickerAndRescopesDigest(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	alpha, err := st.CreateProject("alpha-app", "ALP", t.TempDir())
	if err != nil {
		t.Fatalf("create alpha project: %v", err)
	}
	beta, err := st.CreateProject("beta-app", "BET", t.TempDir())
	if err != nil {
		t.Fatalf("create beta project: %v", err)
	}
	if _, err := st.CreateIssue(alpha, "Alpha scoped issue", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create alpha issue: %v", err)
	}
	if _, err := st.CreateIssue(beta, "Beta scoped issue", "todo", "high", nil, ""); err != nil {
		t.Fatalf("create beta issue: %v", err)
	}

	current, _ := newModel(st, alpha).Update(keyMsg(t, ":"))
	for _, r := range "switch" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	picker := current.View()
	if !strings.Contains(picker, "ito · switch project") || !strings.Contains(picker, "▸ alpha-app   ALP") || !strings.Contains(picker, "  beta-app    BET") {
		t.Fatalf("expected switch project command to open a Project picker, got:\n%s", picker)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	current, _ = current.Update(keyMsg(t, "enter"))
	view := current.View()
	if !strings.Contains(view, "1 issues   beta-app") || !strings.Contains(view, "▲ BET-1 Beta scoped issue") {
		t.Fatalf("expected selecting beta-app to reload the Digest in that Project, got:\n%s", view)
	}
	if strings.Contains(view, "Alpha scoped issue") || strings.Contains(view, "esc cancel") {
		t.Fatalf("expected Project switch to close picker/command and leave alpha issues behind, got:\n%s", view)
	}
}

func TestProjectPickerOpensWhenModelStartsWithoutProject(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	alpha, err := st.CreateProject("alpha-app", "ALP", t.TempDir())
	if err != nil {
		t.Fatalf("create alpha project: %v", err)
	}
	if _, err := st.CreateProject("beta-app", "BET", t.TempDir()); err != nil {
		t.Fatalf("create beta project: %v", err)
	}
	if _, err := st.CreateIssue(alpha, "Alpha issue", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create alpha issue: %v", err)
	}

	current := newModel(st, store.Project{})
	view := current.View()
	if !strings.Contains(view, "ito · switch project") || !strings.Contains(view, "▸ alpha-app   ALP") || !strings.Contains(view, "  beta-app    BET") {
		t.Fatalf("expected missing initial Project to open the Project picker, got:\n%s", view)
	}

	currentAfterSelect, _ := current.Update(keyMsg(t, "enter"))
	selected := currentAfterSelect.View()
	if !strings.Contains(selected, "1 issues   alpha-app") || !strings.Contains(selected, "◆ ALP-1 Alpha issue") {
		t.Fatalf("expected selecting a Project from the startup picker to load its Digest, got:\n%s", selected)
	}
}

func TestProjectPickerShowsInitHintWhenStoreIsEmpty(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	current := newModel(store.New(db), store.Project{})
	view := current.View()
	if !strings.Contains(view, "ito · switch project") || !strings.Contains(view, "run ito init to get started") {
		t.Fatalf("expected empty store to show the init hint, got:\n%s", view)
	}

	currentAfterEsc, _ := current.Update(keyMsg(t, "esc"))
	if view := currentAfterEsc.View(); !strings.Contains(view, "run ito init to get started") {
		t.Fatalf("expected Esc without an initial Project to keep the init hint visible, got:\n%s", view)
	}
}

func TestIssueDetailCommandLineRunsLongTailActionsInline(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("detail-command-app", "DCA", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Priority from command", "todo", "low", nil, "body")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "enter"))
	current, _ = current.Update(keyMsg(t, ":"))
	commanding := current.View()
	if !strings.Contains(commanding, "ito · "+issue.ID+" · Priority from command") {
		t.Fatalf("expected : to keep the Issue detail visible, got:\n%s", commanding)
	}
	if !strings.Contains(commanding, " : ▏") || !strings.Contains(commanding, "p  priority") || strings.Contains(commanding, "esc back") {
		t.Fatalf("expected : to replace Issue detail shortcuts with command input, got:\n%s", commanding)
	}

	for _, r := range "priority" {
		current, _ = current.Update(runeMsg(r))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	edited, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find edited issue: %v", err)
	}
	if edited.Priority != "medium" {
		t.Fatalf("expected priority command to cycle low priority to medium, got %q", edited.Priority)
	}
	if !strings.Contains(current.View(), "todo   ·   medium") || strings.Contains(current.View(), "esc cancel") {
		t.Fatalf("expected priority command to close and refresh Issue detail, got:\n%s", current.View())
	}
}

func TestRefreshShortcutAndCommandReloadDigest(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("refresh-app", "RFR", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	current := newModel(st, project)
	if _, err := st.CreateIssue(project, "External write", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create external issue: %v", err)
	}

	if view := current.View(); strings.Contains(view, "External write") {
		t.Fatalf("expected model snapshot to stay stale before refresh, got:\n%s", view)
	}

	refreshedByKey, _ := current.Update(keyMsg(t, "r"))
	if view := refreshedByKey.View(); !strings.Contains(view, "RFR-1 External write") {
		t.Fatalf("expected r shortcut to reload Digest, got:\n%s", view)
	}

	if _, err := st.CreateIssue(project, "Second external write", "todo", "high", nil, ""); err != nil {
		t.Fatalf("create second external issue: %v", err)
	}
	refreshedByCommand, _ := refreshedByKey.Update(keyMsg(t, ":"))
	for _, r := range "refresh" {
		refreshedByCommand, _ = refreshedByCommand.Update(runeMsg(r))
	}
	refreshedByCommand, _ = refreshedByCommand.Update(keyMsg(t, "enter"))
	if view := refreshedByCommand.View(); !strings.Contains(view, "RFR-2 Second external write") || strings.Contains(view, "esc cancel") {
		t.Fatalf("expected refresh command to reload Digest and close command line, got:\n%s", view)
	}
}

func TestRefreshPreservesSelectedIssueWhenItStillExists(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("refresh-selection-app", "RFS", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Backlog issue", "backlog", "medium", nil, ""); err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "First todo issue", "todo", "high", nil, ""); err != nil {
		t.Fatalf("create first todo issue: %v", err)
	}
	selected, err := st.CreateIssue(project, "Keep this selected", "todo", "low", nil, "")
	if err != nil {
		t.Fatalf("create selected issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, " ▸ · "+selected.ID+" Keep this selected") {
		t.Fatalf("expected TODO issue to be selected before refresh, got:\n%s", view)
	}

	if _, err := st.CreateIssue(project, "External write", "todo", "medium", nil, ""); err != nil {
		t.Fatalf("create external issue: %v", err)
	}
	current, _ = current.Update(keyMsg(t, "r"))
	view := current.View()

	if !strings.Contains(view, "TODO  (3)") || !strings.Contains(view, "RFS-4 External write") {
		t.Fatalf("expected refresh to reload externally-written Issues, got:\n%s", view)
	}
	if !strings.Contains(view, " ▌▾ TODO  (3)") || !strings.Contains(view, " ▸ · "+selected.ID+" Keep this selected") {
		t.Fatalf("expected refresh to preserve focus and selection, got:\n%s", view)
	}
}

func TestStatusKeyMovesSelectedIssueThroughStoreAndReloadsDigest(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("status-edit-app", "SED", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Move from the TUI", "backlog", "medium", []string{"feature"}, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "s"))
	moved, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find moved issue: %v", err)
	}
	if moved.Status != "todo" {
		t.Fatalf("expected s to move Issue to todo through the store, got %q", moved.Status)
	}

	view := current.View()
	if !strings.Contains(view, "BACKLOG  (0)") || !strings.Contains(view, "TODO  (1)") {
		t.Fatalf("expected Digest to reload counts after status edit, got:\n%s", view)
	}
	if !strings.Contains(view, " ▸ ◆ "+issue.ID+" Move from the TUI") {
		t.Fatalf("expected moved Issue to stay selected after reload, got:\n%s", view)
	}
}

func TestIssueDetailPriorityKeyCyclesPriorityThroughStore(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("priority-edit-app", "PED", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Cycle priority from detail", "todo", "low", []string{"feature"}, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "enter"))
	current, _ = current.Update(keyMsg(t, "p"))
	edited, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find edited issue: %v", err)
	}
	if edited.Priority != "medium" {
		t.Fatalf("expected p to cycle low priority to medium through the store, got %q", edited.Priority)
	}

	view := current.View()
	if !strings.Contains(view, "todo   ·   medium   ·   feature") {
		t.Fatalf("expected Issue detail to reload edited priority, got:\n%s", view)
	}
	if !strings.Contains(view, "p priority") {
		t.Fatalf("expected Issue detail shortcuts to expose priority edit, got:\n%s", view)
	}
}

func TestIssueDetailLabelPickerTogglesChosenLabelsThroughStore(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("label-edit-app", "LED", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Toggle labels from detail", "todo", "medium", nil, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "enter"))

	// l opens a picker over the fixed vocabulary, not a blind toggle of one label.
	current, _ = current.Update(keyMsg(t, "l"))
	picker := current.View()
	for _, want := range []string{"[ ] " + store.Labels[0], "[ ] " + store.Labels[1], "⏎ toggle"} {
		if !strings.Contains(picker, want) {
			t.Fatalf("expected label picker to list the vocabulary, got:\n%s", picker)
		}
	}

	// enter toggles the focused (first) label on through the store.
	current, _ = current.Update(keyMsg(t, "enter"))
	edited, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find edited issue: %v", err)
	}
	if !slices.Equal(edited.Labels, []string{store.Labels[0]}) {
		t.Fatalf("expected enter to add the focused Label through the store, got %#v", edited.Labels)
	}
	if !strings.Contains(current.View(), "[x] "+store.Labels[0]) {
		t.Fatalf("expected picker to reflect the added label, got:\n%s", current.View())
	}

	// down picks a different label from the vocabulary; enter adds that one too.
	current, _ = current.Update(keyMsg(t, "down"))
	current, _ = current.Update(keyMsg(t, "enter"))
	edited, err = st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find edited issue after second toggle: %v", err)
	}
	if !slices.Contains(edited.Labels, store.Labels[1]) || !slices.Contains(edited.Labels, store.Labels[0]) {
		t.Fatalf("expected the chosen non-first Label to be added alongside the first, got %#v", edited.Labels)
	}

	// enter again on the focused label removes it.
	current, _ = current.Update(keyMsg(t, "enter"))
	edited, err = st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("find edited issue after toggle off: %v", err)
	}
	if slices.Contains(edited.Labels, store.Labels[1]) {
		t.Fatalf("expected enter to remove the focused Label, got %#v", edited.Labels)
	}

	// esc leaves the picker back to the read-only detail.
	if current, _ = current.Update(keyMsg(t, "esc")); !strings.Contains(current.View(), "esc back   ↑↓ prev/next") {
		t.Fatalf("expected esc to return to the Issue detail, got:\n%s", current.View())
	}
}

func TestDigestViewportUsesTerminalHeightWithoutFixedItemCap(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("viewport-app", "VPT", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for i := 1; i <= 8; i++ {
		if _, err := st.CreateIssue(project, "Todo viewport issue "+string(rune('0'+i)), "todo", "medium", nil, ""); err != nil {
			t.Fatalf("create issue %d: %v", i, err)
		}
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 16})
	small := current.View()
	current, _ = current.Update(tea.WindowSizeMsg{Width: 88, Height: 30})
	tall := current.View()

	if strings.Contains(small, "VPT-8 Todo viewport issue 8") {
		t.Fatalf("expected small Digest viewport to omit the last TODO issue, got:\n%s", small)
	}
	if !strings.Contains(tall, "VPT-8 Todo viewport issue 8") {
		t.Fatalf("expected taller Digest viewport to show more TODO issues, got:\n%s", tall)
	}
}

func TestDigestOverflowShowsMoreIndicatorsAndKeepsSelectionVisible(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("overflow-app", "OVR", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for i := 1; i <= 8; i++ {
		if _, err := st.CreateIssue(project, "Scrollable issue "+string(rune('0'+i)), "todo", "medium", nil, ""); err != nil {
			t.Fatalf("create issue %d: %v", i, err)
		}
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 19})
	current, _ = current.Update(keyMsg(t, "tab"))
	for i := 0; i < 4; i++ {
		current, _ = current.Update(keyMsg(t, "down"))
	}
	view := current.View()

	// The window centers on the selection, so the picked Issue keeps a neighbour
	// above and below it inside the viewport, with the overflow split either side.
	for _, want := range []string{
		"↑ 3 more",
		"OVR-4 Scrollable issue 4",
		" ▸ ◆ OVR-5 Scrollable issue 5",
		"↓ 3 more",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected scrolled Digest viewport to contain %q, got:\n%s", want, view)
		}
	}
	if strings.Contains(view, "OVR-1 Scrollable issue 1") {
		t.Fatalf("expected scrolled Digest viewport to omit Issues above the window, got:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 23 {
		t.Fatalf("expected Digest viewport to fit the terminal height, got %d lines:\n%s", lines, view)
	}
}

func TestIssueDetailOpensSelectedIssueAndReturnsToDigest(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("detail-app", "DET", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	blocker, err := st.CreateIssue(project, "Extract store read path", "done", "high", []string{"refactor"}, "done blocker")
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	related, err := st.CreateIssue(project, "Render the Board later", "backlog", "medium", []string{"feature"}, "related body")
	if err != nil {
		t.Fatalf("create related issue: %v", err)
	}
	target, err := st.CreateIssue(project, "Read-only detail view with a title long enough to truncate in the header", "todo", "urgent", []string{"feature", "docs"}, "## Context\nShow the full markdown body.\n\n## Acceptance\n- read-only")
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}
	if _, err := st.Edit(project, target.ID, store.EditIssueOptions{
		LinkOps: []store.LinkEditOp{
			{Kind: "blocked_by", Action: "add", Target: blocker.ID},
			{Kind: "relates_to", Action: "add", Target: related.ID},
		},
	}); err != nil {
		t.Fatalf("link target issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "enter"))
	detail := current.View()

	for _, want := range []string{
		"ito · " + target.ID + " · Read-only detail view",
		"todo   ·   urgent   ·   docs  feature",
		"blocked by   " + blocker.ID + "   Extract store read path",
		"relates to   " + related.ID + "   Render the Board later",
		"created      ",
		"updated      ",
		"## Context",
		"Show the full markdown body.",
		"## Acceptance",
		"esc back   ↑↓ prev/next   s status   p priority   l labels",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("expected Issue detail to contain %q, got:\n%s", want, detail)
		}
	}

	current, _ = current.Update(keyMsg(t, "esc"))
	digest := current.View()
	if !strings.Contains(digest, "TODO  (1)") || !strings.Contains(digest, " ▸ ● "+target.ID) {
		t.Fatalf("expected Esc to return to the Digest selection, got:\n%s", digest)
	}
}

func TestIssueDetailUpDownMovesBetweenIssues(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("detail-nav-app", "DNV", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	first, err := st.CreateIssue(project, "Backlog first", "backlog", "medium", nil, "first body")
	if err != nil {
		t.Fatalf("create first issue: %v", err)
	}
	second, err := st.CreateIssue(project, "Todo second", "todo", "high", nil, "second body")
	if err != nil {
		t.Fatalf("create second issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "ito · "+first.ID+" · Backlog first") {
		t.Fatalf("expected first Issue detail, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "ito · "+second.ID+" · Todo second") || !strings.Contains(view, "second body") {
		t.Fatalf("expected Down to show next Issue detail, got:\n%s", view)
	}

	current, _ = current.Update(keyMsg(t, "up"))
	if view := current.View(); !strings.Contains(view, "ito · "+first.ID+" · Backlog first") || !strings.Contains(view, "first body") {
		t.Fatalf("expected Up to show previous Issue detail, got:\n%s", view)
	}
}

func keyMsg(t *testing.T, key string) tea.KeyMsg {
	t.Helper()
	switch key {
	case "1":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}
	case "2":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}}
	case "q":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "j":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}
	case "k":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "h":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}}
	case "s":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}
	case "p":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}
	case "l":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}
	case "r":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}
	case "/":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}
	case ":":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		t.Fatalf("unsupported key %q", key)
		return tea.KeyMsg{Type: tea.KeyNull}
	}
}

func runeMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestIssueDetailScrollsLongBodyWithinHeight(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("scroll-app", "SCR", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	var body strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&body, "L%02d unique body line\n", i)
	}
	if _, err := st.CreateIssue(project, "Long body issue", "todo", "high", nil, body.String()); err != nil {
		t.Fatalf("create long issue: %v", err)
	}
	next, err := st.CreateIssue(project, "Following issue", "todo", "low", nil, "short body")
	if err != nil {
		t.Fatalf("create next issue: %v", err)
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 20})
	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "enter"))
	top := current.View()

	if lines := strings.Count(top, "\n") + 1; lines > 20 {
		t.Fatalf("expected Issue detail to fit the terminal height, got %d lines:\n%s", lines, top)
	}
	if !strings.Contains(top, "L01 unique body line") {
		t.Fatalf("expected the first body line at the top of the scroll, got:\n%s", top)
	}
	if strings.Contains(top, "↑ ") { // the down indicator and statusbar carry no "↑ "
		t.Fatalf("expected no up overflow indicator at the top of the body, got:\n%s", top)
	}
	if !strings.Contains(top, "more") {
		t.Fatalf("expected a down overflow indicator for the long body, got:\n%s", top)
	}

	current, _ = current.Update(keyMsg(t, "pgdown"))
	scrolled := current.View()
	if lines := strings.Count(scrolled, "\n") + 1; lines > 20 {
		t.Fatalf("expected the scrolled Issue detail to fit the terminal height, got %d lines:\n%s", lines, scrolled)
	}
	if strings.Contains(scrolled, "L01 unique body line") {
		t.Fatalf("expected the body to scroll past its first line, got:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "↑ ") {
		t.Fatalf("expected an up overflow indicator after scrolling down, got:\n%s", scrolled)
	}

	current, _ = current.Update(keyMsg(t, "pgup"))
	if back := current.View(); strings.Contains(back, "↑ ") || !strings.Contains(back, "L01 unique body line") {
		t.Fatalf("expected pgup to return to the top of the body, got:\n%s", back)
	}

	// ↑↓ still navigate between Issues rather than scrolling the body, and opening
	// the next Issue resets the scroll offset.
	current, _ = current.Update(keyMsg(t, "down"))
	if nav := current.View(); !strings.Contains(nav, "ito · "+next.ID+" · Following issue") {
		t.Fatalf("expected down to move to the next Issue, got:\n%s", nav)
	}
}

func TestIssueDetailWithoutLinksHasNoDoubleBlank(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("spacing-app", "SPC", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Unlinked issue", "todo", "high", []string{"feature"}, "single body line"); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 24})
	current, _ = current.Update(keyMsg(t, "tab")) // focus TODO
	current, _ = current.Update(keyMsg(t, "enter"))
	detail := current.View()
	if !strings.Contains(detail, "ito · SPC-1 · Unlinked issue") {
		t.Fatalf("expected the Issue detail to open, got:\n%s", detail)
	}

	// With no blocked-by / relates-to links, the meta and the dates sit a single
	// blank apart — not two — so the top of the view doesn't read as empty.
	if strings.Contains(detail, "\n\n\n") {
		t.Fatalf("expected no double blank line in an unlinked Issue detail, got:\n%s", detail)
	}
	if !strings.Contains(detail, "created      ") || !strings.Contains(detail, "updated      ") {
		t.Fatalf("expected the dates block to still render, got:\n%s", detail)
	}
}

func TestLabelPickerEditsTheDisplayedIssueAfterStatusMove(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("picker-target-app", "PTA", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	target, err := st.CreateIssue(project, "Picker target", "in_review", "high", nil, "")
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}
	bystander, err := st.CreateIssue(project, "Innocent bystander", "in_review", "low", nil, "")
	if err != nil {
		t.Fatalf("create bystander issue: %v", err)
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "tab"))
	current, _ = current.Update(keyMsg(t, "tab")) // focus IN REVIEW
	current, _ = current.Update(keyMsg(t, "enter"))
	current, _ = current.Update(keyMsg(t, "s")) // target moves to done (hidden section)
	current, _ = current.Update(keyMsg(t, "l"))
	current, _ = current.Update(keyMsg(t, "enter")) // toggle the first label

	editedTarget, err := st.FindIssue(project, target.ID)
	if err != nil {
		t.Fatalf("find target issue: %v", err)
	}
	if !slices.Equal(editedTarget.Labels, []string{store.Labels[0]}) {
		t.Fatalf("expected the displayed Issue to gain the label, got %#v", editedTarget.Labels)
	}
	editedBystander, err := st.FindIssue(project, bystander.ID)
	if err != nil {
		t.Fatalf("find bystander issue: %v", err)
	}
	if len(editedBystander.Labels) != 0 {
		t.Fatalf("expected the digest selection to stay untouched, got %#v", editedBystander.Labels)
	}
}

func TestBoardDetailNavigatesWithinDoneIssues(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := store.New(db)
	project, err := st.CreateProject("board-done-app", "BDN", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, issue := range []struct{ title, status, priority string }{
		{"Backlog one", "backlog", "high"},
		{"Backlog two", "backlog", "low"},
		{"Done one", "done", "high"},
		{"Done two", "done", "low"},
	} {
		if _, err := st.CreateIssue(project, issue.title, issue.status, issue.priority, nil, ""); err != nil {
			t.Fatalf("create issue %q: %v", issue.title, err)
		}
	}

	current, _ := newModel(st, project).Update(keyMsg(t, "2"))
	for i := 0; i < 4; i++ { // focus the DONE column
		current, _ = current.Update(keyMsg(t, "tab"))
	}
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "Done one") {
		t.Fatalf("expected the first done Issue detail, got:\n%s", view)
	}

	// Down must walk the done column the Board displays, not jump to an
	// unrelated Issue because the Digest hides the DONE section.
	current, _ = current.Update(keyMsg(t, "down"))
	if view := current.View(); !strings.Contains(view, "Done two") {
		t.Fatalf("expected Down to show the next done Issue, got:\n%s", view)
	}
}

func TestScrollWindowNeverInvertsOnTinyBudgets(t *testing.T) {
	for budget := 1; budget <= 4; budget++ {
		for total := 0; total <= 8; total++ {
			for scroll := 0; scroll <= total+1; scroll++ {
				start, end, _, _ := scrollWindow(total, scroll, budget)
				if start > end || start < 0 || end > total {
					t.Fatalf("scrollWindow(%d, %d, %d) returned inverted or out-of-range window [%d, %d)", total, scroll, budget, start, end)
				}
			}
		}
	}
}

func TestWrapLineCountsRunesNotBytes(t *testing.T) {
	line := "café crème brûlée" // 17 runes, 22 bytes
	lines := wrapLine(line, 17)
	if len(lines) != 1 || lines[0] != line {
		t.Fatalf("expected accented text to fit its rune width, got %#v", lines)
	}
}
