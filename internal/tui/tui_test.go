package tui

import (
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
		"▸ DONE (1) · h to show",
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
	if !strings.Contains(hiddenBacklog, "▌▸ BACKLOG (1) · h to show") {
		t.Fatalf("expected h to collapse the focused BACKLOG section, got:\n%s", hiddenBacklog)
	}
	if strings.Contains(hiddenBacklog, "HID-1 Backlog can hide") {
		t.Fatalf("expected collapsed BACKLOG to hide its Issue row, got:\n%s", hiddenBacklog)
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
	if !strings.Contains(focusedDone, "▌▸ DONE (1) · h to show") {
		t.Fatalf("expected collapsed DONE section to be focusable, got:\n%s", focusedDone)
	}

	current, _ = current.Update(keyMsg(t, "h"))
	revealedDone := current.View()
	if !strings.Contains(revealedDone, "▌▾ DONE  (1)") || !strings.Contains(revealedDone, "HID-2 Done can reveal") {
		t.Fatalf("expected h to reveal focused DONE section, got:\n%s", revealedDone)
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

	current, _ := newModel(st, project).Update(tea.WindowSizeMsg{Width: 88, Height: 21})
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
		"OVR-6 Scrollable issue 6",
		"↓ 2 more",
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
		"esc back   ↑↓ prev/next",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("expected Issue detail to contain %q, got:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "s status") || strings.Contains(detail, "p priority") || strings.Contains(detail, "l labels") {
		t.Fatalf("Issue detail should be read-only in ITO-7, got edit shortcuts:\n%s", detail)
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
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "h":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}}
	default:
		t.Fatalf("unsupported key %q", key)
		return tea.KeyMsg{Type: tea.KeyNull}
	}
}
