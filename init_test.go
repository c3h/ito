package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type projectJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	RootPath string `json:"root_path"`
}

type issueJSON struct {
	ID        string   `json:"id"`
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels"`
	BlockedBy []string `json:"blocked_by"`
	RelatesTo []string `json:"relates_to"`
	Body      string   `json:"body"`
	Created   string   `json:"created"`
	Updated   string   `json:"updated"`
}

func TestNewCreatesIssueWithDefaults(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "new-app", "--prefix", "NEW")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}

	result := runITO(t, repo, itoHome, "new", "--json", "--title", "First tracked work")
	if result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.ID != "NEW-1" || issue.Project != "new-app" || issue.Title != "First tracked work" {
		t.Fatalf("unexpected issue identity: %#v", issue)
	}
	if issue.Status != "backlog" || issue.Priority != "low" || issue.Body != "" {
		t.Fatalf("unexpected issue defaults: %#v", issue)
	}
	if len(issue.Labels) != 0 || len(issue.BlockedBy) != 0 || len(issue.RelatesTo) != 0 {
		t.Fatalf("expected empty arrays for labels and links, got %#v", issue)
	}
	if issue.Created == "" || issue.Updated == "" || issue.Created != issue.Updated {
		t.Fatalf("expected equal created/updated timestamps, got %#v", issue)
	}
	createdAt, err := time.Parse(time.RFC3339, issue.Created)
	if err != nil {
		t.Fatalf("created timestamp must be RFC3339, got %q: %v", issue.Created, err)
	}
	if createdAt.Location() != time.UTC {
		t.Fatalf("created timestamp must be UTC, got %q", issue.Created)
	}
}

func TestNewHumanModePrintsOnlyMintedIDAndAdvancesCounter(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "human-app", "--prefix", "HUM"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	first := runITO(t, repo, itoHome, "new", "--title", "First")
	if first.exitCode != 0 {
		t.Fatalf("first ito new failed with exit %d\nstdout: %s\nstderr: %s", first.exitCode, first.stdout, first.stderr)
	}
	if first.stdout != "HUM-1\n" || first.stderr != "" {
		t.Fatalf("expected only minted ID on stdout, got stdout=%q stderr=%q", first.stdout, first.stderr)
	}

	second := runITO(t, repo, itoHome, "new", "--title", "Second")
	if second.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", second.exitCode, second.stdout, second.stderr)
	}
	if second.stdout != "HUM-2\n" || second.stderr != "" {
		t.Fatalf("expected next minted ID on stdout, got stdout=%q stderr=%q", second.stdout, second.stderr)
	}
}

func TestNewCreatesIssueWithExplicitFields(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "explicit-app", "--prefix", "EXP"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, repo, itoHome, "new", "--json",
		"--title", "Implement explicit fields",
		"--status", "todo",
		"--priority", "high",
		"--label", "feature",
		"--label", "tests",
		"--body", "## Notes\npreserve markdown",
	)
	if result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.ID != "EXP-1" || issue.Status != "todo" || issue.Priority != "high" || issue.Body != "## Notes\npreserve markdown" {
		t.Fatalf("unexpected explicit fields: %#v", issue)
	}
	if !stringSlicesEqual(issue.Labels, []string{"feature", "tests"}) {
		t.Fatalf("expected labels in input order, got %#v", issue.Labels)
	}
}

func TestNewReadsBodyFromStdin(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "stdin-app", "--prefix", "STD"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	body := "# Plan\n\n- keep markdown\n"
	result := runITOWithInput(t, repo, itoHome, body, "new", "--json", "--title", "From stdin", "--body", "-")
	if result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.Body != body {
		t.Fatalf("expected stdin body %q, got %#v", body, issue)
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "invalid-new", "--prefix", "INV"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "missing title", args: []string{"new", "--json"}},
		{name: "blank title", args: []string{"new", "--json", "--title", " \t\n"}},
		{name: "invalid status", args: []string{"new", "--json", "--title", "Bad status", "--status", "doing"}},
		{name: "invalid priority", args: []string{"new", "--json", "--title", "Bad priority", "--priority", "critical"}},
		{name: "invalid label", args: []string{"new", "--json", "--title", "Bad label", "--label", "custom"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, repo, itoHome, tt.args...)
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
				t.Fatalf("expected actionable bad-usage error object, got %#v", errObject)
			}
		})
	}
}

