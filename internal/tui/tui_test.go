package tui

import (
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
	if strings.Contains(view, "▸ DONE (1) · h to show") {
		t.Fatalf("expected matching hidden section to be expanded while filtering, got:\n%s", view)
	}
	if strings.Contains(view, "FHD-1 Open issue") {
		t.Fatalf("expected filter to keep non-matching Issues hidden, got:\n%s", view)
	}
	if strings.Contains(view, "TODO") || strings.Contains(view, "no Issues") {
		t.Fatalf("expected filter to omit sections with no matches, got:\n%s", view)
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
	case "s":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}
	case "p":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}
	case "l":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}
	case "/":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}
	default:
		t.Fatalf("unsupported key %q", key)
		return tea.KeyMsg{Type: tea.KeyNull}
	}
}

func runeMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}
