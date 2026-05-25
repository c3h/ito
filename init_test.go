package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

type projectJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	RootPath string `json:"root_path"`
}

func TestInitCreatesCentralStoreForGitProject(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}

	itoHome := t.TempDir()
	beforeEntries := dirEntries(t, repo)
	result := runITO(t, repo, itoHome, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	var project projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if project.ID == 0 {
		t.Fatalf("expected durable project id, got %#v", project)
	}
	if project.Name != filepath.Base(repo) {
		t.Fatalf("expected default project name from folder, got %q", project.Name)
	}
	if project.Prefix == "" {
		t.Fatalf("expected default project prefix, got %#v", project)
	}
	if project.RootPath != canonicalRepo {
		t.Fatalf("expected git root_path %q, got %q", canonicalRepo, project.RootPath)
	}

	dbPath := filepath.Join(itoHome, "ito.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected central store at %s: %v", dbPath, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, table := range []string{"schema_version", "projects", "issues", "issue_links", "issue_labels", "issues_fts"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("expected table %s in store: %v", table, err)
		}
	}

	if _, err := os.Stat(filepath.Join(repo, ".ito")); !os.IsNotExist(err) {
		t.Fatalf("ito init must not create .ito inside repo, stat err=%v", err)
	}
	if afterEntries := dirEntries(t, repo); !stringSlicesEqual(beforeEntries, afterEntries) {
		t.Fatalf("ito init must not create or remove repo entries; before=%v after=%v", beforeEntries, afterEntries)
	}
}

func TestInitFromGitWorktreeUsesSharedGitRoot(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "-c", "user.name=Ito Test", "-c", "user.email=ito@example.test", "commit", "-q", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "linked")
	run(t, repo, "git", "worktree", "add", "-q", worktree)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktree).Run()
	})

	result := runITO(t, worktree, t.TempDir(), "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	var project projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, result.stdout)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if project.RootPath != canonicalRepo {
		t.Fatalf("expected shared git root_path %q, got %q", canonicalRepo, project.RootPath)
	}
}

func TestInitOutsideGitUsesClosestRegisteredAncestor(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	itoHome := t.TempDir()

	first := runITO(t, root, itoHome, "init", "--json", "--name", "plain-project")
	if first.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", first.exitCode, first.stdout, first.stderr)
	}

	second := runITO(t, child, itoHome, "init", "--json")
	if second.exitCode != 0 {
		t.Fatalf("ito init from child failed with exit %d\nstdout: %s\nstderr: %s", second.exitCode, second.stdout, second.stderr)
	}

	var project projectJSON
	if err := json.Unmarshal([]byte(second.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, second.stdout)
	}
	if project.Name != "plain-project" {
		t.Fatalf("expected ancestor project, got %#v", project)
	}
	if project.RootPath != canonicalRoot {
		t.Fatalf("expected ancestor root_path %q, got %q", canonicalRoot, project.RootPath)
	}
}

func TestInitMovedRepoPointsToReattach(t *testing.T) {
	parent := t.TempDir()
	originalRepo := filepath.Join(parent, "moved-app")
	if err := os.MkdirAll(originalRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, originalRepo, "git", "init", "-q")
	itoHome := t.TempDir()

	created := runITO(t, originalRepo, itoHome, "init", "--json", "--name", "moved-app")
	if created.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", created.exitCode, created.stdout, created.stderr)
	}

	movedRepo := filepath.Join(t.TempDir(), "moved-app")
	if err := os.Rename(originalRepo, movedRepo); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, movedRepo, itoHome, "init", "--json")
	if result.exitCode != 4 {
		t.Fatalf("expected moved repo diagnostic exit 4, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result.stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", result.stdout)
	}

	var errObject struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &errObject); err != nil {
		t.Fatalf("stderr is not a JSON error object: %v\nstderr: %s", err, result.stderr)
	}
	if errObject.Code != 4 {
		t.Fatalf("expected code 4, got %#v", errObject)
	}
	if !strings.Contains(errObject.Hint, "ito init --reattach moved-app") {
		t.Fatalf("expected actionable reattach hint, got %#v", errObject)
	}
}