func TestNewFromUnregisteredCwdPointsToInit(t *testing.T) {
	result := runITO(t, t.TempDir(), t.TempDir(), "new", "--json", "--title", "No project")
	if result.exitCode != 4 {
		t.Fatalf("expected exit 4, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
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
	if errObject.Code != 4 || !strings.Contains(errObject.Hint, "ito init") {
		t.Fatalf("expected actionable init hint, got %#v", errObject)
	}
}

func TestNewConcurrentCallsMintUniqueProjectScopedIDs(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "concurrent-app", "--prefix", "CON"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	const total = 16
	start := make(chan struct{})
	results := make(chan commandResult, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			results <- runITO(t, repo, itoHome, "new", "--json", "--title", fmt.Sprintf("Concurrent %02d", n))
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	seen := make(map[string]bool, total)
	for result := range results {
		if result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
		var issue issueJSON
		if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
			t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
		}
		if seen[issue.ID] {
			t.Fatalf("duplicate minted ID %s", issue.ID)
		}
		seen[issue.ID] = true
	}
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("CON-%d", i)
		if !seen[id] {
			t.Fatalf("expected minted ID %s in %v", id, seen)
		}
	}
}

func TestListDefaultsToCurrentProjectAndHidesDone(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "list-app", "--prefix", "LST"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Open work", "--status", "todo"); result.exitCode != 0 {
		t.Fatalf("ito new open failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Finished work", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("ito new done failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, repo, itoHome, "list", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito list failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	issues := decodeIssueList(t, result.stdout)
	if len(issues) != 1 || issues[0].ID != "LST-1" {
		t.Fatalf("expected only non-done issue, got %#v", issues)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw[0]["body"]; ok {
		t.Fatalf("list JSON must omit body, got %s", result.stdout)
	}
	for _, key := range []string{"id", "project", "title", "status", "priority", "labels", "blocked_by", "relates_to", "created", "updated"} {
		if _, ok := raw[0][key]; !ok {
			t.Fatalf("list JSON missing key %q in %s", key, result.stdout)
		}
	}
}

func TestListFiltersByStatusPriorityAndLabels(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "filter-app", "--prefix", "FLT"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Feature infra", "--status", "todo", "--priority", "high", "--label", "feature", "--label", "infra"},
		{"new", "--json", "--title", "Feature only", "--status", "todo", "--priority", "medium", "--label", "feature"},
		{"new", "--json", "--title", "Bug infra", "--status", "in_progress", "--priority", "high", "--label", "bug", "--label", "infra"},
		{"new", "--json", "--title", "Done work", "--status", "done", "--priority", "urgent", "--label", "feature", "--label", "infra"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "status allows done explicitly", args: []string{"list", "--json", "--status", "done"}, want: []string{"FLT-4"}},
		{name: "priority", args: []string{"list", "--json", "--priority", "high"}, want: []string{"FLT-1", "FLT-3"}},
		{name: "label", args: []string{"list", "--json", "--label", "infra"}, want: []string{"FLT-1", "FLT-3"}},
		{name: "combined labels use AND", args: []string{"list", "--json", "--label", "feature", "--label", "infra"}, want: []string{"FLT-1"}},
		{name: "combined filters", args: []string{"list", "--json", "--status", "todo", "--priority", "high", "--label", "feature", "--label", "infra"}, want: []string{"FLT-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, repo, itoHome, tt.args...)
			if result.exitCode != 0 {
				t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", tt.args, result.exitCode, result.stdout, result.stderr)
			}
			if got := issueIDs(decodeIssueList(t, result.stdout)); !stringSlicesEqual(got, tt.want) {
				t.Fatalf("expected IDs %v, got %v\nstdout: %s", tt.want, got, result.stdout)
			}
		})
	}
}

func TestListProjectScopeFromAnyCwdAndAllProjects(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "alpha", "--prefix", "ALP"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "beta", "--prefix", "BET"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Alpha work"); result.exitCode != 0 {
		t.Fatalf("alpha new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "new", "--json", "--title", "Beta work"); result.exitCode != 0 {
		t.Fatalf("beta new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	explicit := runITO(t, t.TempDir(), itoHome, "list", "--json", "--project", "beta")
	if explicit.exitCode != 0 {
		t.Fatalf("ito list --project failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}
	if got := issueIDs(decodeIssueList(t, explicit.stdout)); !stringSlicesEqual(got, []string{"BET-1"}) {
		t.Fatalf("expected beta issue from any cwd, got %v", got)
	}

	allJSON := runITO(t, t.TempDir(), itoHome, "list", "--json", "--all-projects")
	if allJSON.exitCode != 0 {
		t.Fatalf("ito list --all-projects failed with exit %d\nstdout: %s\nstderr: %s", allJSON.exitCode, allJSON.stdout, allJSON.stderr)
	}
	if got := issueIDs(decodeIssueList(t, allJSON.stdout)); !stringSlicesEqual(got, []string{"ALP-1", "BET-1"}) {
		t.Fatalf("expected all projects ordered by project, got %v\nstdout: %s", got, allJSON.stdout)
	}

	human := runITO(t, t.TempDir(), itoHome, "list", "--all-projects")
	if human.exitCode != 0 {
		t.Fatalf("ito list --all-projects human failed with exit %d\nstdout: %s\nstderr: %s", human.exitCode, human.stdout, human.stderr)
	}
	for _, want := range []string{"alpha:\n  ALP-1", "beta:\n  BET-1"} {
		if !strings.Contains(human.stdout, want) {
			t.Fatalf("human all-projects output missing %q\nstdout: %s", want, human.stdout)
		}
	}

	conflict := runITO(t, t.TempDir(), itoHome, "list", "--json", "--project", "alpha", "--all-projects")
	if conflict.exitCode != 2 {
		t.Fatalf("--project with --all-projects must fail with exit 2, got %d\nstdout: %s\nstderr: %s", conflict.exitCode, conflict.stdout, conflict.stderr)
	}
}

func TestListOrdersByStatusPriorityAndUpdated(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "order-app", "--prefix", "ORD"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Todo low old", "--status", "todo", "--priority", "low"},
		{"new", "--json", "--title", "Backlog low", "--status", "backlog", "--priority", "low"},
		{"new", "--json", "--title", "Todo high older", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "Todo high newest", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "Review urgent", "--status", "in_review", "--priority", "urgent"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	for id, updated := range map[string]string{
		"ORD-1": "2026-05-24T10:00:00Z",
		"ORD-2": "2026-05-24T09:00:00Z",
		"ORD-3": "2026-05-24T08:00:00Z",
		"ORD-4": "2026-05-24T12:00:00Z",
		"ORD-5": "2026-05-24T13:00:00Z",
	} {
		if _, err := db.Exec(`UPDATE issues SET updated = ? WHERE id = ?`, updated, id); err != nil {
			t.Fatal(err)
		}
	}

	result := runITO(t, repo, itoHome, "list", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito list failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	want := []string{"ORD-2", "ORD-4", "ORD-3", "ORD-1", "ORD-5"}
	if got := issueIDs(decodeIssueList(t, result.stdout)); !stringSlicesEqual(got, want) {
		t.Fatalf("expected order %v, got %v\nstdout: %s", want, got, result.stdout)
	}
}

func TestListEmptyResultsSucceed(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "empty-app", "--prefix", "EMP"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	jsonResult := runITO(t, repo, itoHome, "list", "--json", "--label", "bug")
	if jsonResult.exitCode != 0 || strings.TrimSpace(jsonResult.stdout) != "[]" || jsonResult.stderr != "" {
		t.Fatalf("expected JSON empty success, got exit=%d stdout=%q stderr=%q", jsonResult.exitCode, jsonResult.stdout, jsonResult.stderr)
	}
	human := runITO(t, repo, itoHome, "list", "--label", "bug")
	if human.exitCode != 0 || !strings.Contains(human.stdout, "no Issues found") || human.stderr != "" {
		t.Fatalf("expected human empty success, got exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
}

func TestListIncludesStableArraysForLinks(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "links-app", "--prefix", "LNK")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Linked", "Blocker", "Related"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title, "--label", "feature"); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'LNK-1', 'LNK-2', 'blocked_by');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'LNK-3', 'LNK-1', 'relates_to');
`, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, repo, itoHome, "list", "--json", "--label", "feature")
	if result.exitCode != 0 {
		t.Fatalf("ito list failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	issues := decodeIssueList(t, result.stdout)
	if !stringSlicesEqual(issues[0].Labels, []string{"feature"}) || !stringSlicesEqual(issues[0].BlockedBy, []string{"LNK-2"}) || !stringSlicesEqual(issues[0].RelatesTo, []string{"LNK-3"}) {
		t.Fatalf("expected stable labels and links on first issue, got %#v", issues[0])
	}
	if issues[1].BlockedBy == nil || issues[1].RelatesTo == nil {
		t.Fatalf("expected empty link arrays, got %#v", issues[1])
	}
}

func TestShowResolvesGlobalIDFromAnyCwd(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "show-app", "--prefix", "SHO"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Inspectable", "--body", "## Full body\n\n- preserved\n"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "show", "--json", "SHO-1")
	if result.exitCode != 0 {
		t.Fatalf("ito show failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.ID != "SHO-1" || issue.Project != "show-app" || issue.Title != "Inspectable" {
		t.Fatalf("unexpected issue resolved by global ID: %#v", issue)
	}
	if issue.Body != "## Full body\n\n- preserved\n" {
		t.Fatalf("expected full markdown body, got %#v", issue)
	}
}

func TestShowRejectsMalformedAndUnknownIDs(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "unknown-app", "--prefix", "UNK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	malformed := runITO(t, t.TempDir(), itoHome, "show", "--json", "UNK")
	if malformed.exitCode != 2 {
		t.Fatalf("malformed ID must fail with exit 2, got %d\nstdout: %s\nstderr: %s", malformed.exitCode, malformed.stdout, malformed.stderr)
	}
	unknown := runITO(t, t.TempDir(), itoHome, "show", "--json", "UNK-99")
	if unknown.exitCode != 3 {
		t.Fatalf("unknown ID must fail with exit 3, got %d\nstdout: %s\nstderr: %s", unknown.exitCode, unknown.stdout, unknown.stderr)
	}
	var errObject struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(unknown.stderr), &errObject); err != nil {
		t.Fatalf("stderr is not a JSON error object: %v\nstderr: %s", err, unknown.stderr)
	}
	if errObject.Code != 3 || errObject.Error == "" || errObject.Hint == "" {
		t.Fatalf("expected actionable not-found error object, got %#v", errObject)
	}
}

func TestShowRejectsProjectOverrideMismatch(t *testing.T) {
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

	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-show", "--prefix", "FST"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-show", "--prefix", "SND"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Owned by first"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "show", "--json", "--project", "second-show", "FST-1")
	if result.exitCode != 2 {
		t.Fatalf("project mismatch must fail with exit 2, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
}

func TestShowJSONShapeIncludesStableEmptyAndLinkKeys(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "shape-app", "--prefix", "SHP")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	if result := runITO(t, repo, itoHome, "new", "--json",
		"--title", "JSON shape",
		"--status", "in_progress",
		"--priority", "urgent",
		"--label", "feature",
		"--body", "# Shape\n\nFull markdown",
	); result.exitCode != 0 {
		t.Fatalf("first ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Blocker"); result.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Related"); result.exitCode != 0 {
		t.Fatalf("third ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'SHP-1', 'SHP-2', 'blocked_by');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'SHP-3', 'SHP-1', 'relates_to');
`, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, t.TempDir(), itoHome, "show", "--json", "SHP-1")
	if result.exitCode != 0 {
		t.Fatalf("ito show failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\nstdout: %s", err, result.stdout)
	}
	expectedKeys := []string{"id", "project", "title", "status", "priority", "labels", "blocked_by", "relates_to", "body", "created", "updated"}
	if len(raw) != len(expectedKeys) {
		t.Fatalf("expected exactly keys %v, got %v in %s", expectedKeys, raw, result.stdout)
	}
	for _, key := range expectedKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing JSON key %q in %s", key, result.stdout)
		}
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if !stringSlicesEqual(issue.Labels, []string{"feature"}) {
		t.Fatalf("expected labels, got %#v", issue.Labels)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"SHP-2"}) || !stringSlicesEqual(issue.RelatesTo, []string{"SHP-3"}) {
		t.Fatalf("expected links from fixtures, got blocked_by=%#v relates_to=%#v", issue.BlockedBy, issue.RelatesTo)
	}
	if issue.Body != "# Shape\n\nFull markdown" || issue.Status != "in_progress" || issue.Priority != "urgent" {
		t.Fatalf("unexpected canonical issue fields: %#v", issue)
	}

	empty := runITO(t, t.TempDir(), itoHome, "show", "--json", "SHP-2")
	if empty.exitCode != 0 {
		t.Fatalf("ito show empty issue failed with exit %d\nstdout: %s\nstderr: %s", empty.exitCode, empty.stdout, empty.stderr)
	}
	var emptyIssue issueJSON
	if err := json.Unmarshal([]byte(empty.stdout), &emptyIssue); err != nil {
		t.Fatal(err)
	}
	if emptyIssue.Labels == nil || emptyIssue.BlockedBy == nil || emptyIssue.RelatesTo == nil || emptyIssue.Body != "" {
		t.Fatalf("expected stable empty arrays and body string, got %#v", emptyIssue)
	}
}

func TestShowHumanOutputIncludesFieldsLinksLabelsAndBody(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "human-show", "--prefix", "HSH"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Human issue", "--label", "docs", "--body", "## Body\n\ncomplete markdown"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "show", "HSH-1")
	if result.exitCode != 0 {
		t.Fatalf("ito show failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, want := range []string{
		"ID: HSH-1",
		"Project: human-show",
		"Title: Human issue",
		"Status: backlog",
		"Priority: low",
		"Labels: docs",
		"Links:",
		"blocked_by: []",
		"relates_to: []",
		"Body:",
		"## Body\n\ncomplete markdown",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("human output missing %q\nstdout: %s", want, result.stdout)
		}
	}
}

func TestMoveAcceptsEveryTargetStatusFromAnySourceAndCwd(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "move-app", "--prefix", "MOV"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Movable", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	for _, status := range []string{"backlog", "todo", "in_progress", "in_review", "done"} {
		result := runITO(t, t.TempDir(), itoHome, "move", "--json", "MOV-1", status)
		if result.exitCode != 0 {
			t.Fatalf("ito move %s failed with exit %d\nstdout: %s\nstderr: %s", status, result.exitCode, result.stdout, result.stderr)
		}
		var issue issueJSON
		if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
			t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
		}
		if issue.ID != "MOV-1" || issue.Project != "move-app" || issue.Status != status {
			t.Fatalf("expected issue moved to %s, got %#v", status, issue)
		}
	}
}

func TestMoveRejectsMalformedInvalidUnknownAndProjectMismatch(t *testing.T) {
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

	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-move", "--prefix", "FMV"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-move", "--prefix", "SMV"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Owned by first"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "malformed ID", args: []string{"move", "--json", "FMV", "todo"}, code: 2},
		{name: "invalid status", args: []string{"move", "--json", "FMV-1", "doing"}, code: 2},
		{name: "unknown ID", args: []string{"move", "--json", "FMV-99", "todo"}, code: 3},
		{name: "unknown prefix", args: []string{"move", "--json", "ZZZ-1", "todo"}, code: 3},
		{name: "project mismatch", args: []string{"move", "--json", "--project", "second-move", "FMV-1", "todo"}, code: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, t.TempDir(), itoHome, tt.args...)
			if result.exitCode != tt.code {
				t.Fatalf("expected exit %d, got %d\nstdout: %s\nstderr: %s", tt.code, result.exitCode, result.stdout, result.stderr)
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
			if errObject.Code != tt.code || errObject.Error == "" || errObject.Hint == "" {
				t.Fatalf("expected actionable error object with code %d, got %#v", tt.code, errObject)
			}
		})
	}
}

func TestMoveNoOpKeepsUpdatedAndReturnsCanonicalJSON(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "noop-move", "--prefix", "NOP")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "No-op", "--status", "todo", "--label", "feature"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Blocker"); result.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T11:00:00Z' WHERE project_id = ? AND id = 'NOP-1';
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'NOP-1', 'NOP-2', 'blocked_by');
`, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}

	jsonResult := runITO(t, t.TempDir(), itoHome, "move", "--json", "NOP-1", "todo")
	if jsonResult.exitCode != 0 {
		t.Fatalf("ito move --json no-op failed with exit %d\nstdout: %s\nstderr: %s", jsonResult.exitCode, jsonResult.stdout, jsonResult.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(jsonResult.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, jsonResult.stdout)
	}
	if issue.Status != "todo" || issue.Created != "2026-05-24T10:00:00Z" || issue.Updated != "2026-05-24T11:00:00Z" {
		t.Fatalf("no-op must preserve canonical timestamps and status, got %#v", issue)
	}
	if !stringSlicesEqual(issue.Labels, []string{"feature"}) || !stringSlicesEqual(issue.BlockedBy, []string{"NOP-2"}) {
		t.Fatalf("expected canonical labels and blocked_by links, got %#v", issue)
	}

	human := runITO(t, t.TempDir(), itoHome, "move", "NOP-1", "todo")
	if human.exitCode != 0 || !strings.Contains(human.stdout, "is already in todo") || human.stderr != "" {
		t.Fatalf("expected clear human no-op success, got exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
	var updated string
	if err := db.QueryRow(`SELECT updated FROM issues WHERE project_id = ? AND id = 'NOP-1'`, project.ID).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != "2026-05-24T11:00:00Z" {
		t.Fatalf("human no-op changed updated timestamp to %q", updated)
	}
}

func TestMoveRealChangeUpdatesOnlyUpdatedTimestampAndIgnoresBlockedBy(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "timestamp-move", "--prefix", "TIM")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Blocked move", "--status", "backlog"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Blocker"); result.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T10:00:00Z' WHERE project_id = ? AND id = 'TIM-1';
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'TIM-1', 'TIM-2', 'blocked_by');
`, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, t.TempDir(), itoHome, "move", "--json", "TIM-1", "done")
	if result.exitCode != 0 {
		t.Fatalf("ito move failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.Status != "done" || issue.Created != "2026-05-24T10:00:00Z" {
		t.Fatalf("move must update status and preserve created, got %#v", issue)
	}
	if issue.Updated == "2026-05-24T10:00:00Z" {
		t.Fatalf("move must update updated timestamp, got %#v", issue)
	}
	if _, err := time.Parse(time.RFC3339, issue.Updated); err != nil {
		t.Fatalf("updated timestamp must remain RFC3339, got %q: %v", issue.Updated, err)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"TIM-2"}) {
		t.Fatalf("blocked_by link must not prevent movement or disappear, got %#v", issue)
	}
}

