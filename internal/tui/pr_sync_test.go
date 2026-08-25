package tui

import (
	"reflect"
	"testing"

	"github.com/c3h/ito/internal/store"
)

func TestMapPRStatusMoves(t *testing.T) {
	issues := []store.Issue{
		{ID: "SYN-1", Branch: "feat/open", Status: "in_progress"},
		{ID: "SYN-2", Branch: "feat/merged", Status: "in_review"},
		{ID: "SYN-3", Branch: "feat/closed", Status: "in_progress"},
		{ID: "SYN-4", Branch: "", Status: "in_progress"},
		{ID: "SYN-5", Branch: "feat/done", Status: "done"},
		{ID: "SYN-6", Branch: "feat/unknown", Status: "in_review"},
		{ID: "SYN-7", Branch: "feat/open-review", Status: "in_review"},
		{ID: "SYN-8", Branch: "feat/todo-open", Status: "todo"},
		{ID: "SYN-9", Branch: "feat/todo-merged", Status: "todo"},
	}
	prs := []pullRequest{
		{HeadRefName: "feat/open", State: "OPEN"},
		{HeadRefName: "feat/todo-open", State: "OPEN"},
		{HeadRefName: "feat/todo-merged", State: "MERGED"},
		{HeadRefName: "feat/merged", State: "MERGED"},
		{HeadRefName: "feat/closed", State: "CLOSED"},
		{HeadRefName: "feat/done", State: "MERGED"},
		{HeadRefName: "feat/open-review", State: "OPEN"},
	}
	want := []issueStatusMove{
		{ID: "SYN-1", Status: "in_review"},
		{ID: "SYN-2", Status: "done"},
		{ID: "SYN-3", Status: "done"},
		{ID: "SYN-8", Status: "in_review"},
		{ID: "SYN-9", Status: "done"},
	}
	if got := mapPRStatusMoves(prSyncCandidates(issues), prs); !reflect.DeepEqual(got, want) {
		t.Fatalf("mapPRStatusMoves() = %#v, want %#v", got, want)
	}
}

func TestPRSyncMessageAppliesMovesReloadsAndShowsNote(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	st := store.New(db)
	project, err := st.CreateProject("sync-app", "SYN", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	issue, err := st.CreateIssue(project, "Sync this", "in_progress", "medium", nil, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	updated, _ := newModel(st, project, Options{}).Update(prSyncMsg{
		Project: project,
		Moves:   []issueStatusMove{{ID: issue.ID, Status: "in_review"}},
	})
	model := updated.(model)
	shown, err := st.FindIssue(project, issue.ID)
	if err != nil {
		t.Fatalf("show synced issue: %v", err)
	}
	if shown.Status != "in_review" {
		t.Fatalf("synced status = %q, want in_review", shown.Status)
	}
	if reloaded, ok := model.issueInSections(issue.ID); !ok || reloaded.Status != "in_review" {
		t.Fatalf("reloaded model does not contain %s in review", issue.ID)
	}
	if model.note != "PR sync: 1 updated" {
		t.Fatalf("sync note = %q", model.note)
	}
}