func TestInitReattachRepointsNamedProject(t *testing.T) {
	parent := t.TempDir()
	originalRepo := filepath.Join(parent, "reattach-app")
	if err := os.MkdirAll(originalRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, originalRepo, "git", "init", "-q")
	itoHome := t.TempDir()

	created := runITO(t, originalRepo, itoHome, "init", "--json", "--name", "reattach-app")
	if created.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", created.exitCode, created.stdout, created.stderr)
	}
	var before projectJSON
	if err := json.Unmarshal([]byte(created.stdout), &before); err != nil {
		t.Fatal(err)
	}

	movedRepo := filepath.Join(t.TempDir(), "reattach-app")
	if err := os.Rename(originalRepo, movedRepo); err != nil {
		t.Fatal(err)
	}
	canonicalMovedRepo, err := filepath.EvalSymlinks(movedRepo)
	if err != nil {
		t.Fatal(err)
	}

	result := runITO(t, movedRepo, itoHome, "init", "--json", "--reattach", "reattach-app")
	if result.exitCode != 0 {
		t.Fatalf("ito init --reattach failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var after projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &after); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if after.ID != before.ID {
		t.Fatalf("reattach must preserve durable id; before=%#v after=%#v", before, after)
	}
	if after.Name != before.Name || after.Prefix != before.Prefix {
		t.Fatalf("reattach must preserve name and prefix; before=%#v after=%#v", before, after)
	}
	if after.RootPath != canonicalMovedRepo {
		t.Fatalf("expected reattached root_path %q, got %#v", canonicalMovedRepo, after)
	}
}

func TestInitReattachRejectsInvalidAndUnknownNames(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	invalid := runITO(t, repo, itoHome, "init", "--json", "--reattach", "Bad_Name")
	if invalid.exitCode != 2 {
		t.Fatalf("invalid reattach name must fail with exit 2, got %d\nstdout: %s\nstderr: %s", invalid.exitCode, invalid.stdout, invalid.stderr)
	}

	unknown := runITO(t, repo, itoHome, "init", "--json", "--reattach", "missing-project")
	if unknown.exitCode != 4 {
		t.Fatalf("unknown reattach name must fail with exit 4, got %d\nstdout: %s\nstderr: %s", unknown.exitCode, unknown.stdout, unknown.stderr)
	}
}

func TestInitJSONFailureUsesStableErrorObject(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")

	result := runITO(t, repo, t.TempDir(), "init", "--json", "--name", "Bad_Name")
	if result.exitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result.stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", result.stdout)
	}

	var errObject struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &errObject); err != nil {
		t.Fatalf("stderr is not a JSON error object: %v\nstderr: %s", err, result.stderr)
	}
	if errObject.Code != 2 || errObject.Error == "" || errObject.Hint == "" {
		t.Fatalf("expected actionable error object, got %#v", errObject)
	}
}

func TestInitNameAndPrefixRules(t *testing.T) {
	parent := t.TempDir()
	firstRepo := filepath.Join(parent, "api")
	secondRepo := filepath.Join(parent, "a-p-i")
	thirdRepo := filepath.Join(parent, "third")
	for _, repo := range []string{firstRepo, secondRepo, thirdRepo} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, repo, "git", "init", "-q")
	}
	itoHome := t.TempDir()

	first := runITO(t, firstRepo, itoHome, "init", "--json")
	if first.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", first.exitCode, first.stdout, first.stderr)
	}
	var firstProject projectJSON
	if err := json.Unmarshal([]byte(first.stdout), &firstProject); err != nil {
		t.Fatal(err)
	}
	if firstProject.Prefix != "API" {
		t.Fatalf("expected generated prefix API, got %#v", firstProject)
	}

	second := runITO(t, secondRepo, itoHome, "init", "--json")
	if second.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", second.exitCode, second.stdout, second.stderr)
	}
	var secondProject projectJSON
	if err := json.Unmarshal([]byte(second.stdout), &secondProject); err != nil {
		t.Fatal(err)
	}
	if secondProject.Prefix != "API2" {
		t.Fatalf("expected generated prefix collision to auto-suffix API2, got %#v", secondProject)
	}

	prefixCollision := runITO(t, thirdRepo, itoHome, "init", "--json", "--name", "third", "--prefix", "API")
	if prefixCollision.exitCode != 2 {
		t.Fatalf("manual prefix collision must fail with exit 2, got %d\nstdout: %s\nstderr: %s", prefixCollision.exitCode, prefixCollision.stdout, prefixCollision.stderr)
	}

	nameCollision := runITO(t, thirdRepo, itoHome, "init", "--json", "--name", "api", "--prefix", "THIRD")
	if nameCollision.exitCode != 2 {
		t.Fatalf("manual name collision must fail with exit 2, got %d\nstdout: %s\nstderr: %s", nameCollision.exitCode, nameCollision.stdout, nameCollision.stderr)
	}
}