func TestEditScalarsReturnCanonicalJSONUpdateFTSAndPreserveCreated(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "edit-scalars", "--prefix", "EDS")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Old title", "--priority", "low", "--body", "old body"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T10:00:00Z' WHERE project_id = ? AND id = 'EDS-1'`, project.ID); err != nil {
		t.Fatal(err)
	}

	body := "# New body\n\n- exact markdown\n"
	result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "EDS-1", "--title", "New title", "--priority", "urgent", "--body", body)
	if result.exitCode != 0 {
		t.Fatalf("ito edit failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.Title != "New title" || issue.Priority != "urgent" || issue.Body != body {
		t.Fatalf("edit must replace scalar fields and exact body, got %#v", issue)
	}
	if issue.Created != "2026-05-24T10:00:00Z" || issue.Updated == "2026-05-24T10:00:00Z" {
		t.Fatalf("edit must preserve created and change updated, got %#v", issue)
	}
	if _, err := time.Parse(time.RFC3339, issue.Updated); err != nil {
		t.Fatalf("updated timestamp must remain RFC3339, got %q: %v", issue.Updated, err)
	}
	var ftsMatches int
	if err := db.QueryRow(`SELECT count(*) FROM issues_fts WHERE issues_fts MATCH 'exact'`).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if ftsMatches != 1 {
		t.Fatalf("expected edited body to be indexed in FTS, got %d matches", ftsMatches)
	}
}

