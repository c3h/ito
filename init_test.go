package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
