package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateFreshDatabaseReachesBatchSchemaVersion(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	assertSchemaVersion(t, db, 2)
	assertColumnExists(t, db, "issues", "batch_id")
	if _, err := db.Exec(`INSERT INTO batches(project_id, name, created) VALUES (1, 'orphan', '2026-06-12T10:00:00Z')`); err == nil {
		t.Fatal("expected foreign key to reject a Batch without a Project")
	}
}

func TestMigrateUpgradesExistingV1DatabaseInPlace(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "ito.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (1);
CREATE TABLE projects (
  id        INTEGER PRIMARY KEY,
  name      TEXT UNIQUE NOT NULL,
  root_path TEXT UNIQUE,
  prefix    TEXT UNIQUE NOT NULL,
  last_id   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE issues (
  row_id     INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  id         TEXT NOT NULL,
  title      TEXT NOT NULL,
  status     TEXT NOT NULL,
  priority   TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  created    TEXT NOT NULL,
  updated    TEXT NOT NULL,
  UNIQUE (project_id, id)
);
CREATE VIRTUAL TABLE issues_fts USING fts5(
  title,
  body,
  content='issues',
  content_rowid='row_id',
  tokenize='unicode61',
  prefix='2 3 4'
);
CREATE TABLE issue_links (
  project_id INTEGER NOT NULL,
  source_id  TEXT NOT NULL,
  target_id  TEXT NOT NULL,
  kind       TEXT NOT NULL,
  PRIMARY KEY (project_id, source_id, target_id, kind),
  FOREIGN KEY (project_id, source_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  FOREIGN KEY (project_id, target_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  CHECK (source_id != target_id)
);
CREATE TABLE issue_labels (
  project_id INTEGER NOT NULL,
  issue_id   TEXT NOT NULL,
  label      TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_id, label),
  FOREIGN KEY (project_id, issue_id) REFERENCES issues(project_id, id) ON DELETE CASCADE
);
INSERT INTO projects(id, name, root_path, prefix, last_id) VALUES (1, 'legacy-app', '/tmp/legacy-app', 'LEG', 1);
INSERT INTO issues(project_id, id, title, status, priority, body, created, updated)
VALUES (1, 'LEG-1', 'Legacy issue', 'todo', 'low', '', '2026-06-12T10:00:00Z', '2026-06-12T10:00:00Z');
`); err != nil {
		t.Fatalf("seed v1 database: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate v1 database: %v", err)
	}

	assertSchemaVersion(t, db, 2)
	assertColumnExists(t, db, "issues", "batch_id")
	var title string
	if err := db.QueryRow(`SELECT title FROM issues WHERE id = 'LEG-1'`).Scan(&title); err != nil {
		t.Fatalf("legacy issue was not preserved: %v", err)
	}
	if title != "Legacy issue" {
		t.Fatalf("expected legacy issue title to survive, got %q", title)
	}
	if _, err := db.Exec(`INSERT INTO batches(project_id, name, created) VALUES (1, 'refactor', '2026-06-12T11:00:00Z')`); err != nil {
		t.Fatalf("insert batch after migration: %v", err)
	}
}

func assertSchemaVersion(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&got); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if got != want {
		t.Fatalf("expected schema version %d, got %d", want, got)
	}
}

func assertColumnExists(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	found, err := columnExists(db, table, column)
	if err != nil {
		t.Fatalf("read columns for %s: %v", table, err)
	}
	if !found {
		t.Fatalf("expected %s.%s to exist", table, column)
	}
}

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

func TestEditConflictsWithNormalizesSymmetricLinks(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	st := New(db)
	project, err := st.CreateProject("conflict-app", "CNF", filepath.Join(t.TempDir(), "conflict"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	first, err := st.CreateIssue(project, "First issue", "backlog", "low", nil, "")
	if err != nil {
		t.Fatalf("create first issue: %v", err)
	}
	second, err := st.CreateIssue(project, "Second issue", "backlog", "low", nil, "")
	if err != nil {
		t.Fatalf("create second issue: %v", err)
	}

	added, err := st.Edit(project, second.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{{Kind: "conflicts_with", Action: "add", Target: first.ID}},
	})
	if err != nil {
		t.Fatalf("add conflict: %v", err)
	}
	if !added.Changed {
		t.Fatalf("expected first conflict add to change")
	}
	firstFound, err := st.FindIssue(project, first.ID)
	if err != nil {
		t.Fatalf("find first issue: %v", err)
	}
	secondFound, err := st.FindIssue(project, second.ID)
	if err != nil {
		t.Fatalf("find second issue: %v", err)
	}
	if !slices.Equal(firstFound.ConflictsWith, []string{second.ID}) || !slices.Equal(secondFound.ConflictsWith, []string{first.ID}) {
		t.Fatalf("expected symmetric conflict, first=%#v second=%#v", firstFound.ConflictsWith, secondFound.ConflictsWith)
	}

	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM issue_links WHERE project_id = ? AND kind = 'conflicts_with'`, project.ID).Scan(&rows); err != nil {
		t.Fatalf("count conflict rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected one normalized conflict row, got %d", rows)
	}

	redundantAdd, err := st.Edit(project, first.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{{Kind: "conflicts_with", Action: "add", Target: second.ID}},
	})
	if err != nil {
		t.Fatalf("redundant add conflict: %v", err)
	}
	if redundantAdd.Changed || redundantAdd.Issue.Updated != firstFound.Updated {
		t.Fatalf("redundant conflict add must not stamp updated, before=%q after=%q changed=%v", firstFound.Updated, redundantAdd.Issue.Updated, redundantAdd.Changed)
	}

	removed, err := st.Edit(project, first.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{{Kind: "conflicts_with", Action: "remove", Target: second.ID}},
	})
	if err != nil {
		t.Fatalf("remove conflict: %v", err)
	}
	if !removed.Changed || len(removed.Issue.ConflictsWith) != 0 {
		t.Fatalf("expected conflict removal to change and clear links, got %#v", removed)
	}
	redundantRemove, err := st.Edit(project, second.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{{Kind: "conflicts_with", Action: "remove", Target: first.ID}},
	})
	if err != nil {
		t.Fatalf("redundant remove conflict: %v", err)
	}
	if redundantRemove.Changed || redundantRemove.Issue.Updated != secondFound.Updated {
		t.Fatalf("redundant conflict remove must not stamp updated, before=%q after=%q changed=%v", secondFound.Updated, redundantRemove.Issue.Updated, redundantRemove.Changed)
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