func TestEditReadsReplacementBodyFromStdin(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "edit-stdin", "--prefix", "EST"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Body issue", "--body", "old"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	body := "## Replacement\n\n```go\nfmt.Println(\"kept\")\n```\n"
	result := runITOWithInput(t, t.TempDir(), itoHome, body, "edit", "--json", "EST-1", "--body", "-")
	if result.exitCode != 0 {
		t.Fatalf("ito edit failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.Body != body {
		t.Fatalf("expected stdin body %q, got %#v", body, issue)
	}
}

func TestEditLabelsAndIdempotentOperationsKeepUpdatedWhenFinalStateUnchanged(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "edit-labels", "--prefix", "EDL")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Labels", "--label", "bug", "--label", "docs"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T10:00:00Z' WHERE project_id = ? AND id = 'EDL-1'`, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "EDL-1", "--add-label", "feature", "--remove-label", "docs", "--remove-label", "infra")
	if result.exitCode != 0 {
		t.Fatalf("ito edit labels failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if !stringSlicesEqual(issue.Labels, []string{"bug", "feature"}) || issue.Updated == "2026-05-24T10:00:00Z" {
		t.Fatalf("expected label change and updated timestamp, got %#v", issue)
	}
	updatedAfterChange := issue.Updated

	noChange := runITO(t, t.TempDir(), itoHome, "edit", "--json", "EDL-1", "--add-label", "bug", "--remove-label", "docs")
	if noChange.exitCode != 0 {
		t.Fatalf("ito edit idempotent labels failed with exit %d\nstdout: %s\nstderr: %s", noChange.exitCode, noChange.stdout, noChange.stderr)
	}
	if err := json.Unmarshal([]byte(noChange.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, noChange.stdout)
	}
	if !stringSlicesEqual(issue.Labels, []string{"bug", "feature"}) || issue.Updated != updatedAfterChange || issue.Created != "2026-05-24T10:00:00Z" {
		t.Fatalf("idempotent label operations must preserve final state and timestamps, got %#v", issue)
	}
}

func TestEditLinksDirectionalAndSymmetricNormalization(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "edit-links", "--prefix", "EDK")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Source", "Blocker", "Related"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}

	result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "EDK-1", "--block", "EDK-2", "--relate", "EDK-3")
	if result.exitCode != 0 {
		t.Fatalf("ito edit links failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"EDK-2"}) || !stringSlicesEqual(issue.RelatesTo, []string{"EDK-3"}) {
		t.Fatalf("expected directional and symmetric links on source, got %#v", issue)
	}
	blocker := runITO(t, t.TempDir(), itoHome, "show", "--json", "EDK-2")
	if blocker.exitCode != 0 {
		t.Fatalf("ito show blocker failed with exit %d\nstdout: %s\nstderr: %s", blocker.exitCode, blocker.stdout, blocker.stderr)
	}
	if err := json.Unmarshal([]byte(blocker.stdout), &issue); err != nil {
		t.Fatal(err)
	}
	if len(issue.BlockedBy) != 0 {
		t.Fatalf("blocked_by must be directional, got blocker issue %#v", issue)
	}
	related := runITO(t, t.TempDir(), itoHome, "show", "--json", "EDK-3")
	if related.exitCode != 0 {
		t.Fatalf("ito show related failed with exit %d\nstdout: %s\nstderr: %s", related.exitCode, related.stdout, related.stderr)
	}
	if err := json.Unmarshal([]byte(related.stdout), &issue); err != nil {
		t.Fatal(err)
	}
	if !stringSlicesEqual(issue.RelatesTo, []string{"EDK-1"}) {
		t.Fatalf("relates_to must display symmetrically, got %#v", issue)
	}

	duplicate := runITO(t, t.TempDir(), itoHome, "edit", "--json", "EDK-3", "--relate", "EDK-1")
	if duplicate.exitCode != 0 {
		t.Fatalf("reverse relate must be idempotent, got exit %d\nstdout: %s\nstderr: %s", duplicate.exitCode, duplicate.stdout, duplicate.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM issue_links WHERE project_id = ? AND kind = 'relates_to'`, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one normalized relates_to row, got %d", count)
	}
	var sourceID, targetID string
	if err := db.QueryRow(`SELECT source_id, target_id FROM issue_links WHERE project_id = ? AND kind = 'relates_to'`, project.ID).Scan(&sourceID, &targetID); err != nil {
		t.Fatal(err)
	}
	if sourceID != "EDK-1" || targetID != "EDK-3" {
		t.Fatalf("expected normalized pair EDK-1 -> EDK-3, got %s -> %s", sourceID, targetID)
	}
}

func TestEditLinksIdempotencyAndTimestampBehavior(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "link-timestamps", "--prefix", "LTS")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Source", "Blocker", "Related"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T10:00:00Z' WHERE project_id = ? AND id = 'LTS-1'`, project.ID); err != nil {
		t.Fatal(err)
	}

	changed := runITO(t, t.TempDir(), itoHome, "edit", "--json", "LTS-1", "--block", "LTS-2")
	if changed.exitCode != 0 {
		t.Fatalf("ito edit link change failed with exit %d\nstdout: %s\nstderr: %s", changed.exitCode, changed.stdout, changed.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(changed.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, changed.stdout)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"LTS-2"}) || issue.Updated == "2026-05-24T10:00:00Z" || issue.Created != "2026-05-24T10:00:00Z" {
		t.Fatalf("expected changed link and updated timestamp, got %#v", issue)
	}
	updatedAfterChange := issue.Updated

	noChange := runITO(t, t.TempDir(), itoHome, "edit", "--json", "LTS-1", "--block", "LTS-2", "--relate", "LTS-3", "--unrelate", "LTS-3")
	if noChange.exitCode != 0 {
		t.Fatalf("idempotent link edit failed with exit %d\nstdout: %s\nstderr: %s", noChange.exitCode, noChange.stdout, noChange.stderr)
	}
	if err := json.Unmarshal([]byte(noChange.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, noChange.stdout)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"LTS-2"}) || len(issue.RelatesTo) != 0 || issue.Updated != updatedAfterChange {
		t.Fatalf("idempotent link operations must preserve final state and updated, got %#v", issue)
	}
}

