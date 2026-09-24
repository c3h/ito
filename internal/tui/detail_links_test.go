package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// linkedDetailFixture holds a todo target Issue linked to backlog Issues, so
// tab + enter from the Digest opens the target.
type linkedDetailFixture struct {
	st      *store.Store
	project store.Project
	target  store.Issue
}

func newLinkedDetailFixture(t *testing.T, prefix string, targetBody string, links map[string][]string) linkedDetailFixture {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	st := store.New(db)
	project, err := st.CreateProject(strings.ToLower(prefix)+"-app", prefix, t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	var ops []store.LinkEditOp
	for _, kind := range []string{"blocked_by", "conflicts_with", "relates_to"} {
		for _, title := range links[kind] {
			linked, err := st.CreateIssue(project, store.NewIssue{Title: title, Status: "backlog", Priority: "low", Body: title + " body"})
			if err != nil {
				t.Fatalf("create linked issue %q: %v", title, err)
			}
			ops = append(ops, store.LinkEditOp{Kind: kind, Action: "add", Target: linked.ID})
		}
	}
	target, err := st.CreateIssue(project, store.NewIssue{Title: "Linked target", Status: "todo", Priority: "high", Body: targetBody})
	if err != nil {
		t.Fatalf("create target issue: %v", err)
	}
	if len(ops) > 0 {
		if _, err := st.Edit(project, target.ID, store.EditIssueOptions{LinkOps: ops}); err != nil {
			t.Fatalf("link target issue: %v", err)
		}
	}
	return linkedDetailFixture{st: st, project: project, target: target}
}

// openTarget opens the fixture's target detail from the Digest at the given
// terminal size.
func (f linkedDetailFixture) openTarget(t *testing.T, width, height int) tea.Model {
	t.Helper()
	current, _ := newModel(f.st, f.project, Options{}).Update(tea.WindowSizeMsg{Width: width, Height: height})
	current, _ = current.Update(keyMsg(t, "tab")) // focus TODO
	current, _ = current.Update(keyMsg(t, "enter"))
	if view := current.View(); !strings.Contains(view, "ito · "+f.target.ID+" · Linked target") {
		t.Fatalf("expected the target Issue detail, got:\n%s", view)
	}
	return current
}

func press(t *testing.T, current tea.Model, keys ...string) tea.Model {
	t.Helper()
	for _, key := range keys {
		current, _ = current.Update(keyMsg(t, key))
	}
	return current
}

// linkRows returns the rendered lines of the Links block: rows led by a kind
// label, and the unlabelled rows and overflow indicators indented to the ids.
func linkRows(view string) []string {
	var rows []string
	leads := []string{" blocked by ", " conflicts with ", " relates to ", strings.Repeat(" ", detailLabelWidth-1)}
	for _, line := range strings.Split(view, "\n") {
		for _, lead := range leads {
			if strings.HasPrefix(line, lead) && strings.TrimSpace(line) != "" {
				rows = append(rows, line)
				break
			}
		}
	}
	return rows
}

func TestDetailLinksOrderByStrengthAndNameEachIssueOnce(t *testing.T) {
	links := detailLinks(store.Issue{
		BlockedBy:     []string{"A-1"},
		RelatesTo:     []string{"A-2", "A-3", "A-1"},
		ConflictsWith: []string{"A-3", "A-4"},
	})
	want := []detailLink{
		{kind: "blocked by", id: "A-1"},
		{kind: "conflicts with", id: "A-3"},
		{kind: "conflicts with", id: "A-4"},
		{kind: "relates to", id: "A-2"},
	}
	if !slices.Equal(links, want) {
		t.Fatalf("expected Links strongest kind first, each Issue once:\nwant %v\ngot  %v", want, links)
	}
}

func TestIssueDetailCapsLinksAndKeepsTheBodyInView(t *testing.T) {
	long := strings.Repeat("a linked title long enough to run past the frame ", 4)
	var related []string
	for i := 1; i <= 10; i++ {
		related = append(related, fmt.Sprintf("Related %02d %s", i, long))
	}
	f := newLinkedDetailFixture(t, "CAP", "first body line\nsecond body line", map[string][]string{
		"blocked_by":     {"Blocker " + long},
		"conflicts_with": {"Conflict one", "Conflict two"},
		"relates_to":     related,
	})

	const width, height = 100, 30
	detail := f.openTarget(t, width, height).View()

	lines := strings.Split(detail, "\n")
	if len(lines) > height {
		t.Fatalf("expected the detail to fit %d lines, got %d:\n%s", height, len(lines), detail)
	}
	if !strings.Contains(detail, "first body line") || !strings.Contains(detail, "second body line") {
		t.Fatalf("expected the body to stay in view beside a heavily linked Issue, got:\n%s", detail)
	}
	rows := linkRows(detail)
	if len(rows) > detailLinkLines {
		t.Fatalf("expected the Links block capped at %d lines, got %d:\n%s", detailLinkLines, len(rows), detail)
	}
	blocked, conflict := strings.Index(detail, " blocked by "), strings.Index(detail, " conflicts with ")
	if blocked < 0 || conflict < 0 || blocked > conflict {
		t.Fatalf("expected blocked by ahead of conflicts with, got:\n%s", detail)
	}
	if !strings.Contains(detail, "↓ 10 more") {
		t.Fatalf("expected an overflow indicator for the hidden Links, got:\n%s", detail)
	}
	if strings.Contains(detail, " relates to ") {
		t.Fatalf("expected relates to past the window before scrolling, got:\n%s", detail)
	}
}

func TestIssueDetailTabFocusesLinksAndEnterFollowsThem(t *testing.T) {
	f := newLinkedDetailFixture(t, "FOL", "target body", map[string][]string{
		"blocked_by": {"Blocking work"},
		"relates_to": {"Nearby work"},
	})
	blocker, related := "FOL-1", "FOL-2"

	current := f.openTarget(t, 100, 30)
	if view := current.View(); !strings.Contains(view, "esc back   tab links   ↑↓ prev/next") || strings.Contains(view, "▸") {
		t.Fatalf("expected the body focused, with tab offered to reach the Links, got:\n%s", view)
	}

	current = press(t, current, "tab")
	view := current.View()
	if !strings.Contains(view, "blocked by     ▸ "+blocker+"   Blocking work") {
		t.Fatalf("expected tab to put the cursor on the first Link, got:\n%s", view)
	}
	if !strings.Contains(view, "tab body   ↑↓ select   ⏎ open") {
		t.Fatalf("expected the Links key set in the bottom bar, got:\n%s", view)
	}

	current = press(t, current, "down")
	if view := current.View(); !strings.Contains(view, "relates to     ▸ "+related+"   Nearby work") {
		t.Fatalf("expected down to move the Links cursor, not the Issue, got:\n%s", view)
	}

	current = press(t, current, "enter")
	if view := current.View(); !strings.Contains(view, "ito · "+related+" · Nearby work") || !strings.Contains(view, "Nearby work body") {
		t.Fatalf("expected enter to open the linked Issue, got:\n%s", view)
	}
	if view := current.View(); strings.Contains(view, "▸") {
		t.Fatalf("expected the linked Issue to open with its body focused, got:\n%s", view)
	}

	current = press(t, current, "esc")
	if view := current.View(); !strings.Contains(view, "ito · "+f.target.ID+" · Linked target") ||
		!strings.Contains(view, "relates to     ▸ "+related) {
		t.Fatalf("expected esc to walk back to the origin with the followed Link selected, got:\n%s", view)
	}

	current = press(t, current, "esc")
	if view := current.View(); !strings.Contains(view, "ito · "+f.target.ID) || strings.Contains(view, "▸") {
		t.Fatalf("expected esc to hand focus back to the body, got:\n%s", view)
	}

	current = press(t, current, "esc")
	if view := current.View(); !strings.Contains(view, "TODO  (1)") {
		t.Fatalf("expected the last esc to return to the Digest, got:\n%s", view)
	}
}

func TestIssueDetailLinksWindowFollowsTheCursor(t *testing.T) {
	var related []string
	for i := 1; i <= 6; i++ {
		related = append(related, fmt.Sprintf("Related %d", i))
	}
	f := newLinkedDetailFixture(t, "WIN", "target body", map[string][]string{"relates_to": related})

	current := press(t, f.openTarget(t, 100, 30), "tab", "down", "down", "down", "down")
	view := current.View()
	if !strings.Contains(view, "▸ WIN-5   Related 5") {
		t.Fatalf("expected the cursor on the fifth Link, got:\n%s", view)
	}
	if !strings.Contains(view, "↑ ") || !strings.Contains(view, "↓ ") {
		t.Fatalf("expected overflow indicators on both sides mid-list, got:\n%s", view)
	}
	if rows := linkRows(view); len(rows) > detailLinkLines {
		t.Fatalf("expected the scrolled Links block capped at %d lines, got %d:\n%s", detailLinkLines, len(rows), view)
	}
	if strings.Count(view, "relates to") != 1 {
		t.Fatalf("expected the kind label once, on the window's first row, got:\n%s", view)
	}

	current = press(t, current, "up", "up", "up", "up")
	if view := current.View(); strings.Contains(view, "↑ ") || !strings.Contains(view, "relates to     ▸ WIN-1") {
		t.Fatalf("expected up to bring the window back to the first Link, got:\n%s", view)
	}
}

func TestIssueDetailPrevNextLeavesTheLinkTrail(t *testing.T) {
	f := newLinkedDetailFixture(t, "TRL", "target body", map[string][]string{"relates_to": {"First related", "Second related"}})

	current := press(t, f.openTarget(t, 100, 30), "tab", "enter")
	if view := current.View(); !strings.Contains(view, "ito · TRL-1 · First related") {
		t.Fatalf("expected enter to open the first linked Issue, got:\n%s", view)
	}

	current = press(t, current, "down")
	if view := current.View(); !strings.Contains(view, "ito · TRL-2 · Second related") {
		t.Fatalf("expected down to step to the next Issue in the Digest, got:\n%s", view)
	}

	current = press(t, current, "esc")
	if view := current.View(); !strings.Contains(view, "BACKLOG  (2)") {
		t.Fatalf("expected esc after prev/next to return to the Digest, not the Link's origin, got:\n%s", view)
	}
}

func TestIssueDetailWithoutLinksKeepsTabInert(t *testing.T) {
	f := newLinkedDetailFixture(t, "NOL", "plain body", nil)

	current := press(t, f.openTarget(t, 100, 30), "tab")
	view := current.View()
	if strings.Contains(view, "tab links") || strings.Contains(view, "tab body") || strings.Contains(view, "▸") {
		t.Fatalf("expected tab to do nothing on an Issue without Links, got:\n%s", view)
	}
	if !strings.Contains(view, "esc back   ↑↓ prev/next") {
		t.Fatalf("expected the plain detail key set, got:\n%s", view)
	}
}

func (f linkedDetailFixture) editLinks(t *testing.T, ops ...store.LinkEditOp) {
	t.Helper()
	if _, err := f.st.Edit(f.project, f.target.ID, store.EditIssueOptions{LinkOps: ops}); err != nil {
		t.Fatalf("edit target links: %v", err)
	}
}

func TestIssueDetailLinksCursorSurvivesARefreshThatShortensTheBlock(t *testing.T) {
	f := newLinkedDetailFixture(t, "SHR", "target body", map[string][]string{
		"relates_to": {"Related 1", "Related 2", "Related 3", "Related 4", "Related 5"},
	})

	current := press(t, f.openTarget(t, 100, 30), "tab", "down", "down", "down", "down")
	f.editLinks(t,
		store.LinkEditOp{Kind: "relates_to", Action: "remove", Target: "SHR-3"},
		store.LinkEditOp{Kind: "relates_to", Action: "remove", Target: "SHR-4"},
		store.LinkEditOp{Kind: "relates_to", Action: "remove", Target: "SHR-5"},
	)
	current = press(t, current, "r")
	if view := current.View(); !strings.Contains(view, "▸ SHR-2") {
		t.Fatalf("expected the cursor clamped onto the last remaining Link, got:\n%s", view)
	}

	current = press(t, current, "up")
	if view := current.View(); !strings.Contains(view, "▸ SHR-1") {
		t.Fatalf("expected the first up after the refresh to move the cursor, got:\n%s", view)
	}
}

func TestIssueDetailWalksBackToTheFollowedLinkAfterARefreshReordersIt(t *testing.T) {
	f := newLinkedDetailFixture(t, "RDR", "target body", map[string][]string{
		"relates_to": {"Followed work", "Unrelated blocker"},
	})
	followed, blocker := "RDR-1", "RDR-2"
	f.editLinks(t, store.LinkEditOp{Kind: "relates_to", Action: "remove", Target: blocker})

	current := press(t, f.openTarget(t, 100, 30), "tab", "enter")
	if view := current.View(); !strings.Contains(view, "ito · "+followed+" · Followed work") {
		t.Fatalf("expected enter to open the followed Issue, got:\n%s", view)
	}

	// A stronger Link lands on the origin while the followed Issue is open, so
	// the followed Link no longer sits on the first row.
	f.editLinks(t, store.LinkEditOp{Kind: "blocked_by", Action: "add", Target: blocker})
	current = press(t, current, "r", "esc")
	if view := current.View(); !strings.Contains(view, "relates to     ▸ "+followed) {
		t.Fatalf("expected esc to land on the followed Link, not its old row, got:\n%s", view)
	}
}

func TestIssueDetailEscSkipsALinksFocusARefreshEmptied(t *testing.T) {
	f := newLinkedDetailFixture(t, "EMP", "target body", map[string][]string{"relates_to": {"Only link"}})

	current := press(t, f.openTarget(t, 100, 30), "tab")
	f.editLinks(t, store.LinkEditOp{Kind: "relates_to", Action: "remove", Target: "EMP-1"})
	current = press(t, current, "r")
	if view := current.View(); !strings.Contains(view, "esc back   ↑↓ prev/next") {
		t.Fatalf("expected the plain detail key set once the Links are gone, got:\n%s", view)
	}

	current = press(t, current, "esc")
	if view := current.View(); !strings.Contains(view, "TODO  (1)") {
		t.Fatalf("expected esc to return to the Digest as the bar offers, got:\n%s", view)
	}
}