func TestRenamePreservesProjectIdentityAndIssueRows(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	created := runITO(t, repo, itoHome, "init", "--json", "--name", "old-name", "--prefix", "OLD")
	if created.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", created.exitCode, created.stdout, created.stderr)
	}
	var before projectJSON
	if err := json.Unmarshal([]byte(created.stdout), &before); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	_, err := db.Exec(`
UPDATE projects SET last_id = 7 WHERE id = ?;
INSERT INTO issues(project_id, id, title, status, priority, body, created, updated)
VALUES (?, 'OLD-7', 'keep me', 'backlog', 'low', '', '2026-05-24T00:00:00Z', '2026-05-24T00:00:00Z');
`, before.ID, before.ID)
	if err != nil {
		t.Fatal(err)
	}

	result := runITO(t, repo, itoHome, "rename", "--json", "new-name")
	if result.exitCode != 0 {
		t.Fatalf("ito rename failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var after projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &after); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if after.ID != before.ID || after.Prefix != before.Prefix || after.RootPath != before.RootPath {
		t.Fatalf("rename must preserve id, prefix and root_path; before=%#v after=%#v", before, after)
	}
	if after.Name != "new-name" {
		t.Fatalf("expected new project name, got %#v", after)
	}

	var projectName, issueTitle string
	var lastID int
	err = db.QueryRow(`
SELECT projects.name, projects.last_id, issues.title
FROM projects
JOIN issues ON issues.project_id = projects.id
WHERE projects.id = ? AND issues.id = 'OLD-7'
`, before.ID).Scan(&projectName, &lastID, &issueTitle)
	if err != nil {
		t.Fatal(err)
	}
	if projectName != "new-name" || lastID != 7 || issueTitle != "keep me" {
		t.Fatalf("rename changed unrelated project data: name=%q lastID=%d issueTitle=%q", projectName, lastID, issueTitle)
	}

	otherDir := t.TempDir()
	explicit := runITO(t, otherDir, itoHome, "rename", "--json", "--project", "new-name", "final-name")
	if explicit.exitCode != 0 {
		t.Fatalf("ito rename --project failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}
	var final projectJSON
	if err := json.Unmarshal([]byte(explicit.stdout), &final); err != nil {
		t.Fatal(err)
	}
	if final.ID != before.ID || final.Name != "final-name" {
		t.Fatalf("expected explicit project rename from any cwd, got %#v", final)
	}
}

func TestRenameFromGitWorktreeResolvesSharedProject(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "-c", "user.name=Ito Test", "-c", "user.email=ito@example.test", "commit", "-q", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "linked")
	run(t, repo, "git", "worktree", "add", "-q", worktree)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktree).Run()
	})
	itoHome := t.TempDir()

	created := runITO(t, repo, itoHome, "init", "--json", "--name", "worktree-app")
	if created.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", created.exitCode, created.stdout, created.stderr)
	}
	var before projectJSON
	if err := json.Unmarshal([]byte(created.stdout), &before); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, worktree, itoHome, "rename", "--json", "worktree-renamed")
	if result.exitCode != 0 {
		t.Fatalf("ito rename from worktree failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var after projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &after); err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.RootPath != before.RootPath || after.Name != "worktree-renamed" {
		t.Fatalf("expected worktree rename to resolve shared Project; before=%#v after=%#v", before, after)
	}
}

func TestRenameValidatesFormatUniquenessAndUnknownProject(t *testing.T) {
	parent := t.TempDir()
	firstRepo := filepath.Join(parent, "first")
	secondRepo := filepath.Join(parent, "second")
	for _, repo := range []string{firstRepo, secondRepo} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, repo, "git", "init", "-q")
	}
	itoHome := t.TempDir()
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	invalid := runITO(t, firstRepo, itoHome, "rename", "--json", "Bad_Name")
	if invalid.exitCode != 2 {
		t.Fatalf("invalid rename must fail with exit 2, got %d\nstdout: %s\nstderr: %s", invalid.exitCode, invalid.stdout, invalid.stderr)
	}
	collision := runITO(t, firstRepo, itoHome, "rename", "--json", "second")
	if collision.exitCode != 2 {
		t.Fatalf("rename collision must fail with exit 2, got %d\nstdout: %s\nstderr: %s", collision.exitCode, collision.stdout, collision.stderr)
	}
	unknown := runITO(t, t.TempDir(), itoHome, "rename", "--json", "--project", "missing-project", "new-name")
	if unknown.exitCode != 4 {
		t.Fatalf("unknown --project must fail with exit 4, got %d\nstdout: %s\nstderr: %s", unknown.exitCode, unknown.stdout, unknown.stderr)
	}
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func runITO(t *testing.T, dir, itoHome string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"--test.run=TestHelperProcess", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"ITO_HOME="+itoHome,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run helper process: %v", err)
		}
	}
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			os.Exit(runCLI(os.Args[1:]))
		}
	}
	os.Exit(2)
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func openTestDB(t *testing.T, itoHome string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(itoHome, "ito.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