func TestEditLinksDeletionIndependentDisplay(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "deleted-links", "--prefix", "DLK")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Source", "Blocker", "Related"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "DLK-1", "--block", "DLK-2", "--relate", "DLK-3"); result.exitCode != 0 {
		t.Fatalf("ito edit links failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM issues WHERE project_id = ? AND id IN ('DLK-2', 'DLK-3')`, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, t.TempDir(), itoHome, "show", "--json", "DLK-1")
	if result.exitCode != 0 {
		t.Fatalf("ito show after deletion failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatal(err)
	}
	if issue.BlockedBy == nil || issue.RelatesTo == nil || len(issue.BlockedBy) != 0 || len(issue.RelatesTo) != 0 {
		t.Fatalf("deleted linked issues must not leave broken display links, got %#v", issue)
	}
}

func TestEditLinksValidationFailures(t *testing.T) {
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

	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-links", "--prefix", "FLK"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-links", "--prefix", "SLK"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Source"); result.exitCode != 0 {
		t.Fatalf("first ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "new", "--json", "--title", "Other project"); result.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "malformed link target", args: []string{"edit", "--json", "FLK-1", "--block", "FLK"}, code: 2},
		{name: "self block", args: []string{"edit", "--json", "FLK-1", "--block", "FLK-1"}, code: 2},
		{name: "self relate", args: []string{"edit", "--json", "FLK-1", "--relate", "FLK-1"}, code: 2},
		{name: "unknown target issue", args: []string{"edit", "--json", "FLK-1", "--block", "FLK-99"}, code: 3},
		{name: "unknown target prefix", args: []string{"edit", "--json", "FLK-1", "--relate", "ZZZ-1"}, code: 3},
		{name: "cross project target", args: []string{"edit", "--json", "FLK-1", "--block", "SLK-1"}, code: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, t.TempDir(), itoHome, tt.args...)
			if result.exitCode != tt.code {
				t.Fatalf("expected exit %d, got %d\nstdout: %s\nstderr: %s", tt.code, result.exitCode, result.stdout, result.stderr)
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
			if errObject.Code != tt.code || errObject.Error == "" || errObject.Hint == "" {
				t.Fatalf("expected actionable error object with code %d, got %#v", tt.code, errObject)
			}
		})
	}
}

