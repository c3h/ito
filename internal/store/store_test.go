package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveProjectReturnsDetachedErrorWithProjectName(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	oldRoot := filepath.Join(t.TempDir(), "old-root")
	currentRoot := filepath.Join(t.TempDir(), "detached-app")
	if _, err := st.CreateProject("detached-app", "DET", oldRoot); err != nil {
		t.Fatalf("create project: %v", err)
	}

	_, err = st.ResolveProject(currentRoot, true, "")
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("expected ErrDetached, got %v", err)
	}
	var detached *DetachedError
	if !errors.As(err, &detached) {
		t.Fatalf("expected *DetachedError, got %T", err)
	}
	if detached.ProjectName != "detached-app" {
		t.Fatalf("expected project name detached-app, got %q", detached.ProjectName)
	}
}

func TestCreateIssueDeduplicatesLabels(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	project, err := st.CreateProject("label-app", "LAB", filepath.Join(t.TempDir(), "label"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	created, err := st.CreateIssue(project, "Repeated label", "backlog", "low", []string{"bug", "bug", "docs"}, "")
	if err != nil {
		t.Fatalf("create issue with repeated label: %v", err)
	}
	if !slices.Equal(created.Labels, []string{"bug", "docs"}) {
		t.Fatalf("expected deduplicated labels [bug docs], got %#v", created.Labels)
	}
}

func TestEditMissingLinkTargetNamesTheTarget(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	project, err := st.CreateProject("link-app", "LNK", filepath.Join(t.TempDir(), "link"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	source, err := st.CreateIssue(project, "Source issue", "backlog", "low", nil, "")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	_, err = st.Edit(project, source.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{{Kind: "blocked_by", Action: "add", Target: "LNK-999"}},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the error to unwrap to ErrNotFound, got %v", err)
	}
	var linkTarget *LinkTargetNotFoundError
	if !errors.As(err, &linkTarget) {
		t.Fatalf("expected *LinkTargetNotFoundError, got %T", err)
	}
	if linkTarget.TargetID != "LNK-999" {
		t.Fatalf("expected target LNK-999, got %q", linkTarget.TargetID)
	}
}

func TestCreateProjectReturnsTypedUniquenessErrors(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	if _, err := st.CreateProject("taken-app", "TAK", filepath.Join(t.TempDir(), "taken")); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, err := st.CreateProject("taken-app", "OTH", filepath.Join(t.TempDir(), "other")); !errors.Is(err, ErrNameExists) {
		t.Fatalf("expected ErrNameExists for a duplicate name, got %v", err)
	}
	if _, err := st.CreateProject("other-app", "TAK", filepath.Join(t.TempDir(), "other")); !errors.Is(err, ErrPrefixExists) {
		t.Fatalf("expected ErrPrefixExists for a duplicate prefix, got %v", err)
	}
	if _, err := st.CreateProjectWithGeneratedPrefix("taken-app", "taken-app", filepath.Join(t.TempDir(), "gen")); !errors.Is(err, ErrNameExists) {
		t.Fatalf("expected ErrNameExists from the generated-prefix path, got %v", err)
	}
}

func TestListIssuesIncludeDoneLiftsTheDefaultFilter(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	project, err := st.CreateProject("done-app", "DON", filepath.Join(t.TempDir(), "done"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, "Open issue", "todo", "low", nil, ""); err != nil {
		t.Fatalf("create open issue: %v", err)
	}
	if _, err := st.CreateIssue(project, "Done issue", "done", "low", nil, ""); err != nil {
		t.Fatalf("create done issue: %v", err)
	}

	hidden, err := st.ListIssues(ListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if len(hidden) != 1 || hidden[0].Status != "todo" {
		t.Fatalf("expected the default list to hide done, got %#v", hidden)
	}

	all, err := st.ListIssues(ListOptions{ProjectID: project.ID, IncludeDone: true})
	if err != nil {
		t.Fatalf("list issues with IncludeDone: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected IncludeDone to return both issues, got %#v", all)
	}
}

func TestListProjectsReturnsProjectsByName(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	if _, err := st.CreateProject("zeta-app", "ZET", filepath.Join(t.TempDir(), "zeta")); err != nil {
		t.Fatalf("create zeta project: %v", err)
	}
	if _, err := st.CreateProject("alpha-app", "ALP", filepath.Join(t.TempDir(), "alpha")); err != nil {
		t.Fatalf("create alpha project: %v", err)
	}

	projects, err := st.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}

	names := make([]string, 0, len(projects))
	for _, project := range projects {
		names = append(names, project.Name)
	}
	if !slices.Equal(names, []string{"alpha-app", "zeta-app"}) {
		t.Fatalf("expected projects ordered by name, got %#v", names)
	}
}
