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