func TestEditRejectsNoChangeInvalidInputsAndProjectMismatch(t *testing.T) {
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

	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-edit", "--prefix", "FED"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-edit", "--prefix", "SED"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Editable"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "no requested change", args: []string{"edit", "--json", "FED-1"}, code: 2},
		{name: "blank title", args: []string{"edit", "--json", "FED-1", "--title", " \t\n"}, code: 2},
		{name: "invalid priority", args: []string{"edit", "--json", "FED-1", "--priority", "critical"}, code: 2},
		{name: "invalid add label", args: []string{"edit", "--json", "FED-1", "--add-label", "custom"}, code: 2},
		{name: "invalid remove label", args: []string{"edit", "--json", "FED-1", "--remove-label", "custom"}, code: 2},
		{name: "malformed ID", args: []string{"edit", "--json", "FED", "--title", "New"}, code: 2},
		{name: "unknown ID", args: []string{"edit", "--json", "FED-99", "--title", "New"}, code: 3},
		{name: "unknown prefix", args: []string{"edit", "--json", "ZZZ-1", "--title", "New"}, code: 3},
		{name: "project mismatch", args: []string{"edit", "--json", "--project", "second-edit", "FED-1", "--title", "New"}, code: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, t.TempDir(), itoHome, tt.args...)
			if result.exitCode != tt.code {
				t.Fatalf("expected exit %d, got %d\nstdout: %s\nstderr: %s", tt.code, result.exitCode, result.stdout, result.stderr)
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
			if errObject.Code != tt.code || errObject.Error == "" || errObject.Hint == "" {
				t.Fatalf("expected actionable error object with code %d, got %#v", tt.code, errObject)
			}
		})
	}
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
	return runITOWithInput(t, dir, itoHome, "", args...)
}

func runITOWithInput(t *testing.T, dir, itoHome string, stdin string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"--test.run=TestHelperProcess", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"ITO_HOME="+itoHome,
	)
	cmd.Stdin = strings.NewReader(stdin)
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

func decodeIssueList(t *testing.T, stdout string) []issueJSON {
	t.Helper()
	var issues []issueJSON
	if err := json.Unmarshal([]byte(stdout), &issues); err != nil {
		t.Fatalf("stdout is not a JSON issue array: %v\nstdout: %s", err, stdout)
	}
	return issues
}

func issueIDs(issues []issueJSON) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func openTestDB(t *testing.T, itoHome string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(itoHome, "ito.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
