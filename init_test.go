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

	itostore "github.com/c3h/ito/internal/store"
	_ "modernc.org/sqlite"
)

type projectJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	RootPath string `json:"root_path"`
}

type issueJSON struct {
	ID            string   `json:"id"`
	Project       string   `json:"project"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	Priority      string   `json:"priority"`
	Labels        []string `json:"labels"`
	BlockedBy     []string `json:"blocked_by"`
	RelatesTo     []string `json:"relates_to"`
	ConflictsWith []string `json:"conflicts_with"`
	Batch         *string  `json:"batch"`
	Body          string   `json:"body"`
	Created       string   `json:"created"`
	Updated       string   `json:"updated"`
}

type batchJSON struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Created string `json:"created"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Waves   *int   `json:"waves"`
}

type deletedBatchJSON struct {
	Deleted        string `json:"deleted"`
	MembersCleared int    `json:"members_cleared"`
}

type batchShowJSON struct {
	Name    string          `json:"name"`
	Project string          `json:"project"`
	Created string          `json:"created"`
	Total   int             `json:"total"`
	Done    int             `json:"done"`
	Waves   []batchWaveJSON `json:"waves"`
}

type batchWaveJSON struct {
	Wave   int         `json:"wave"`
	Ready  bool        `json:"ready"`
	Issues []issueJSON `json:"issues"`
}

func TestHelpPrintsUsageForRootAndCommands(t *testing.T) {
	repo := t.TempDir()
	itoHome := t.TempDir()

	tests := []struct {
		name     string
		args     []string
		contains []string
	}{
		{
			name:     "root long help",
			args:     []string{"--help"},
			contains: []string{"usage: ito <command> [flags]", "Commands:", "init", "new", "list", "batch"},
		},
		{
			name:     "root short help",
			args:     []string{"-h"},
			contains: []string{"usage: ito <command> [flags]", "Commands:"},
		},
		{
			name:     "init help",
			args:     []string{"init", "--help"},
			contains: []string{"usage: ito init", "--name", "--prefix", "--reattach", "--json"},
		},
		{
			name:     "new help",
			args:     []string{"new", "--help"},
			contains: []string{"usage: ito new", "--title", "--status", "--priority", "--label", "--body", "--batch"},
		},
		{
			name:     "list help",
			args:     []string{"list", "--help"},
			contains: []string{"usage: ito list", "--ready", "--batch", "conflicts_with", "one git worktree per ready Issue"},
		},
		{
			name:     "batch help",
			args:     []string{"batch", "--help"},
			contains: []string{"usage: ito batch <command>", "new", "list", "show", "rename", "rm"},
		},
		{
			name:     "batch new help",
			args:     []string{"batch", "new", "--help"},
			contains: []string{"usage: ito batch new", "--project", "--json"},
		},
		{
			name:     "batch list help",
			args:     []string{"batch", "list", "--help"},
			contains: []string{"usage: ito batch list", "--project", "--json"},
		},
		{
			name:     "batch rename help",
			args:     []string{"batch", "rename", "--help"},
			contains: []string{"usage: ito batch rename", "--project", "--json"},
		},
		{
			name:     "batch rm help",
			args:     []string{"batch", "rm", "--help"},
			contains: []string{"usage: ito batch rm", "--project", "--json", "Issues are never deleted"},
		},
		{
			name:     "batch show help",
			args:     []string{"batch", "show", "--help"},
			contains: []string{"usage: ito batch show", "--project", "--json", "derived Waves"},
		},
		{
			name:     "edit help",
			args:     []string{"edit", "--help"},
			contains: []string{"usage: ito edit", "--title", "--priority", "--batch", "--add-label", "--block", "--relate", "--conflict", "--unconflict"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, repo, itoHome, tt.args...)
			if result.exitCode != 0 {
				t.Fatalf("expected help to exit 0, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
			}
			if result.stderr != "" {
				t.Fatalf("expected empty stderr for help, got %q", result.stderr)
			}
			for _, want := range tt.contains {
				if !strings.Contains(result.stdout, want) {
					t.Fatalf("expected help output to contain %q\nstdout: %s", want, result.stdout)
				}
			}
		})
	}
}

func TestBareITOWithoutTTYPrintsRootHelpAndExitsZero(t *testing.T) {
	result := runITO(t, t.TempDir(), t.TempDir())
	if result.exitCode != 0 {
		t.Fatalf("expected bare ito without a TTY to exit 0, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result.stderr != "" {
		t.Fatalf("expected empty stderr, got %q", result.stderr)
	}
	if !strings.Contains(result.stdout, "usage: ito <command> [flags]") {
		t.Fatalf("expected root help on stdout, got %q", result.stdout)
	}
}

func TestBareITOWithTTYLaunchesTUIWithStoreAndResolvedProject(t *testing.T) {
	oldIsTerminal := isTerminal
	oldRunTUI := runTUI
	t.Cleanup(func() {
		isTerminal = oldIsTerminal
		runTUI = oldRunTUI
	})

	isTerminal = func(uintptr) bool {
		return true
	}
	var gotStore *itostore.Store
	var gotProject itostore.Project
	runTUI = func(st *itostore.Store, project itostore.Project) error {
		gotStore = st
		gotProject = project
		return nil
	}

	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st := itostore.New(db)
	rootPath, _, err := resolveCurrentRoot()
	if err != nil {
		t.Fatalf("resolve current root: %v", err)
	}
	if _, err := st.CreateProject("tty-app", "TTY", rootPath); err != nil {
		t.Fatalf("create project: %v", err)
	}
	db.Close()

	exitCode := runCLI(nil)
	if exitCode != 0 {
		t.Fatalf("expected bare ito on a TTY to exit 0, got %d", exitCode)
	}
	if gotStore == nil {
		t.Fatal("expected TUI launcher to receive a store")
	}
	if gotProject.Name != "tty-app" {
		t.Fatalf("expected TUI launcher to receive resolved Project tty-app, got %#v", gotProject)
	}
}

func TestBareITOWithTTYLaunchesTUIWithoutProjectWhenCWDIsUnregistered(t *testing.T) {
	oldIsTerminal := isTerminal
	oldRunTUI := runTUI
	t.Cleanup(func() {
		isTerminal = oldIsTerminal
		runTUI = oldRunTUI
	})

	isTerminal = func(uintptr) bool {
		return true
	}
	var gotStore *itostore.Store
	var gotProject itostore.Project
	runTUI = func(st *itostore.Store, project itostore.Project) error {
		gotStore = st
		gotProject = project
		return nil
	}

	t.Setenv("ITO_HOME", t.TempDir())
	exitCode := runCLI(nil)
	if exitCode != 0 {
		t.Fatalf("expected bare ito on a TTY without a registered cwd Project to exit 0, got %d", exitCode)
	}
	if gotStore == nil {
		t.Fatal("expected TUI launcher to receive a store")
	}
	if gotProject.ID != 0 {
		t.Fatalf("expected TUI launcher to receive no initial Project, got %#v", gotProject)
	}
}

func TestTUISubcommandDoesNotExist(t *testing.T) {
	result := runITO(t, t.TempDir(), t.TempDir(), "tui")
	if result.exitCode != exitBadUsage {
		t.Fatalf("expected ito tui to be an unknown command with exit %d, got %d\nstdout: %s\nstderr: %s", exitBadUsage, result.exitCode, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "unknown command: tui.") {
		t.Fatalf("expected unknown command error, got %q", result.stderr)
	}
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
	if len(issue.Labels) != 0 || len(issue.BlockedBy) != 0 || len(issue.RelatesTo) != 0 || len(issue.ConflictsWith) != 0 {
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

func TestConcurrentMoveAndEditDoNotDeadlock(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "lock-app", "--prefix", "LCK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Hot issue"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
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
			var args []string
			if n%2 == 0 {
				status := "todo"
				if n%4 == 0 {
					status = "in_progress"
				}
				args = []string{"move", "LCK-1", status, "--json"}
			} else {
				args = []string{"edit", "LCK-1", "--title", fmt.Sprintf("Hot issue %02d", n), "--json"}
			}
			results <- runITO(t, repo, itoHome, args...)
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	for result := range results {
		if result.exitCode != 0 {
			t.Fatalf("concurrent write failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
		if strings.Contains(result.stdout, "database is locked") || strings.Contains(result.stderr, "database is locked") {
			t.Fatalf("write hit a lock-upgrade deadlock\nstdout: %s\nstderr: %s", result.stdout, result.stderr)
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
	for _, key := range []string{"id", "project", "title", "status", "priority", "labels", "blocked_by", "relates_to", "conflicts_with", "created", "updated"} {
		if _, ok := raw[0][key]; !ok {
			t.Fatalf("list JSON missing key %q in %s", key, result.stdout)
		}
	}
}

func TestListStyleColorsHumanFieldsWhenEnabled(t *testing.T) {
	style := listStyle{enabled: true}

	if got := style.issueID("ITO-1"); got != "\x1b[36m\x1b[1mITO-1\x1b[0m" {
		t.Fatalf("unexpected ID style: %q", got)
	}
	if got := style.status("backlog"); got != "\x1b[36mbacklog\x1b[0m" {
		t.Fatalf("unexpected Status style: %q", got)
	}
	if got := style.priority("urgent"); got != "\x1b[31m\x1b[1murgent\x1b[0m" {
		t.Fatalf("unexpected urgent Priority style: %q", got)
	}
	if got := style.priority("high"); got != "\x1b[38;5;208m\x1b[1mhigh\x1b[0m" {
		t.Fatalf("unexpected high Priority style: %q", got)
	}
	if got := style.priority("medium"); got != "\x1b[34mmedium\x1b[0m" {
		t.Fatalf("unexpected medium Priority style: %q", got)
	}
	if got := style.priority("low"); got != "\x1b[2mlow\x1b[0m" {
		t.Fatalf("unexpected low Priority style: %q", got)
	}
	if got := style.project("ito"); got != "\x1b[35m\x1b[1mito\x1b[0m" {
		t.Fatalf("unexpected Project style: %q", got)
	}
}

func TestListStyleLeavesHumanFieldsPlainWhenDisabled(t *testing.T) {
	style := listStyle{enabled: false}

	for _, value := range []string{"ITO-1", "backlog", "urgent", "high", "medium", "low", "ito"} {
		if got := style.apply(ansiRed, value); got != value {
			t.Fatalf("disabled style changed %q to %q", value, got)
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

func TestListReadyReturnsOpenIssuesWithOnlyDoneBlockers(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "ready-app", "--prefix", "RDY"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "No blockers", "--status", "backlog"},
		{"new", "--json", "--title", "Done blocker", "--status", "todo"},
		{"new", "--json", "--title", "Review blocker", "--status", "todo"},
		{"new", "--json", "--title", "Done dependency", "--status", "done"},
		{"new", "--json", "--title", "Review dependency", "--status", "in_review"},
		{"new", "--json", "--title", "In progress is not ready", "--status", "in_progress"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "RDY-2", "--block", "RDY-4"); result.exitCode != 0 {
		t.Fatalf("ito edit done blocker failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "RDY-3", "--block", "RDY-5"); result.exitCode != 0 {
		t.Fatalf("ito edit review blocker failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, repo, itoHome, "list", "--ready", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito list --ready failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, result.stdout)); !stringSlicesEqual(got, []string{"RDY-1", "RDY-2"}) {
		t.Fatalf("expected ready frontier IDs [RDY-1 RDY-2], got %v\nstdout: %s", got, result.stdout)
	}
}

func TestListReadyHonoursConflictsWithInJSONAndHumanOutput(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "conflict-ready-app", "--prefix", "CRD"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Low loser", "--status", "todo", "--priority", "low"},
		{"new", "--json", "--title", "High winner", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "Ready unrelated", "--status", "backlog", "--priority", "medium"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "CRD-1", "--conflict", "CRD-2"); result.exitCode != 0 {
		t.Fatalf("ito edit conflict failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	jsonResult := runITO(t, repo, itoHome, "list", "--ready", "--json")
	if jsonResult.exitCode != 0 {
		t.Fatalf("ito list --ready --json failed with exit %d\nstdout: %s\nstderr: %s", jsonResult.exitCode, jsonResult.stdout, jsonResult.stderr)
	}
	jsonIDs := issueIDs(decodeIssueList(t, jsonResult.stdout))
	if !stringSlicesEqual(jsonIDs, []string{"CRD-3", "CRD-2"}) {
		t.Fatalf("expected JSON ready IDs [CRD-3 CRD-2], got %v\nstdout: %s", jsonIDs, jsonResult.stdout)
	}

	humanResult := runITO(t, repo, itoHome, "list", "--ready")
	if humanResult.exitCode != 0 {
		t.Fatalf("ito list --ready failed with exit %d\nstdout: %s\nstderr: %s", humanResult.exitCode, humanResult.stdout, humanResult.stderr)
	}
	if humanIDs := humanIssueIDs(humanResult.stdout); !stringSlicesEqual(humanIDs, jsonIDs) {
		t.Fatalf("expected human ready IDs to match JSON %v, got %v\nstdout: %s", jsonIDs, humanIDs, humanResult.stdout)
	}
}

func TestListReadyComposesWithFiltersAndAllProjects(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "ready-alpha", "--prefix", "RDA"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "ready-beta", "--prefix", "RDB"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := []struct {
		repo string
		args []string
	}{
		{firstRepo, []string{"new", "--json", "--title", "Alpha ready high feature", "--status", "todo", "--priority", "high", "--label", "feature"}},
		{firstRepo, []string{"new", "--json", "--title", "Alpha ready low feature", "--status", "todo", "--priority", "low", "--label", "feature"}},
		{firstRepo, []string{"new", "--json", "--title", "Alpha in progress", "--status", "in_progress", "--priority", "high", "--label", "feature"}},
		{secondRepo, []string{"new", "--json", "--title", "Beta ready high feature", "--status", "backlog", "--priority", "high", "--label", "feature"}},
		{secondRepo, []string{"new", "--json", "--title", "Beta ready high bug", "--status", "todo", "--priority", "high", "--label", "bug"}},
	}
	for _, fixture := range fixtures {
		if result := runITO(t, fixture.repo, itoHome, fixture.args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", fixture.args, result.exitCode, result.stdout, result.stderr)
		}
	}

	filtered := runITO(t, firstRepo, itoHome, "list", "--ready", "--json", "--label", "feature", "--priority", "high")
	if filtered.exitCode != 0 {
		t.Fatalf("ito list --ready with filters failed with exit %d\nstdout: %s\nstderr: %s", filtered.exitCode, filtered.stdout, filtered.stderr)
	}
	if got := issueIDs(decodeIssueList(t, filtered.stdout)); !stringSlicesEqual(got, []string{"RDA-1"}) {
		t.Fatalf("expected filtered ready IDs [RDA-1], got %v\nstdout: %s", got, filtered.stdout)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(filtered.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected one filtered issue, got %s", filtered.stdout)
	}
	if _, ok := raw[0]["body"]; ok {
		t.Fatalf("list --ready JSON must omit body, got %s", filtered.stdout)
	}

	contradictory := runITO(t, firstRepo, itoHome, "list", "--ready", "--json", "--status", "in_progress")
	if contradictory.exitCode != 0 || strings.TrimSpace(contradictory.stdout) != "[]" {
		t.Fatalf("expected contradictory ready status to yield [], got exit=%d stdout=%q stderr=%q", contradictory.exitCode, contradictory.stdout, contradictory.stderr)
	}

	all := runITO(t, t.TempDir(), itoHome, "list", "--ready", "--json", "--all-projects", "--label", "feature", "--priority", "high")
	if all.exitCode != 0 {
		t.Fatalf("ito list --ready --all-projects failed with exit %d\nstdout: %s\nstderr: %s", all.exitCode, all.stdout, all.stderr)
	}
	if got := issueIDs(decodeIssueList(t, all.stdout)); !stringSlicesEqual(got, []string{"RDA-1", "RDB-1"}) {
		t.Fatalf("expected all-projects ready IDs [RDA-1 RDB-1], got %v\nstdout: %s", got, all.stdout)
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

func TestBatchNewCreatesBatchAndRejectsInvalidOrDuplicateNames(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "batch-app", "--prefix", "BAT"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	created := runITO(t, repo, itoHome, "batch", "new", "refactor", "--json")
	if created.exitCode != 0 {
		t.Fatalf("ito batch new failed with exit %d\nstdout: %s\nstderr: %s", created.exitCode, created.stdout, created.stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(created.stdout), &raw); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\nstdout: %s", err, created.stdout)
	}
	for _, key := range []string{"name", "project", "created"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("batch new JSON missing key %q in %s", key, created.stdout)
		}
	}
	if len(raw) != 3 {
		t.Fatalf("batch new JSON must contain only name, project and created, got %s", created.stdout)
	}
	var batch batchJSON
	if err := json.Unmarshal([]byte(created.stdout), &batch); err != nil {
		t.Fatalf("stdout is not a Batch JSON object: %v\nstdout: %s", err, created.stdout)
	}
	if batch.Name != "refactor" || batch.Project != "batch-app" {
		t.Fatalf("unexpected batch identity: %#v", batch)
	}
	createdAt, err := time.Parse(time.RFC3339, batch.Created)
	if err != nil {
		t.Fatalf("created timestamp must be RFC3339, got %q: %v", batch.Created, err)
	}
	if createdAt.Location() != time.UTC {
		t.Fatalf("created timestamp must be UTC, got %q", batch.Created)
	}

	human := runITO(t, repo, itoHome, "batch", "new", "cleanup")
	if human.exitCode != 0 || human.stdout != "cleanup\n" || human.stderr != "" {
		t.Fatalf("expected human batch new to print only the name, got exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}

	duplicate := runITO(t, repo, itoHome, "batch", "new", "refactor", "--json")
	if duplicate.exitCode != exitBadUsage || duplicate.stdout != "" {
		t.Fatalf("duplicate batch must fail with exit 2 and no stdout, got exit=%d stdout=%q stderr=%q", duplicate.exitCode, duplicate.stdout, duplicate.stderr)
	}
	envelope := decodeErrorEnvelope(t, duplicate.stderr)
	if envelope.Code != exitBadUsage || !strings.Contains(envelope.Error, "already exists") || envelope.Hint == "" {
		t.Fatalf("expected actionable duplicate error, got %#v", envelope)
	}

	invalid := runITO(t, repo, itoHome, "batch", "new", "Bad_Name", "--json")
	if invalid.exitCode != exitBadUsage || invalid.stdout != "" {
		t.Fatalf("invalid batch name must fail with exit 2 and no stdout, got exit=%d stdout=%q stderr=%q", invalid.exitCode, invalid.stdout, invalid.stderr)
	}
	envelope = decodeErrorEnvelope(t, invalid.stderr)
	if envelope.Code != exitBadUsage || !strings.Contains(envelope.Error, "invalid batch name") || envelope.Hint == "" {
		t.Fatalf("expected actionable invalid-name error, got %#v", envelope)
	}
}

func TestBatchNamesAreProjectScopedAndListNewestFirst(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "batch-alpha", "--prefix", "BAA"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "batch-beta", "--prefix", "BAB"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, fixture := range []struct {
		repo string
		name string
	}{
		{firstRepo, "refactor"},
		{secondRepo, "refactor"},
		{firstRepo, "cleanup"},
	} {
		if result := runITO(t, fixture.repo, itoHome, "batch", "new", fixture.name, "--json"); result.exitCode != 0 {
			t.Fatalf("ito batch new %s failed with exit %d\nstdout: %s\nstderr: %s", fixture.name, result.exitCode, result.stdout, result.stderr)
		}
	}

	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
UPDATE batches SET created = '2026-06-12T10:00:00Z' WHERE name = 'refactor' AND project_id = (SELECT id FROM projects WHERE name = 'batch-alpha');
UPDATE batches SET created = '2026-06-12T12:00:00Z' WHERE name = 'cleanup' AND project_id = (SELECT id FROM projects WHERE name = 'batch-alpha');
UPDATE batches SET created = '2026-06-12T11:00:00Z' WHERE name = 'refactor' AND project_id = (SELECT id FROM projects WHERE name = 'batch-beta');
`); err != nil {
		t.Fatal(err)
	}

	listed := runITO(t, firstRepo, itoHome, "batch", "list", "--json")
	if listed.exitCode != 0 {
		t.Fatalf("ito batch list failed with exit %d\nstdout: %s\nstderr: %s", listed.exitCode, listed.stdout, listed.stderr)
	}
	batches := decodeBatchList(t, listed.stdout)
	if got := batchNames(batches); !stringSlicesEqual(got, []string{"cleanup", "refactor"}) {
		t.Fatalf("expected newest-first alpha batches [cleanup refactor], got %v\nstdout: %s", got, listed.stdout)
	}
	for _, batch := range batches {
		if batch.Project != "batch-alpha" || batch.Total != 0 || batch.Done != 0 {
			t.Fatalf("expected alpha batch with 0/0 progress, got %#v", batch)
		}
		if batch.Waves == nil || *batch.Waves != 0 {
			t.Fatalf("empty batch must report waves=0, got %#v", batch)
		}
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(listed.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "project", "created", "total", "done", "waves"} {
		if _, ok := raw[0][key]; !ok {
			t.Fatalf("batch list JSON missing key %q in %s", key, listed.stdout)
		}
	}

	human := runITO(t, firstRepo, itoHome, "batch", "list")
	if human.exitCode != 0 || human.stderr != "" {
		t.Fatalf("ito batch list human failed with exit %d\nstdout: %s\nstderr: %s", human.exitCode, human.stdout, human.stderr)
	}
	if !strings.Contains(human.stdout, "cleanup 2026-06-12 0/0") || !strings.Contains(human.stdout, "refactor 2026-06-12 0/0") {
		t.Fatalf("human batch list must show date and progress, got %q", human.stdout)
	}
	if strings.Index(human.stdout, "cleanup") > strings.Index(human.stdout, "refactor") {
		t.Fatalf("human batch list must be newest-first, got %q", human.stdout)
	}

	explicit := runITO(t, t.TempDir(), itoHome, "batch", "list", "--project", "batch-beta", "--json")
	if explicit.exitCode != 0 {
		t.Fatalf("ito batch list --project failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}
	if got := batchNames(decodeBatchList(t, explicit.stdout)); !stringSlicesEqual(got, []string{"refactor"}) {
		t.Fatalf("expected beta to own its own refactor batch, got %v", got)
	}
}

func TestBatchListShowsWaveProgressAndDegradesCyclicBatches(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "batch-list-waves", "--prefix", "BLW"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, name := range []string{"empty", "complete", "refactor", "cycle"} {
		if result := runITO(t, repo, itoHome, "batch", "new", name, "--json"); result.exitCode != 0 {
			t.Fatalf("ito batch new %s failed with exit %d\nstdout: %s\nstderr: %s", name, result.exitCode, result.stdout, result.stderr)
		}
	}
	for _, args := range [][]string{
		{"new", "--json", "--title", "Done work", "--batch", "complete", "--status", "done"},
		{"new", "--json", "--title", "First wave", "--batch", "refactor", "--status", "todo"},
		{"new", "--json", "--title", "Second wave", "--batch", "refactor", "--status", "todo"},
		{"new", "--json", "--title", "Cycle one", "--batch", "cycle", "--status", "todo"},
		{"new", "--json", "--title", "Cycle two", "--batch", "cycle", "--status", "todo"},
		{"new", "--json", "--title", "Cycle three", "--batch", "cycle", "--status", "todo"},
	} {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	for _, args := range [][]string{
		{"edit", "--json", "BLW-3", "--block", "BLW-2"},
		{"edit", "--json", "BLW-4", "--block", "BLW-5"},
		{"edit", "--json", "BLW-5", "--block", "BLW-6"},
		{"edit", "--json", "BLW-6", "--block", "BLW-4"},
	} {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}

	listed := runITO(t, repo, itoHome, "batch", "list", "--json")
	if listed.exitCode != 0 {
		t.Fatalf("ito batch list --json failed with exit %d\nstdout: %s\nstderr: %s", listed.exitCode, listed.stdout, listed.stderr)
	}
	batches := decodeBatchList(t, listed.stdout)
	for _, tt := range []struct {
		name  string
		total int
		done  int
		waves *int
	}{
		{name: "empty", total: 0, done: 0, waves: intPtr(0)},
		{name: "complete", total: 1, done: 1, waves: intPtr(0)},
		{name: "refactor", total: 2, done: 0, waves: intPtr(2)},
		{name: "cycle", total: 3, done: 0, waves: nil},
	} {
		batch, ok := findBatchJSON(batches, tt.name)
		if !ok {
			t.Fatalf("missing batch %q in %s", tt.name, listed.stdout)
		}
		if batch.Total != tt.total || batch.Done != tt.done {
			t.Fatalf("unexpected progress for %s: %#v", tt.name, batch)
		}
		if tt.waves == nil {
			if batch.Waves != nil {
				t.Fatalf("cyclic batch %s must report waves=null, got %#v", tt.name, batch)
			}
			continue
		}
		if batch.Waves == nil || *batch.Waves != *tt.waves {
			t.Fatalf("batch %s must report waves=%d, got %#v", tt.name, *tt.waves, batch)
		}
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(listed.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, item := range raw {
		if _, ok := item["waves"]; !ok {
			t.Fatalf("batch list JSON missing waves key in %s", listed.stdout)
		}
		var name string
		if err := json.Unmarshal(item["name"], &name); err != nil {
			t.Fatal(err)
		}
		if name == "cycle" && string(item["waves"]) != "null" {
			t.Fatalf("cyclic batch must encode waves as null, got %s", item["waves"])
		}
	}

	human := runITO(t, repo, itoHome, "batch", "list")
	if human.exitCode != 0 || human.stderr != "" {
		t.Fatalf("ito batch list human failed with exit %d\nstdout: %s\nstderr: %s", human.exitCode, human.stdout, human.stderr)
	}
	for _, want := range []string{
		"0/0 done",
		"done",
		"0/2 done · wave 1/2",
		"0/3 done · cycle",
	} {
		if !strings.Contains(human.stdout, want) {
			t.Fatalf("human batch list missing %q in %q", want, human.stdout)
		}
	}
}

func TestBatchListEmptyAndUnregisteredCwd(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "empty-batches", "--prefix", "EBT"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	jsonResult := runITO(t, repo, itoHome, "batch", "list", "--json")
	if jsonResult.exitCode != 0 || strings.TrimSpace(jsonResult.stdout) != "[]" || jsonResult.stderr != "" {
		t.Fatalf("expected JSON empty success, got exit=%d stdout=%q stderr=%q", jsonResult.exitCode, jsonResult.stdout, jsonResult.stderr)
	}
	human := runITO(t, repo, itoHome, "batch", "list")
	if human.exitCode != 0 || !strings.Contains(human.stdout, "no batches. create one with 'ito batch new <name>'.") || human.stderr != "" {
		t.Fatalf("expected human empty success, got exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}

	unregisteredNew := runITO(t, t.TempDir(), itoHome, "batch", "new", "refactor", "--json")
	if unregisteredNew.exitCode != exitNotRegistered || unregisteredNew.stdout != "" {
		t.Fatalf("batch new outside a Project must fail with exit 4, got exit=%d stdout=%q stderr=%q", unregisteredNew.exitCode, unregisteredNew.stdout, unregisteredNew.stderr)
	}
	envelope := decodeErrorEnvelope(t, unregisteredNew.stderr)
	if envelope.Code != exitNotRegistered || !strings.Contains(envelope.Hint, "ito init") {
		t.Fatalf("expected init hint outside a Project, got %#v", envelope)
	}

	unregisteredList := runITO(t, t.TempDir(), itoHome, "batch", "list", "--json")
	if unregisteredList.exitCode != exitNotRegistered || unregisteredList.stdout != "" {
		t.Fatalf("batch list outside a Project must fail with exit 4, got exit=%d stdout=%q stderr=%q", unregisteredList.exitCode, unregisteredList.stdout, unregisteredList.stderr)
	}
}

func TestIssueBatchMembershipTravelsThroughNewEditListProgressAndDelete(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "member-app", "--prefix", "MBR"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, name := range []string{"alpha", "beta"} {
		if result := runITO(t, repo, itoHome, "batch", "new", name, "--json"); result.exitCode != 0 {
			t.Fatalf("ito batch new %s failed with exit %d\nstdout: %s\nstderr: %s", name, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Outside"); result.exitCode != 0 {
		t.Fatalf("ito new outside failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	member := runITO(t, repo, itoHome, "new", "--json", "--title", "Alpha feature", "--batch", "alpha", "--status", "todo", "--priority", "high", "--label", "feature")
	if member.exitCode != 0 {
		t.Fatalf("ito new --batch failed with exit %d\nstdout: %s\nstderr: %s", member.exitCode, member.stdout, member.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(member.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, member.stdout)
	}
	if got := issueBatchName(issue); got != "alpha" {
		t.Fatalf("new --batch must report batch alpha, got %q in %#v", got, issue)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Done alpha", "--batch", "alpha", "--status", "done", "--label", "feature"); result.exitCode != 0 {
		t.Fatalf("ito new done member failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Other label", "--batch", "alpha", "--status", "todo", "--label", "bug"); result.exitCode != 0 {
		t.Fatalf("ito new other-label member failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	showOutside := runITO(t, repo, itoHome, "show", "--json", "MBR-1")
	if showOutside.exitCode != 0 {
		t.Fatalf("ito show outside failed with exit %d\nstdout: %s\nstderr: %s", showOutside.exitCode, showOutside.stdout, showOutside.stderr)
	}
	if err := json.Unmarshal([]byte(showOutside.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, showOutside.stdout)
	}
	if issue.Batch != nil {
		t.Fatalf("issue outside any Batch must report batch=null, got %#v", issue.Batch)
	}

	listed := runITO(t, repo, itoHome, "list", "--json", "--batch", "alpha")
	if listed.exitCode != 0 {
		t.Fatalf("ito list --batch failed with exit %d\nstdout: %s\nstderr: %s", listed.exitCode, listed.stdout, listed.stderr)
	}
	if got := issueIDs(decodeIssueList(t, listed.stdout)); !stringSlicesEqual(got, []string{"MBR-2", "MBR-4"}) {
		t.Fatalf("list --batch must hide done by default and include only alpha members, got %v\nstdout: %s", got, listed.stdout)
	}
	filtered := runITO(t, repo, itoHome, "list", "--json", "--batch", "alpha", "--status", "todo", "--priority", "high", "--label", "feature", "--search", "feature", "--ready")
	if filtered.exitCode != 0 {
		t.Fatalf("ito list --batch with filters failed with exit %d\nstdout: %s\nstderr: %s", filtered.exitCode, filtered.stdout, filtered.stderr)
	}
	if got := issueIDs(decodeIssueList(t, filtered.stdout)); !stringSlicesEqual(got, []string{"MBR-2"}) {
		t.Fatalf("list --batch must AND-combine filters, got %v\nstdout: %s", got, filtered.stdout)
	}

	progress := decodeBatchList(t, runITO(t, repo, itoHome, "batch", "list", "--json").stdout)
	alpha, ok := findBatchJSON(progress, "alpha")
	if !ok || alpha.Done != 1 || alpha.Total != 3 {
		t.Fatalf("expected alpha progress 1/3, got %#v in %#v", alpha, progress)
	}

	moved := runITO(t, repo, itoHome, "edit", "--json", "MBR-2", "--batch", "beta")
	if moved.exitCode != 0 {
		t.Fatalf("ito edit --batch beta failed with exit %d\nstdout: %s\nstderr: %s", moved.exitCode, moved.stdout, moved.stderr)
	}
	if err := json.Unmarshal([]byte(moved.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, moved.stdout)
	}
	if got := issueBatchName(issue); got != "beta" {
		t.Fatalf("edit --batch must replace membership with beta, got %q in %#v", got, issue)
	}
	updatedAfterMove := issue.Updated
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--batch", "beta").stdout)); !stringSlicesEqual(got, []string{"MBR-2"}) {
		t.Fatalf("list --batch beta must include moved issue only, got %v", got)
	}

	unchanged := runITO(t, repo, itoHome, "edit", "MBR-2", "--batch", "beta")
	if unchanged.exitCode != 0 || !strings.Contains(unchanged.stdout, "unchanged") {
		t.Fatalf("idempotent batch edit must exit 0 and say unchanged, got exit=%d stdout=%q stderr=%q", unchanged.exitCode, unchanged.stdout, unchanged.stderr)
	}
	show := runITO(t, repo, itoHome, "show", "--json", "MBR-2")
	if show.exitCode != 0 {
		t.Fatalf("ito show moved issue failed with exit %d\nstdout: %s\nstderr: %s", show.exitCode, show.stdout, show.stderr)
	}
	if err := json.Unmarshal([]byte(show.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, show.stdout)
	}
	if issue.Updated != updatedAfterMove {
		t.Fatalf("idempotent batch edit must preserve updated, got %q want %q", issue.Updated, updatedAfterMove)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET updated = '2026-05-24T10:00:00Z' WHERE id = 'MBR-2'`); err != nil {
		t.Fatal(err)
	}

	cleared := runITO(t, repo, itoHome, "edit", "--json", "MBR-2", "--batch", "")
	if cleared.exitCode != 0 {
		t.Fatalf("ito edit --batch empty failed with exit %d\nstdout: %s\nstderr: %s", cleared.exitCode, cleared.stdout, cleared.stderr)
	}
	if err := json.Unmarshal([]byte(cleared.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, cleared.stdout)
	}
	if issue.Batch != nil || issue.Updated == "2026-05-24T10:00:00Z" {
		t.Fatalf("clearing batch must remove membership and stamp updated, got %#v", issue)
	}

	if result := runITO(t, repo, itoHome, "rm", "--json", "MBR-3"); result.exitCode != 0 {
		t.Fatalf("ito rm batch member failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	progress = decodeBatchList(t, runITO(t, repo, itoHome, "batch", "list", "--json").stdout)
	alpha, ok = findBatchJSON(progress, "alpha")
	if !ok || alpha.Done != 0 || alpha.Total != 1 {
		t.Fatalf("deleting a member must keep the Batch with one fewer member, got %#v in %#v", alpha, progress)
	}
}

func TestBatchRenameKeepsIdentityMembershipAndProjectScope(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "rename-first", "--prefix", "BRF"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "rename-second", "--prefix", "BRS"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, name := range []string{"storage-refactor", "existing"} {
		if result := runITO(t, firstRepo, itoHome, "batch", "new", name, "--json"); result.exitCode != 0 {
			t.Fatalf("batch new %s failed with exit %d\nstdout: %s\nstderr: %s", name, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, secondRepo, itoHome, "batch", "new", "storage-refactor", "--json"); result.exitCode != 0 {
		t.Fatalf("second project batch new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Member", "--batch", "storage-refactor", "--status", "todo"); result.exitCode != 0 {
		t.Fatalf("member create failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Done member", "--batch", "storage-refactor", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("done member create failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
UPDATE batches SET created = '2026-06-12T10:00:00Z' WHERE name = 'storage-refactor' AND project_id = (SELECT id FROM projects WHERE name = 'rename-first');
UPDATE issues SET updated = '2026-06-12T11:00:00Z' WHERE id = 'BRF-1';
`); err != nil {
		t.Fatal(err)
	}

	renamed := runITO(t, firstRepo, itoHome, "batch", "rename", "storage-refactor", "store-rework", "--json")
	if renamed.exitCode != 0 {
		t.Fatalf("batch rename failed with exit %d\nstdout: %s\nstderr: %s", renamed.exitCode, renamed.stdout, renamed.stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(renamed.stdout), &raw); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\nstdout: %s", err, renamed.stdout)
	}
	for _, key := range []string{"name", "project", "created", "total", "done"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("batch rename JSON missing key %q in %s", key, renamed.stdout)
		}
	}
	if len(raw) != 5 {
		t.Fatalf("batch rename JSON must contain only name, project, created, total and done, got %s", renamed.stdout)
	}
	var batch batchJSON
	if err := json.Unmarshal([]byte(renamed.stdout), &batch); err != nil {
		t.Fatalf("stdout is not a Batch JSON object: %v\nstdout: %s", err, renamed.stdout)
	}
	if batch.Name != "store-rework" || batch.Project != "rename-first" || batch.Created != "2026-06-12T10:00:00Z" || batch.Total != 2 || batch.Done != 1 {
		t.Fatalf("rename must keep created and progress with the new name, got %#v", batch)
	}
	show := runITO(t, firstRepo, itoHome, "show", "--json", "BRF-1")
	if show.exitCode != 0 {
		t.Fatalf("show member failed with exit %d\nstdout: %s\nstderr: %s", show.exitCode, show.stdout, show.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(show.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, show.stdout)
	}
	if got := issueBatchName(issue); got != "store-rework" || issue.Updated != "2026-06-12T11:00:00Z" {
		t.Fatalf("rename must update visible membership without stamping Issue updated, got %#v", issue)
	}

	human := runITO(t, firstRepo, itoHome, "batch", "rename", "store-rework", "storage-refactor")
	if human.exitCode != 0 || human.stdout != "store-rework renamed to storage-refactor.\n" || human.stderr != "" {
		t.Fatalf("unexpected human rename output: exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
	if explicit := runITO(t, t.TempDir(), itoHome, "batch", "rename", "--project", "rename-second", "storage-refactor", "second-rework", "--json"); explicit.exitCode != 0 {
		t.Fatalf("batch rename --project failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "invalid new name", args: []string{"batch", "rename", "--json", "storage-refactor", "Bad_Name"}, code: exitBadUsage},
		{name: "collision", args: []string{"batch", "rename", "--json", "storage-refactor", "existing"}, code: exitBadUsage},
		{name: "unknown old name", args: []string{"batch", "rename", "--json", "missing", "new-name"}, code: exitNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, firstRepo, itoHome, tt.args...)
			if result.exitCode != tt.code || result.stdout != "" {
				t.Fatalf("expected exit %d and no stdout, got exit=%d stdout=%q stderr=%q", tt.code, result.exitCode, result.stdout, result.stderr)
			}
			envelope := decodeErrorEnvelope(t, result.stderr)
			if envelope.Code != tt.code {
				t.Fatalf("expected envelope code %d, got %#v", tt.code, envelope)
			}
		})
	}
}

func TestBatchRMReleasesMembersWithoutDeletingIssuesAndHonoursProject(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "delete-first", "--prefix", "BDF"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "delete-second", "--prefix", "BDS"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, name := range []string{"storage-refactor", "empty-batch"} {
		if result := runITO(t, firstRepo, itoHome, "batch", "new", name, "--json"); result.exitCode != 0 {
			t.Fatalf("first batch new %s failed with exit %d\nstdout: %s\nstderr: %s", name, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, secondRepo, itoHome, "batch", "new", "storage-refactor", "--json"); result.exitCode != 0 {
		t.Fatalf("second batch new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, title := range []string{"First member", "Second member"} {
		if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", title, "--batch", "storage-refactor"); result.exitCode != 0 {
			t.Fatalf("member create failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Outside"); result.exitCode != 0 {
		t.Fatalf("outside create failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET updated = '2026-06-12T10:00:00Z' WHERE id IN ('BDF-1', 'BDF-2', 'BDF-3')`); err != nil {
		t.Fatal(err)
	}

	deleted := runITO(t, firstRepo, itoHome, "batch", "rm", "--json", "storage-refactor")
	if deleted.exitCode != 0 {
		t.Fatalf("batch rm failed with exit %d\nstdout: %s\nstderr: %s", deleted.exitCode, deleted.stdout, deleted.stderr)
	}
	var deletion deletedBatchJSON
	if err := json.Unmarshal([]byte(deleted.stdout), &deletion); err != nil {
		t.Fatalf("stdout is not a deletion JSON object: %v\nstdout: %s", err, deleted.stdout)
	}
	if deletion.Deleted != "storage-refactor" || deletion.MembersCleared != 2 {
		t.Fatalf("unexpected deletion JSON: %#v", deletion)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(deleted.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("batch rm JSON must contain only deleted and members_cleared, got %s", deleted.stdout)
	}
	for _, id := range []string{"BDF-1", "BDF-2", "BDF-3"} {
		show := runITO(t, firstRepo, itoHome, "show", "--json", id)
		if show.exitCode != 0 {
			t.Fatalf("show %s failed with exit %d\nstdout: %s\nstderr: %s", id, show.exitCode, show.stdout, show.stderr)
		}
		var issue issueJSON
		if err := json.Unmarshal([]byte(show.stdout), &issue); err != nil {
			t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, show.stdout)
		}
		if id == "BDF-3" {
			if issue.Batch != nil || issue.Updated != "2026-06-12T10:00:00Z" {
				t.Fatalf("outside Issue must stay outside and untouched, got %#v", issue)
			}
			continue
		}
		if issue.Batch != nil || issue.Updated == "2026-06-12T10:00:00Z" {
			t.Fatalf("former member %s must survive with batch=null and stamped updated, got %#v", id, issue)
		}
	}
	listed := runITO(t, firstRepo, itoHome, "batch", "list", "--json")
	if listed.exitCode != 0 {
		t.Fatalf("batch list failed with exit %d\nstdout: %s\nstderr: %s", listed.exitCode, listed.stdout, listed.stderr)
	}
	if _, ok := findBatchJSON(decodeBatchList(t, listed.stdout), "storage-refactor"); ok {
		t.Fatalf("deleted Batch must be absent from list, got %s", listed.stdout)
	}
	unknown := runITO(t, firstRepo, itoHome, "batch", "rm", "--json", "storage-refactor")
	if unknown.exitCode != exitNotFound || unknown.stdout != "" {
		t.Fatalf("unknown batch rm must fail with exit 3 and no stdout, got exit=%d stdout=%q stderr=%q", unknown.exitCode, unknown.stdout, unknown.stderr)
	}
	envelope := decodeErrorEnvelope(t, unknown.stderr)
	if envelope.Code != exitNotFound || !strings.Contains(envelope.Hint, "ito batch list") {
		t.Fatalf("expected unknown Batch error with batch-list hint, got %#v", envelope)
	}

	human := runITO(t, firstRepo, itoHome, "batch", "rm", "empty-batch")
	if human.exitCode != 0 || human.stdout != "empty-batch deleted. 0 members released.\n" || human.stderr != "" {
		t.Fatalf("unexpected human rm output: exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
	explicit := runITO(t, t.TempDir(), itoHome, "batch", "rm", "--project", "delete-second", "storage-refactor", "--json")
	if explicit.exitCode != 0 {
		t.Fatalf("batch rm --project failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}
}

func TestIssueBatchMembershipValidationAndProjectScope(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "batch-first", "--prefix", "BFI"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "batch-second", "--prefix", "BSE"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "batch", "new", "remote", "--json"); result.exitCode != 0 {
		t.Fatalf("second batch new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Editable"); result.exitCode != 0 {
		t.Fatalf("first new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "new empty batch", args: []string{"new", "--json", "--title", "Bad empty", "--batch", ""}, code: exitBadUsage},
		{name: "new unknown batch", args: []string{"new", "--json", "--title", "Bad", "--batch", "missing"}, code: exitNotFound},
		{name: "edit unknown batch", args: []string{"edit", "--json", "BFI-1", "--batch", "missing"}, code: exitNotFound},
		{name: "list unknown batch", args: []string{"list", "--json", "--batch", "missing"}, code: exitNotFound},
		{name: "other project batch unreachable", args: []string{"new", "--json", "--title", "Cross", "--batch", "remote"}, code: exitNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runITO(t, firstRepo, itoHome, tt.args...)
			if result.exitCode != tt.code || result.stdout != "" {
				t.Fatalf("expected exit %d and no stdout, got exit=%d stdout=%q stderr=%q", tt.code, result.exitCode, result.stdout, result.stderr)
			}
			envelope := decodeErrorEnvelope(t, result.stderr)
			if envelope.Code != tt.code {
				t.Fatalf("expected envelope code %d, got %#v", tt.code, envelope)
			}
			if tt.code == exitNotFound && !strings.Contains(envelope.Hint, "ito batch list") {
				t.Fatalf("unknown Batch errors need batch-list hint, got %#v", envelope)
			}
		})
	}
}

func TestBatchShowDerivesWavesAndMatchesReadyList(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "batch-show-app", "--prefix", "BSH"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "batch", "new", "refactor", "--json"); result.exitCode != 0 {
		t.Fatalf("ito batch new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Low loser", "--batch", "refactor", "--status", "todo", "--priority", "low"},
		{"new", "--json", "--title", "High winner", "--batch", "refactor", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "Later work", "--batch", "refactor", "--status", "todo", "--priority", "medium"},
		{"new", "--json", "--title", "Done work", "--batch", "refactor", "--status", "done", "--priority", "urgent"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "BSH-1", "--conflict", "BSH-2"); result.exitCode != 0 {
		t.Fatalf("ito edit conflict failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "BSH-3", "--block", "BSH-2"); result.exitCode != 0 {
		t.Fatalf("ito edit block failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	shown := runITO(t, repo, itoHome, "batch", "show", "refactor", "--json")
	if shown.exitCode != 0 {
		t.Fatalf("ito batch show failed with exit %d\nstdout: %s\nstderr: %s", shown.exitCode, shown.stdout, shown.stderr)
	}
	plan := decodeBatchShow(t, shown.stdout)
	if plan.Name != "refactor" || plan.Project != "batch-show-app" || plan.Total != 4 || plan.Done != 1 {
		t.Fatalf("unexpected batch show identity/progress: %#v", plan)
	}
	if len(plan.Waves) != 2 {
		t.Fatalf("expected two waves, got %#v", plan.Waves)
	}
	if !plan.Waves[0].Ready || plan.Waves[1].Ready {
		t.Fatalf("only wave 1 should be marked ready, got %#v", plan.Waves)
	}
	if got := issueIDs(plan.Waves[0].Issues); !stringSlicesEqual(got, []string{"BSH-2"}) {
		t.Fatalf("expected conflict winner in wave 1, got %v\nstdout: %s", got, shown.stdout)
	}
	if got := issueIDs(plan.Waves[1].Issues); !stringSlicesEqual(got, []string{"BSH-3", "BSH-1"}) {
		t.Fatalf("expected dependent work and conflict loser in wave 2, got %v\nstdout: %s", got, shown.stdout)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(shown.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "project", "created", "total", "done", "waves"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("batch show JSON missing key %q in %s", key, shown.stdout)
		}
	}
	var rawWaves []map[string]json.RawMessage
	if err := json.Unmarshal(raw["waves"], &rawWaves); err != nil {
		t.Fatal(err)
	}
	var rawIssues []map[string]json.RawMessage
	if err := json.Unmarshal(rawWaves[0]["issues"], &rawIssues); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawIssues[0]["body"]; ok {
		t.Fatalf("batch show issue JSON must omit body, got %s", shown.stdout)
	}

	ready := runITO(t, repo, itoHome, "list", "--json", "--batch", "refactor", "--ready")
	if ready.exitCode != 0 {
		t.Fatalf("ito list --batch --ready failed with exit %d\nstdout: %s\nstderr: %s", ready.exitCode, ready.stdout, ready.stderr)
	}
	if got, want := issueIDs(plan.Waves[0].Issues), issueIDs(decodeIssueList(t, ready.stdout)); !stringSlicesEqual(got, want) {
		t.Fatalf("batch show wave 1 must match list --batch --ready, wave=%v ready=%v", got, want)
	}

	human := runITO(t, repo, itoHome, "batch", "show", "refactor")
	if human.exitCode != 0 || human.stderr != "" {
		t.Fatalf("ito batch show human failed with exit %d\nstdout: %s\nstderr: %s", human.exitCode, human.stdout, human.stderr)
	}
	if !strings.Contains(human.stdout, "refactor ") || !strings.Contains(human.stdout, "1/4") || !strings.Contains(human.stdout, "Wave 1") || !strings.Contains(human.stdout, "ready") || !strings.Contains(human.stdout, "Wave 2") || !strings.Contains(human.stdout, "waiting") {
		t.Fatalf("human batch show must include progress and wave headings, got %q", human.stdout)
	}
}

func TestBatchShowPrototypeConflictLoserCanPrecedeBlockedWinner(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "prototype-show", "--prefix", "PSH"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "batch", "new", "storage-refactor", "--json"); result.exitCode != 0 {
		t.Fatalf("ito batch new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, args := range [][]string{
		{"new", "--json", "--title", "extract config loader", "--batch", "storage-refactor", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "migrate writes to new store", "--batch", "storage-refactor", "--status", "todo", "--priority", "high"},
		{"new", "--json", "--title", "port FTS triggers", "--batch", "storage-refactor", "--status", "todo", "--priority", "medium"},
	} {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "PSH-2", "--block", "PSH-1"); result.exitCode != 0 {
		t.Fatalf("ito edit block failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "PSH-3", "--conflict", "PSH-2"); result.exitCode != 0 {
		t.Fatalf("ito edit conflict failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	plan := decodeBatchShow(t, runITO(t, repo, itoHome, "batch", "show", "storage-refactor", "--json").stdout)
	if got := issueIDs(plan.Waves[0].Issues); !stringSlicesEqual(got, []string{"PSH-1", "PSH-3"}) {
		t.Fatalf("expected blocker and conflict loser in wave 1, got %v", got)
	}
	if got := issueIDs(plan.Waves[1].Issues); !stringSlicesEqual(got, []string{"PSH-2"}) {
		t.Fatalf("expected blocked conflict winner in wave 2, got %v", got)
	}
}

func TestBatchShowUnknownCompleteAndCycleErrors(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "batch-errors", "--prefix", "BER"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	unknown := runITO(t, repo, itoHome, "batch", "show", "missing", "--json")
	if unknown.exitCode != exitNotFound || unknown.stdout != "" {
		t.Fatalf("unknown batch show must fail exit 3 with no stdout, got exit=%d stdout=%q stderr=%q", unknown.exitCode, unknown.stdout, unknown.stderr)
	}
	envelope := decodeErrorEnvelope(t, unknown.stderr)
	if envelope.Code != exitNotFound || !strings.Contains(envelope.Hint, "ito batch list") {
		t.Fatalf("unexpected unknown batch error: %#v", envelope)
	}

	if result := runITO(t, repo, itoHome, "batch", "new", "complete", "--json"); result.exitCode != 0 {
		t.Fatalf("ito batch new complete failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Done", "--batch", "complete", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("ito new done failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	completeResult := runITO(t, repo, itoHome, "batch", "show", "complete", "--json")
	if completeResult.exitCode != 0 {
		t.Fatalf("complete batch show failed with exit %d\nstdout: %s\nstderr: %s", completeResult.exitCode, completeResult.stdout, completeResult.stderr)
	}
	complete := decodeBatchShow(t, completeResult.stdout)
	if complete.Total != 1 || complete.Done != 1 || len(complete.Waves) != 0 {
		t.Fatalf("complete batch must show full progress and no waves, got %#v", complete)
	}

	if result := runITO(t, repo, itoHome, "batch", "new", "cycle", "--json"); result.exitCode != 0 {
		t.Fatalf("ito batch new cycle failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, title := range []string{"First", "Second", "Third"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title, "--batch", "cycle", "--status", "todo"); result.exitCode != 0 {
			t.Fatalf("ito new %s failed with exit %d\nstdout: %s\nstderr: %s", title, result.exitCode, result.stdout, result.stderr)
		}
	}
	for _, args := range [][]string{
		{"edit", "--json", "BER-2", "--block", "BER-3"},
		{"edit", "--json", "BER-3", "--block", "BER-4"},
		{"edit", "--json", "BER-4", "--block", "BER-2"},
	} {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	cycle := runITO(t, repo, itoHome, "batch", "show", "cycle", "--json")
	if cycle.exitCode != exitGeneric || cycle.stdout != "" {
		t.Fatalf("cycle batch show must fail exit 1 with no stdout, got exit=%d stdout=%q stderr=%q", cycle.exitCode, cycle.stdout, cycle.stderr)
	}
	envelope = decodeErrorEnvelope(t, cycle.stderr)
	for _, id := range []string{"BER-2", "BER-3", "BER-4"} {
		if !strings.Contains(envelope.Error, id) {
			t.Fatalf("cycle error must name %s, got %#v", id, envelope)
		}
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
	for _, title := range []string{"Linked", "Blocker", "Related", "Conflicting"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title, "--label", "feature"); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'LNK-1', 'LNK-2', 'blocked_by');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'LNK-3', 'LNK-1', 'relates_to');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'LNK-1', 'LNK-4', 'conflicts_with');
`, project.ID, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, repo, itoHome, "list", "--json", "--label", "feature")
	if result.exitCode != 0 {
		t.Fatalf("ito list failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	issues := decodeIssueList(t, result.stdout)
	linked, found := findIssueJSON(issues, "LNK-1")
	if !found {
		t.Fatalf("expected linked Issue LNK-1 in list, got %#v", issues)
	}
	if !stringSlicesEqual(linked.Labels, []string{"feature"}) || !stringSlicesEqual(linked.BlockedBy, []string{"LNK-2"}) || !stringSlicesEqual(linked.RelatesTo, []string{"LNK-3"}) || !stringSlicesEqual(linked.ConflictsWith, []string{"LNK-4"}) {
		t.Fatalf("expected stable labels and links on linked issue, got %#v", linked)
	}
	blocker, found := findIssueJSON(issues, "LNK-2")
	if !found {
		t.Fatalf("expected blocker Issue LNK-2 in list, got %#v", issues)
	}
	if blocker.BlockedBy == nil || blocker.RelatesTo == nil || blocker.ConflictsWith == nil {
		t.Fatalf("expected empty link arrays, got %#v", blocker)
	}
}

func TestListSearchMatchesTextOnlyAndComposesWithFilters(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "search-app", "--prefix", "SRC"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Alpha gateway", "--status", "todo", "--priority", "high", "--label", "bug", "--body", "plain body"},
		{"new", "--json", "--title", "Body carrier", "--status", "in_progress", "--priority", "low", "--label", "feature", "--body", "contains alpha marker"},
		{"new", "--json", "--title", "Metadata only", "--status", "todo", "--priority", "urgent", "--label", "docs", "--body", "plain body"},
		{"new", "--json", "--title", "Finished alpha", "--status", "done", "--priority", "urgent", "--label", "feature", "--body", "plain body"},
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
		{name: "title and body matches hide done by default", args: []string{"list", "--json", "--search", "alpha"}, want: []string{"SRC-1", "SRC-2"}},
		{name: "prefix terms", args: []string{"list", "--json", "--search", "gate"}, want: []string{"SRC-1"}},
		{name: "metadata is not searched", args: []string{"list", "--json", "--search", "docs"}, want: []string{}},
		{name: "status filter", args: []string{"list", "--json", "--search", "alpha", "--status", "in_progress"}, want: []string{"SRC-2"}},
		{name: "label filter", args: []string{"list", "--json", "--search", "alpha", "--label", "feature"}, want: []string{"SRC-2"}},
		{name: "priority filter", args: []string{"list", "--json", "--search", "alpha", "--priority", "high"}, want: []string{"SRC-1"}},
		{name: "explicit done status", args: []string{"list", "--json", "--search", "alpha", "--status", "done"}, want: []string{"SRC-4"}},
		{name: "empty results", args: []string{"list", "--json", "--search", "missingterm"}, want: []string{}},
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
			var raw []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
				t.Fatal(err)
			}
			if len(raw) > 0 {
				if _, ok := raw[0]["body"]; ok {
					t.Fatalf("list search JSON must omit body, got %s", result.stdout)
				}
			}
		})
	}
}

func TestListSearchScopesProjects(t *testing.T) {
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
	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-search", "--prefix", "FSE"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-search", "--prefix", "SSE"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "Shared needle"); result.exitCode != 0 {
		t.Fatalf("first new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "new", "--json", "--title", "Shared needle"); result.exitCode != 0 {
		t.Fatalf("second new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	explicit := runITO(t, t.TempDir(), itoHome, "list", "--json", "--project", "second-search", "--search", "needle")
	if explicit.exitCode != 0 {
		t.Fatalf("ito list explicit project search failed with exit %d\nstdout: %s\nstderr: %s", explicit.exitCode, explicit.stdout, explicit.stderr)
	}
	if got := issueIDs(decodeIssueList(t, explicit.stdout)); !stringSlicesEqual(got, []string{"SSE-1"}) {
		t.Fatalf("expected explicit second project result, got %v", got)
	}

	all := runITO(t, t.TempDir(), itoHome, "list", "--json", "--all-projects", "--search", "needle")
	if all.exitCode != 0 {
		t.Fatalf("ito list all projects search failed with exit %d\nstdout: %s\nstderr: %s", all.exitCode, all.stdout, all.stderr)
	}
	if got := issueIDs(decodeIssueList(t, all.stdout)); !stringSlicesEqual(got, []string{"FSE-1", "SSE-1"}) {
		t.Fatalf("expected all project search results ordered by project, got %v", got)
	}
}

func TestListSearchRanksByTextualRelevanceBeforeListOrdering(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "rank-search", "--prefix", "RNK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Sparse", "--body", "rankword"); result.exitCode != 0 {
		t.Fatalf("sparse new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Dense", "--body", "rankword rankword rankword rankword rankword"); result.exitCode != 0 {
		t.Fatalf("dense new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET updated = CASE id WHEN 'RNK-1' THEN '2026-05-24T12:00:00Z' ELSE '2026-05-24T10:00:00Z' END WHERE id IN ('RNK-1', 'RNK-2')`); err != nil {
		t.Fatal(err)
	}

	result := runITO(t, repo, itoHome, "list", "--json", "--search", "rankword")
	if result.exitCode != 0 {
		t.Fatalf("ito list search failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, result.stdout)); !stringSlicesEqual(got, []string{"RNK-2", "RNK-1"}) {
		t.Fatalf("expected textual rank before updated tie-breaker, got %v\nstdout: %s", got, result.stdout)
	}
}

func TestListSearchStaysSynchronizedAfterEditDeleteAndPrune(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "sync-search", "--prefix", "SYN"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Original oldtoken", "--body", "body"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "oldtoken").stdout)); !stringSlicesEqual(got, []string{"SYN-1"}) {
		t.Fatalf("expected old token before edit, got %v", got)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "SYN-1", "--title", "Replacement", "--body", "newtoken body"); result.exitCode != 0 {
		t.Fatalf("ito edit failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "oldtoken").stdout)); len(got) != 0 {
		t.Fatalf("old token must leave FTS after edit, got %v", got)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "newtoken").stdout)); !stringSlicesEqual(got, []string{"SYN-1"}) {
		t.Fatalf("new token must enter FTS after edit, got %v", got)
	}
	if result := runITO(t, repo, itoHome, "rm", "--json", "SYN-1"); result.exitCode != 0 {
		t.Fatalf("ito rm failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "newtoken").stdout)); len(got) != 0 {
		t.Fatalf("deleted token must leave FTS, got %v", got)
	}

	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Pruned prunefts", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("ito new done failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "prunefts", "--status", "done").stdout)); !stringSlicesEqual(got, []string{"SYN-2"}) {
		t.Fatalf("expected done token before prune, got %v", got)
	}
	if result := runITO(t, repo, itoHome, "prune", "--json", "--status", "done", "--yes"); result.exitCode != 0 {
		t.Fatalf("ito prune failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if got := issueIDs(decodeIssueList(t, runITO(t, repo, itoHome, "list", "--json", "--search", "prunefts", "--status", "done").stdout)); len(got) != 0 {
		t.Fatalf("pruned token must leave FTS, got %v", got)
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
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Conflicting"); result.exitCode != 0 {
		t.Fatalf("fourth ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'SHP-1', 'SHP-2', 'blocked_by');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'SHP-3', 'SHP-1', 'relates_to');
INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, 'SHP-4', 'SHP-1', 'conflicts_with');
`, project.ID, project.ID, project.ID); err != nil {
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
	expectedKeys := []string{"id", "project", "title", "status", "priority", "labels", "blocked_by", "relates_to", "conflicts_with", "batch", "body", "created", "updated"}
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
	if !stringSlicesEqual(issue.BlockedBy, []string{"SHP-2"}) || !stringSlicesEqual(issue.RelatesTo, []string{"SHP-3"}) || !stringSlicesEqual(issue.ConflictsWith, []string{"SHP-4"}) {
		t.Fatalf("expected links from fixtures, got blocked_by=%#v relates_to=%#v conflicts_with=%#v", issue.BlockedBy, issue.RelatesTo, issue.ConflictsWith)
	}
	if issue.Batch != nil {
		t.Fatalf("expected issue outside any Batch to report batch=null, got %#v", issue.Batch)
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
	if emptyIssue.Labels == nil || emptyIssue.BlockedBy == nil || emptyIssue.RelatesTo == nil || emptyIssue.ConflictsWith == nil || emptyIssue.Body != "" {
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
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Conflicting work"); result.exitCode != 0 {
		t.Fatalf("second ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "HSH-1", "--conflict", "HSH-2"); result.exitCode != 0 {
		t.Fatalf("ito edit conflict failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
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
		"conflicts_with: HSH-2",
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
	for _, title := range []string{"Source", "Blocker", "Related", "Conflicting"} {
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
	for _, title := range []string{"Source", "Blocker", "Related", "Conflicting"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	db := openTestDB(t, itoHome)
	defer db.Close()
	if _, err := db.Exec(`UPDATE issues SET created = '2026-05-24T10:00:00Z', updated = '2026-05-24T10:00:00Z' WHERE project_id = ? AND id = 'LTS-1'`, project.ID); err != nil {
		t.Fatal(err)
	}

	changed := runITO(t, t.TempDir(), itoHome, "edit", "--json", "LTS-1", "--block", "LTS-2", "--conflict", "LTS-4")
	if changed.exitCode != 0 {
		t.Fatalf("ito edit link change failed with exit %d\nstdout: %s\nstderr: %s", changed.exitCode, changed.stdout, changed.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(changed.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, changed.stdout)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"LTS-2"}) || !stringSlicesEqual(issue.ConflictsWith, []string{"LTS-4"}) || issue.Updated == "2026-05-24T10:00:00Z" || issue.Created != "2026-05-24T10:00:00Z" {
		t.Fatalf("expected changed link and updated timestamp, got %#v", issue)
	}
	conflicting := runITO(t, t.TempDir(), itoHome, "show", "--json", "LTS-4")
	if conflicting.exitCode != 0 {
		t.Fatalf("ito show conflicting issue failed with exit %d\nstdout: %s\nstderr: %s", conflicting.exitCode, conflicting.stdout, conflicting.stderr)
	}
	var conflictingIssue issueJSON
	if err := json.Unmarshal([]byte(conflicting.stdout), &conflictingIssue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, conflicting.stdout)
	}
	if !stringSlicesEqual(conflictingIssue.ConflictsWith, []string{"LTS-1"}) {
		t.Fatalf("expected symmetric conflicts_with on target, got %#v", conflictingIssue.ConflictsWith)
	}
	updatedAfterChange := issue.Updated

	noChange := runITO(t, t.TempDir(), itoHome, "edit", "--json", "LTS-1", "--block", "LTS-2", "--relate", "LTS-3", "--unrelate", "LTS-3", "--conflict", "LTS-4", "--unconflict", "LTS-4", "--conflict", "LTS-4")
	if noChange.exitCode != 0 {
		t.Fatalf("idempotent link edit failed with exit %d\nstdout: %s\nstderr: %s", noChange.exitCode, noChange.stdout, noChange.stderr)
	}
	if err := json.Unmarshal([]byte(noChange.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, noChange.stdout)
	}
	if !stringSlicesEqual(issue.BlockedBy, []string{"LTS-2"}) || len(issue.RelatesTo) != 0 || !stringSlicesEqual(issue.ConflictsWith, []string{"LTS-4"}) || issue.Updated != updatedAfterChange {
		t.Fatalf("idempotent link operations must preserve final state and updated, got %#v", issue)
	}

	removed := runITO(t, t.TempDir(), itoHome, "edit", "--json", "LTS-4", "--unconflict", "LTS-1")
	if removed.exitCode != 0 {
		t.Fatalf("ito edit conflict removal failed with exit %d\nstdout: %s\nstderr: %s", removed.exitCode, removed.stdout, removed.stderr)
	}
	if err := json.Unmarshal([]byte(removed.stdout), &conflictingIssue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, removed.stdout)
	}
	if len(conflictingIssue.ConflictsWith) != 0 {
		t.Fatalf("expected --unconflict to remove conflict from either side, got %#v", conflictingIssue.ConflictsWith)
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
		{name: "malformed conflict target", args: []string{"edit", "--json", "FLK-1", "--conflict", "FLK"}, code: 2},
		{name: "self block", args: []string{"edit", "--json", "FLK-1", "--block", "FLK-1"}, code: 2},
		{name: "self relate", args: []string{"edit", "--json", "FLK-1", "--relate", "FLK-1"}, code: 2},
		{name: "self conflict", args: []string{"edit", "--json", "FLK-1", "--conflict", "FLK-1"}, code: 2},
		{name: "unknown target issue", args: []string{"edit", "--json", "FLK-1", "--block", "FLK-99"}, code: 3},
		{name: "unknown target prefix", args: []string{"edit", "--json", "FLK-1", "--relate", "ZZZ-1"}, code: 3},
		{name: "unknown conflict target", args: []string{"edit", "--json", "FLK-1", "--conflict", "FLK-99"}, code: 3},
		{name: "cross project target", args: []string{"edit", "--json", "FLK-1", "--block", "SLK-1"}, code: 2},
		{name: "cross project conflict target", args: []string{"edit", "--json", "FLK-1", "--conflict", "SLK-1"}, code: 2},
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

func TestEditMissingLinkTargetReportsTheTargetID(t *testing.T) {
	dir := t.TempDir()
	itoHome := t.TempDir()

	if result := runITO(t, dir, itoHome, "init", "--json", "--name", "target-links", "--prefix", "TGT"); result.exitCode != 0 {
		t.Fatalf("init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, dir, itoHome, "new", "--json", "--title", "Source"); result.exitCode != 0 {
		t.Fatalf("new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, dir, itoHome, "edit", "TGT-1", "--block", "TGT-999")
	if result.exitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, `Issue "TGT-999" not found.`) {
		t.Fatalf("expected the error to name the missing link target, got %q", result.stderr)
	}
	if strings.Contains(result.stderr, `"TGT-1"`) {
		t.Fatalf("expected the error not to blame the existing source Issue, got %q", result.stderr)
	}
}

func TestNewWithRepeatedLabelDeduplicates(t *testing.T) {
	dir := t.TempDir()
	itoHome := t.TempDir()

	if result := runITO(t, dir, itoHome, "init", "--json", "--name", "dup-label-app", "--prefix", "DUP"); result.exitCode != 0 {
		t.Fatalf("init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, dir, itoHome, "new", "--json", "--title", "Repeated label", "--label", "bug", "--label", "bug")
	if result.exitCode != 0 {
		t.Fatalf("expected the redundant label to be tolerated, got exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	var created issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &created); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if len(created.Labels) != 1 || created.Labels[0] != "bug" {
		t.Fatalf("expected deduplicated labels [bug], got %#v", created.Labels)
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

func TestRmDeletesIssueLabelsOutgoingIncomingLinksAndFTS(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "delete-app", "--prefix", "DEL")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Delete me", "--label", "bug", "--body", "deleteonlytoken"},
		{"new", "--json", "--title", "Outgoing target"},
		{"new", "--json", "--title", "Related survivor"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "DEL-1", "--block", "DEL-2", "--relate", "DEL-3"); result.exitCode != 0 {
		t.Fatalf("ito edit outgoing links failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, t.TempDir(), itoHome, "edit", "--json", "DEL-2", "--block", "DEL-1"); result.exitCode != 0 {
		t.Fatalf("ito edit incoming link failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "rm", "DEL-1")
	if result.exitCode != 0 || result.stdout != "DEL-1 deleted.\n" || result.stderr != "" {
		t.Fatalf("expected short human deletion confirmation, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
	showDeleted := runITO(t, t.TempDir(), itoHome, "show", "--json", "DEL-1")
	if showDeleted.exitCode != 3 {
		t.Fatalf("deleted Issue must be unknown, got exit %d\nstdout: %s\nstderr: %s", showDeleted.exitCode, showDeleted.stdout, showDeleted.stderr)
	}
	for _, id := range []string{"DEL-2", "DEL-3"} {
		show := runITO(t, t.TempDir(), itoHome, "show", "--json", id)
		if show.exitCode != 0 {
			t.Fatalf("survivor %s was deleted or unreadable: exit %d\nstdout: %s\nstderr: %s", id, show.exitCode, show.stdout, show.stderr)
		}
		var issue issueJSON
		if err := json.Unmarshal([]byte(show.stdout), &issue); err != nil {
			t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, show.stdout)
		}
		if len(issue.BlockedBy) != 0 || len(issue.RelatesTo) != 0 {
			t.Fatalf("links to deleted Issue must be removed from %s, got %#v", id, issue)
		}
	}

	db := openTestDB(t, itoHome)
	defer db.Close()
	var issuesCount, labelsCount, linksCount, ftsMatches int
	if err := db.QueryRow(`SELECT count(*) FROM issues WHERE project_id = ?`, project.ID).Scan(&issuesCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issue_labels WHERE project_id = ? AND issue_id = 'DEL-1'`, project.ID).Scan(&labelsCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issue_links WHERE project_id = ? AND (source_id = 'DEL-1' OR target_id = 'DEL-1')`, project.ID).Scan(&linksCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issues_fts WHERE issues_fts MATCH 'deleteonlytoken'`).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if issuesCount != 2 || labelsCount != 0 || linksCount != 0 || ftsMatches != 0 {
		t.Fatalf("delete cleanup mismatch: issues=%d labels=%d links=%d fts=%d", issuesCount, labelsCount, linksCount, ftsMatches)
	}
}

func TestRmPreservesProjectCounter(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "counter-delete", "--prefix", "CTR"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, title := range []string{"First", "Second"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, t.TempDir(), itoHome, "rm", "--json", "CTR-2"); result.exitCode != 0 {
		t.Fatalf("ito rm failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	result := runITO(t, repo, itoHome, "new", "--json", "--title", "Third")
	if result.exitCode != 0 {
		t.Fatalf("ito new after delete failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.ID != "CTR-3" {
		t.Fatalf("deleted IDs must not be reused, got %#v", issue)
	}
}

func TestRmRejectsMalformedAndUnknownIDs(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "rm-unknown", "--prefix", "RUK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "malformed ID", args: []string{"rm", "--json", "RUK"}, code: 2},
		{name: "unknown ID", args: []string{"rm", "--json", "RUK-99"}, code: 3},
		{name: "unknown prefix", args: []string{"rm", "--json", "ZZZ-1"}, code: 3},
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

func TestRmJSONOutput(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "json-delete", "--prefix", "JRM"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Delete JSON"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "rm", "--json", "JRM-1")
	if result.exitCode != 0 || result.stdout != "{\"deleted\":1,\"id\":\"JRM-1\"}\n" || result.stderr != "" {
		t.Fatalf("expected exact JSON deletion output, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
}

func TestPruneRequiresFilterConfirmationAndValidStatus(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "prune-guards", "--prefix", "PRG"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "missing filter", args: []string{"prune", "--json", "--yes"}},
		{name: "missing confirmation", args: []string{"prune", "--json", "--status", "done"}},
		{name: "invalid status", args: []string{"prune", "--json", "--status", "closed", "--yes"}},
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

func TestPruneDeletesMatchingStatusOnlyWithHumanCount(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "status-prune", "--prefix", "SPN"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Backlog survivor", "--status", "backlog"},
		{"new", "--json", "--title", "Done one", "--status", "done"},
		{"new", "--json", "--title", "Todo survivor", "--status", "todo"},
		{"new", "--json", "--title", "Done two", "--status", "done"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}

	result := runITO(t, repo, itoHome, "prune", "--status", "done", "--yes")
	if result.exitCode != 0 || result.stdout != "2\n" || result.stderr != "" {
		t.Fatalf("expected human prune count only, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
	for _, id := range []string{"SPN-1", "SPN-3"} {
		show := runITO(t, repo, itoHome, "show", "--json", id)
		if show.exitCode != 0 {
			t.Fatalf("survivor %s was deleted or unreadable: exit %d\nstdout: %s\nstderr: %s", id, show.exitCode, show.stdout, show.stderr)
		}
	}
	for _, id := range []string{"SPN-2", "SPN-4"} {
		show := runITO(t, repo, itoHome, "show", "--json", id)
		if show.exitCode != 3 {
			t.Fatalf("matching Issue %s must be deleted, got exit %d\nstdout: %s\nstderr: %s", id, show.exitCode, show.stdout, show.stderr)
		}
	}
}

func TestPruneDeletesLabelsLinksAndFTSForMatchingIssues(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	createdProject := runITO(t, repo, itoHome, "init", "--json", "--name", "cleanup-prune", "--prefix", "CLP")
	if createdProject.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", createdProject.exitCode, createdProject.stdout, createdProject.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(createdProject.stdout), &project); err != nil {
		t.Fatal(err)
	}
	fixtures := [][]string{
		{"new", "--json", "--title", "Done cleanup one", "--status", "done", "--label", "bug", "--body", "pruneonlytoken"},
		{"new", "--json", "--title", "Todo survivor", "--status", "todo"},
		{"new", "--json", "--title", "Done cleanup two", "--status", "done", "--label", "docs"},
		{"new", "--json", "--title", "Backlog survivor", "--status", "backlog"},
	}
	for _, args := range fixtures {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstdout: %s\nstderr: %s", args, result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "CLP-1", "--block", "CLP-2", "--relate", "CLP-3", "--conflict", "CLP-4"); result.exitCode != 0 {
		t.Fatalf("ito edit outgoing links failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "CLP-2", "--block", "CLP-1"); result.exitCode != 0 {
		t.Fatalf("ito edit incoming link failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "edit", "--json", "CLP-3", "--relate", "CLP-4"); result.exitCode != 0 {
		t.Fatalf("ito edit survivor-facing link failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, repo, itoHome, "prune", "--status", "done", "--yes")
	if result.exitCode != 0 || result.stdout != "2\n" || result.stderr != "" {
		t.Fatalf("expected prune cleanup success, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
	for _, id := range []string{"CLP-2", "CLP-4"} {
		show := runITO(t, repo, itoHome, "show", "--json", id)
		if show.exitCode != 0 {
			t.Fatalf("survivor %s was deleted or unreadable: exit %d\nstdout: %s\nstderr: %s", id, show.exitCode, show.stdout, show.stderr)
		}
		var issue issueJSON
		if err := json.Unmarshal([]byte(show.stdout), &issue); err != nil {
			t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, show.stdout)
		}
		if len(issue.BlockedBy) != 0 || len(issue.RelatesTo) != 0 || len(issue.ConflictsWith) != 0 {
			t.Fatalf("links to pruned Issues must be removed from %s, got %#v", id, issue)
		}
	}

	db := openTestDB(t, itoHome)
	defer db.Close()
	var issuesCount, labelsCount, linksCount, ftsMatches int
	if err := db.QueryRow(`SELECT count(*) FROM issues WHERE project_id = ?`, project.ID).Scan(&issuesCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issue_labels WHERE project_id = ? AND issue_id IN ('CLP-1', 'CLP-3')`, project.ID).Scan(&labelsCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issue_links WHERE project_id = ? AND (source_id IN ('CLP-1', 'CLP-3') OR target_id IN ('CLP-1', 'CLP-3'))`, project.ID).Scan(&linksCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM issues_fts WHERE issues_fts MATCH 'pruneonlytoken'`).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if issuesCount != 2 || labelsCount != 0 || linksCount != 0 || ftsMatches != 0 {
		t.Fatalf("prune cleanup mismatch: issues=%d labels=%d links=%d fts=%d", issuesCount, labelsCount, linksCount, ftsMatches)
	}
}

func TestPrunePreservesProjectCounter(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "counter-prune", "--prefix", "CPR"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	for _, title := range []string{"First", "Second"} {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", title, "--status", "done"); result.exitCode != 0 {
			t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
		}
	}
	if result := runITO(t, repo, itoHome, "prune", "--json", "--status", "done", "--yes"); result.exitCode != 0 {
		t.Fatalf("ito prune failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	result := runITO(t, repo, itoHome, "new", "--json", "--title", "Third")
	if result.exitCode != 0 {
		t.Fatalf("ito new after prune failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.ID != "CPR-3" {
		t.Fatalf("pruned IDs must not be reused, got %#v", issue)
	}
}

func TestPruneProjectScoping(t *testing.T) {
	firstRepo := t.TempDir()
	secondRepo := t.TempDir()
	run(t, firstRepo, "git", "init", "-q")
	run(t, secondRepo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, firstRepo, itoHome, "init", "--json", "--name", "first-prune", "--prefix", "FPR"); result.exitCode != 0 {
		t.Fatalf("first init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "init", "--json", "--name", "second-prune", "--prefix", "SPR"); result.exitCode != 0 {
		t.Fatalf("second init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, firstRepo, itoHome, "new", "--json", "--title", "First done", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("first new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, secondRepo, itoHome, "new", "--json", "--title", "Second done", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("second new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, t.TempDir(), itoHome, "prune", "--project", "second-prune", "--status", "done", "--yes")
	if result.exitCode != 0 || result.stdout != "1\n" || result.stderr != "" {
		t.Fatalf("expected explicit project prune success, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
	if show := runITO(t, firstRepo, itoHome, "show", "--json", "FPR-1"); show.exitCode != 0 {
		t.Fatalf("first Project Issue must survive explicit second prune, got exit %d\nstdout: %s\nstderr: %s", show.exitCode, show.stdout, show.stderr)
	}
	if show := runITO(t, secondRepo, itoHome, "show", "--json", "SPR-1"); show.exitCode != 3 {
		t.Fatalf("second Project Issue must be pruned, got exit %d\nstdout: %s\nstderr: %s", show.exitCode, show.stdout, show.stderr)
	}
}

func TestPruneJSONOutput(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "json-prune", "--prefix", "JPR"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Delete JSON", "--status", "done"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	result := runITO(t, repo, itoHome, "prune", "--json", "--status", "done", "--yes")
	if result.exitCode != 0 || result.stdout != "{\"deleted\":1}\n" || result.stderr != "" {
		t.Fatalf("expected exact JSON prune output, got exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
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

func TestInitOutsideGitWithExplicitFlagsCreatesNestedProject(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalChild, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	itoHome := t.TempDir()

	if result := runITO(t, root, itoHome, "init", "--json", "--name", "covering-project"); result.exitCode != 0 {
		t.Fatalf("root init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}

	// Explicit --name/--prefix asks for a new Project at the cwd; the covering
	// ancestor must not swallow the flags into a silent no-op.
	result := runITO(t, child, itoHome, "init", "--json", "--name", "child-project", "--prefix", "CHL")
	if result.exitCode != 0 {
		t.Fatalf("child init failed with exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if project.Name != "child-project" || project.Prefix != "CHL" {
		t.Fatalf("expected the explicit child project, got %#v", project)
	}
	if project.RootPath != canonicalChild {
		t.Fatalf("expected child root_path %q, got %q", canonicalChild, project.RootPath)
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

func TestListAllProjectsSearchGroupsByProjectDespiteRelevance(t *testing.T) {
	parent := t.TempDir()
	aaaRepo := filepath.Join(parent, "aaa")
	zzzRepo := filepath.Join(parent, "zzz")
	for _, repo := range []string{aaaRepo, zzzRepo} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, repo, "git", "init", "-q")
	}
	itoHome := t.TempDir()
	if result := runITO(t, aaaRepo, itoHome, "init", "--json", "--name", "aaa-proj", "--prefix", "AAA"); result.exitCode != 0 {
		t.Fatalf("aaa init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	if result := runITO(t, zzzRepo, itoHome, "init", "--json", "--name", "zzz-proj", "--prefix", "ZZZ"); result.exitCode != 0 {
		t.Fatalf("zzz init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	// Distinct relevance across Projects: if bm25 were the primary key under
	// --all-projects, the global order would be AAA-1 (dense), ZZZ-1 (medium), AAA-2
	// (sparse), interleaving the Projects. The contract (§6/§6.2) requires Project
	// as the primary key, with textual ranking as the intra-Project tie-breaker.
	if result := runITO(t, aaaRepo, itoHome, "new", "--json", "--title", "Dense", "--body", "needle needle needle needle"); result.exitCode != 0 {
		t.Fatalf("AAA-1 new failed: %s", result.stderr)
	}
	if result := runITO(t, aaaRepo, itoHome, "new", "--json", "--title", "Sparse", "--body", "needle"); result.exitCode != 0 {
		t.Fatalf("AAA-2 new failed: %s", result.stderr)
	}
	if result := runITO(t, zzzRepo, itoHome, "new", "--json", "--title", "Medium", "--body", "needle needle"); result.exitCode != 0 {
		t.Fatalf("ZZZ-1 new failed: %s", result.stderr)
	}

	allJSON := runITO(t, t.TempDir(), itoHome, "list", "--json", "--all-projects", "--search", "needle")
	if allJSON.exitCode != 0 {
		t.Fatalf("ito list --all-projects --search failed with exit %d\nstderr: %s", allJSON.exitCode, allJSON.stderr)
	}
	want := []string{"AAA-1", "AAA-2", "ZZZ-1"}
	if got := issueIDs(decodeIssueList(t, allJSON.stdout)); !stringSlicesEqual(got, want) {
		t.Fatalf("expected project-primary ordering %v, got %v\nstdout: %s", want, got, allJSON.stdout)
	}

	human := runITO(t, t.TempDir(), itoHome, "list", "--all-projects", "--search", "needle")
	if human.exitCode != 0 {
		t.Fatalf("ito list --all-projects --search human failed: %s", human.stderr)
	}
	if n := strings.Count(human.stdout, "aaa-proj:"); n != 1 {
		t.Fatalf("each Project must group under a single header; aaa-proj appeared %d times\nstdout: %s", n, human.stdout)
	}
}

func TestDetachedProjectSerializesNullRootPath(t *testing.T) {
	parent := t.TempDir()
	originalRepo := filepath.Join(parent, "detach-app")
	if err := os.MkdirAll(originalRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, originalRepo, "git", "init", "-q")
	itoHome := t.TempDir()
	if result := runITO(t, originalRepo, itoHome, "init", "--json", "--name", "detach-app", "--prefix", "DET"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}

	movedRepo := filepath.Join(t.TempDir(), "detach-app")
	if err := os.Rename(originalRepo, movedRepo); err != nil {
		t.Fatal(err)
	}
	// Detects the moved repo: must diagnose (exit 4) and clear the stale pointer.
	if result := runITO(t, movedRepo, itoHome, "init", "--json"); result.exitCode != 4 {
		t.Fatalf("expected moved-repo diagnostic exit 4, got %d\nstderr: %s", result.exitCode, result.stderr)
	}

	db := openTestDB(t, itoHome)
	defer db.Close()
	var rootPath sql.NullString
	if err := db.QueryRow(`SELECT root_path FROM projects WHERE name = 'detach-app'`).Scan(&rootPath); err != nil {
		t.Fatal(err)
	}
	if rootPath.Valid {
		t.Fatalf("detached Project must persist root_path NULL, got %q", rootPath.String)
	}

	// The Project object serializes root_path as null (not "").
	renamed := runITO(t, t.TempDir(), itoHome, "rename", "--json", "--project", "detach-app", "detached-app")
	if renamed.exitCode != 0 {
		t.Fatalf("ito rename failed with exit %d\nstderr: %s", renamed.exitCode, renamed.stderr)
	}
	var project struct {
		RootPath *string `json:"root_path"`
	}
	if err := json.Unmarshal([]byte(renamed.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project object: %v\nstdout: %s", err, renamed.stdout)
	}
	if project.RootPath != nil {
		t.Fatalf("detached Project must serialize root_path null, got %q", *project.RootPath)
	}
	if !strings.Contains(renamed.stdout, `"root_path":null`) {
		t.Fatalf("expected literal null root_path in JSON, got %s", renamed.stdout)
	}
}

func TestInitNormalizesUnicodeNameAndPrefix(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "café-app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	result := runITO(t, repo, itoHome, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	var project projectJSON
	if err := json.Unmarshal([]byte(result.stdout), &project); err != nil {
		t.Fatalf("stdout is not a project object: %v\nstdout: %s", err, result.stdout)
	}
	if project.Name != "cafe-app" {
		t.Fatalf("expected transliterated name %q, got %q", "cafe-app", project.Name)
	}
	if project.Prefix != "CAFEAP" {
		t.Fatalf("expected transliterated prefix %q, got %q", "CAFEAP", project.Prefix)
	}
}

func TestLinkArraysSortNumericallyForMultiDigitIDs(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()
	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "link-order", "--prefix", "LNK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	for i := 0; i < 12; i++ {
		if result := runITO(t, repo, itoHome, "new", "--json", "--title", fmt.Sprintf("Issue %d", i+1)); result.exitCode != 0 {
			t.Fatalf("new %d failed: %s", i+1, result.stderr)
		}
	}
	// Applies the links out of order; multi-digit IDs require numeric ordering.
	for _, target := range []string{"LNK-10", "LNK-2", "LNK-11", "LNK-3"} {
		if result := runITO(t, repo, itoHome, "edit", "--json", "LNK-1", "--block", target); result.exitCode != 0 {
			t.Fatalf("block %s failed: %s", target, result.stderr)
		}
	}
	for _, target := range []string{"LNK-12", "LNK-4", "LNK-9"} {
		if result := runITO(t, repo, itoHome, "edit", "--json", "LNK-1", "--relate", target); result.exitCode != 0 {
			t.Fatalf("relate %s failed: %s", target, result.stderr)
		}
	}
	for _, target := range []string{"LNK-11", "LNK-3", "LNK-10"} {
		if result := runITO(t, repo, itoHome, "edit", "--json", "LNK-1", "--conflict", target); result.exitCode != 0 {
			t.Fatalf("conflict %s failed: %s", target, result.stderr)
		}
	}

	show := runITO(t, repo, itoHome, "show", "--json", "LNK-1")
	if show.exitCode != 0 {
		t.Fatalf("ito show failed: %s", show.stderr)
	}
	var shown issueJSON
	if err := json.Unmarshal([]byte(show.stdout), &shown); err != nil {
		t.Fatalf("stdout is not an issue object: %v\nstdout: %s", err, show.stdout)
	}
	if want := []string{"LNK-2", "LNK-3", "LNK-10", "LNK-11"}; !stringSlicesEqual(shown.BlockedBy, want) {
		t.Fatalf("blocked_by must sort numerically; want %v got %v", want, shown.BlockedBy)
	}
	if want := []string{"LNK-4", "LNK-9", "LNK-12"}; !stringSlicesEqual(shown.RelatesTo, want) {
		t.Fatalf("relates_to must sort numerically; want %v got %v", want, shown.RelatesTo)
	}
	if want := []string{"LNK-3", "LNK-10", "LNK-11"}; !stringSlicesEqual(shown.ConflictsWith, want) {
		t.Fatalf("conflicts_with must sort numerically; want %v got %v", want, shown.ConflictsWith)
	}

	list := runITO(t, repo, itoHome, "list", "--json")
	if list.exitCode != 0 {
		t.Fatalf("ito list failed: %s", list.stderr)
	}
	for _, item := range decodeIssueList(t, list.stdout) {
		if item.ID != "LNK-1" {
			continue
		}
		if want := []string{"LNK-2", "LNK-3", "LNK-10", "LNK-11"}; !stringSlicesEqual(item.BlockedBy, want) {
			t.Fatalf("list blocked_by must sort numerically; want %v got %v", want, item.BlockedBy)
		}
		if want := []string{"LNK-3", "LNK-10", "LNK-11"}; !stringSlicesEqual(item.ConflictsWith, want) {
			t.Fatalf("list conflicts_with must sort numerically; want %v got %v", want, item.ConflictsWith)
		}
	}
}

func TestWritingCommandsLeaveRepoUntouched(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()
	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "footprint", "--prefix", "FOO"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	before := dirEntries(t, repo)

	steps := [][]string{
		{"new", "--json", "--title", "First", "--label", "bug"},
		{"new", "--json", "--title", "Second", "--status", "todo"},
		{"edit", "--json", "FOO-1", "--title", "First edited", "--add-label", "infra", "--block", "FOO-2"},
		{"move", "--json", "FOO-2", "in_progress"},
		{"rm", "--json", "FOO-2"},
		{"new", "--json", "--title", "Throwaway", "--status", "done"},
		{"prune", "--json", "--status", "done", "--yes"},
	}
	for _, args := range steps {
		if result := runITO(t, repo, itoHome, args...); result.exitCode != 0 {
			t.Fatalf("ito %v failed with exit %d\nstderr: %s", args, result.exitCode, result.stderr)
		}
	}

	if _, err := os.Stat(filepath.Join(repo, ".ito")); !os.IsNotExist(err) {
		t.Fatalf("mutations must not create .ito inside repo, stat err=%v", err)
	}
	if after := dirEntries(t, repo); !stringSlicesEqual(before, after) {
		t.Fatalf("writing commands must not touch repo entries; before=%v after=%v", before, after)
	}
}

func TestInitOutsideGitLeavesDirUntouched(t *testing.T) {
	dir := t.TempDir()
	itoHome := t.TempDir()
	before := dirEntries(t, dir)
	if result := runITO(t, dir, itoHome, "init", "--json", "--name", "no-git", "--prefix", "NOG"); result.exitCode != 0 {
		t.Fatalf("ito init outside git failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	if after := dirEntries(t, dir); !stringSlicesEqual(before, after) {
		t.Fatalf("init outside git must not write to the dir; before=%v after=%v", before, after)
	}
}

func TestNewAndListResolveSharedProjectFromWorktree(t *testing.T) {
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
	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "shared-wt", "--prefix", "WKT"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	// An Issue created from the linked worktree lands in the shared git root's Project.
	created := runITO(t, worktree, itoHome, "new", "--json", "--title", "From worktree")
	if created.exitCode != 0 {
		t.Fatalf("ito new from worktree failed with exit %d\nstderr: %s", created.exitCode, created.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(created.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue object: %v\nstdout: %s", err, created.stdout)
	}
	if issue.ID != "WKT-1" || issue.Project != "shared-wt" {
		t.Fatalf("worktree issue must belong to the shared Project, got %#v", issue)
	}
	// And it is visible from both the worktree and the main root.
	for _, dir := range []string{worktree, repo} {
		listed := runITO(t, dir, itoHome, "list", "--json")
		if listed.exitCode != 0 {
			t.Fatalf("ito list from %s failed: %s", dir, listed.stderr)
		}
		if got := issueIDs(decodeIssueList(t, listed.stdout)); !stringSlicesEqual(got, []string{"WKT-1"}) {
			t.Fatalf("worktree-shared issue must list from %s, got %v", dir, got)
		}
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

func intPtr(value int) *int {
	return &value
}

func decodeIssueList(t *testing.T, stdout string) []issueJSON {
	t.Helper()
	var issues []issueJSON
	if err := json.Unmarshal([]byte(stdout), &issues); err != nil {
		t.Fatalf("stdout is not a JSON issue array: %v\nstdout: %s", err, stdout)
	}
	return issues
}

func decodeBatchList(t *testing.T, stdout string) []batchJSON {
	t.Helper()
	var batches []batchJSON
	if err := json.Unmarshal([]byte(stdout), &batches); err != nil {
		t.Fatalf("stdout is not a JSON Batch array: %v\nstdout: %s", err, stdout)
	}
	return batches
}

func decodeBatchShow(t *testing.T, stdout string) batchShowJSON {
	t.Helper()
	var batch batchShowJSON
	if err := json.Unmarshal([]byte(stdout), &batch); err != nil {
		t.Fatalf("stdout is not a JSON Batch object: %v\nstdout: %s", err, stdout)
	}
	return batch
}

func issueIDs(issues []issueJSON) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func humanIssueIDs(stdout string) []string {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && itostore.IssueIDPattern.MatchString(fields[0]) {
			ids = append(ids, fields[0])
		}
	}
	return ids
}

func batchNames(batches []batchJSON) []string {
	names := make([]string, 0, len(batches))
	for _, batch := range batches {
		names = append(names, batch.Name)
	}
	return names
}

func findBatchJSON(batches []batchJSON, name string) (batchJSON, bool) {
	for _, batch := range batches {
		if batch.Name == name {
			return batch, true
		}
	}
	return batchJSON{}, false
}

func issueBatchName(issue issueJSON) string {
	if issue.Batch == nil {
		return ""
	}
	return *issue.Batch
}

func findIssueJSON(issues []issueJSON, id string) (issueJSON, bool) {
	for _, issue := range issues {
		if issue.ID == id {
			return issue, true
		}
	}
	return issueJSON{}, false
}

func openTestDB(t *testing.T, itoHome string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(itoHome, "ito.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestFlagsAfterPositionalAreAccepted hardens the agent-native contract: commands
// with a positional (show/move/rm/rename) must accept flags in any position,
// not only before the positional. The stdlib flag.Parse stops at the first non-flag,
// so without the split the natural invocation `<command> <ID> --json` failed with
// exit 2 ("takes exactly one ID") even with the ID present.
func TestFlagsAfterPositionalAreAccepted(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "flag-order", "--prefix", "FLO"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	if result := runITO(t, repo, itoHome, "new", "--json", "--title", "Reorderable"); result.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}

	// show <ID> --json
	if result := runITO(t, repo, itoHome, "show", "FLO-1", "--json"); result.exitCode != 0 {
		t.Fatalf("show <ID> --json must succeed, got exit %d\nstderr: %s", result.exitCode, result.stderr)
	} else {
		var issue issueJSON
		if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil || issue.ID != "FLO-1" {
			t.Fatalf("show <ID> --json produced unexpected output: err=%v stdout=%s", err, result.stdout)
		}
	}

	// move <ID> <status> --json
	if result := runITO(t, repo, itoHome, "move", "FLO-1", "in_progress", "--json"); result.exitCode != 0 {
		t.Fatalf("move <ID> <status> --json must succeed, got exit %d\nstderr: %s", result.exitCode, result.stderr)
	} else {
		var issue issueJSON
		if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil || issue.Status != "in_progress" {
			t.Fatalf("move <ID> <status> --json produced unexpected output: err=%v stdout=%s", err, result.stdout)
		}
	}

	// rename <name> --json
	if result := runITO(t, repo, itoHome, "rename", "flag-order-2", "--json"); result.exitCode != 0 {
		t.Fatalf("rename <name> --json must succeed, got exit %d\nstderr: %s", result.exitCode, result.stderr)
	} else {
		var project projectJSON
		if err := json.Unmarshal([]byte(result.stdout), &project); err != nil || project.Name != "flag-order-2" {
			t.Fatalf("rename <name> --json produced unexpected output: err=%v stdout=%s", err, result.stdout)
		}
	}

	// rm <ID> --json
	if result := runITO(t, repo, itoHome, "rm", "FLO-1", "--json"); result.exitCode != 0 {
		t.Fatalf("rm <ID> --json must succeed, got exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
}

// decodeErrorEnvelope parses a JSON {error,code,hint} failure envelope.
func decodeErrorEnvelope(t *testing.T, stderr string) cliError {
	t.Helper()
	var envelope cliError
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON error envelope: %v\nstderr: %s", err, stderr)
	}
	return envelope
}

func TestJSONValueFormSelectsErrorEnvelope(t *testing.T) {
	repo := t.TempDir()
	itoHome := t.TempDir()

	// --json=true on a flag-parse error must emit a JSON envelope on stderr.
	result := runITO(t, repo, itoHome, "new", "--json=true", "--bogus")
	if result.exitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", result.exitCode, result.stderr)
	}
	if result.stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", result.stdout)
	}
	envelope := decodeErrorEnvelope(t, result.stderr)
	if envelope.Code != 2 || envelope.Error == "" || envelope.Hint == "" {
		t.Fatalf("expected actionable envelope, got %#v", envelope)
	}
}

func TestJSONValueFormFalseStaysHuman(t *testing.T) {
	repo := t.TempDir()
	itoHome := t.TempDir()

	// --json=false (and a bad value) must keep the human, non-JSON failure path.
	for _, value := range []string{"--json=false", "--json=nope"} {
		result := runITO(t, repo, itoHome, "new", value, "--bogus")
		if result.exitCode != 2 {
			t.Fatalf("%s: expected exit 2, got %d\nstderr: %s", value, result.exitCode, result.stderr)
		}
		if strings.HasPrefix(strings.TrimSpace(result.stderr), "{") {
			t.Fatalf("%s: expected human stderr, got JSON envelope: %s", value, result.stderr)
		}
		if result.stderr == "" {
			t.Fatalf("%s: expected a human error message on stderr", value)
		}
	}
}

func TestHelpDetectionIgnoresFlagValues(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "hijack-app", "--prefix", "HJK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}

	// `new --title -h --json` must create an issue titled "-h", not print help.
	result := runITO(t, repo, itoHome, "new", "--title", "-h", "--json")
	if result.exitCode != 0 {
		t.Fatalf("expected issue creation, got exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, result.stdout)
	}
	if issue.Title != "-h" {
		t.Fatalf("expected title %q, got %q", "-h", issue.Title)
	}

	// `show --project -h <ID>` must not print help; it fails validating "-h".
	show := runITO(t, repo, itoHome, "show", "--project", "-h", issue.ID)
	if show.exitCode == 0 {
		t.Fatalf("expected failure (project name -h is invalid), got exit 0\nstdout: %s", show.stdout)
	}
	if strings.Contains(show.stdout, "usage: ito show") {
		t.Fatalf("show must not print help when -h is a flag value\nstdout: %s", show.stdout)
	}
}

func TestUnknownCommandHonorsJSON(t *testing.T) {
	repo := t.TempDir()
	itoHome := t.TempDir()

	// Human mode: plain-text message, exit 2.
	human := runITO(t, repo, itoHome, "frobnicate")
	if human.exitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", human.exitCode, human.stderr)
	}
	if !strings.Contains(human.stderr, "unknown command: frobnicate") {
		t.Fatalf("expected human unknown-command message, got %q", human.stderr)
	}
	if !strings.Contains(human.stderr, "Run 'ito --help' to see available commands.") {
		t.Fatalf("expected human unknown-command help hint, got %q", human.stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(human.stderr), "{") {
		t.Fatalf("human mode must not emit a JSON envelope: %s", human.stderr)
	}

	// JSON mode: error envelope, exit 2.
	jsonResult := runITO(t, repo, itoHome, "frobnicate", "--json")
	if jsonResult.exitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", jsonResult.exitCode, jsonResult.stderr)
	}
	envelope := decodeErrorEnvelope(t, jsonResult.stderr)
	if envelope.Code != 2 || !strings.Contains(envelope.Error, "unknown command: frobnicate") || envelope.Hint != "Run 'ito --help' to see available commands." {
		t.Fatalf("expected unknown-command envelope, got %#v", envelope)
	}
}

func TestEndOfFlagsMarker(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	itoHome := t.TempDir()

	if result := runITO(t, repo, itoHome, "init", "--json", "--name", "marker-app", "--prefix", "MRK"); result.exitCode != 0 {
		t.Fatalf("ito init failed with exit %d\nstderr: %s", result.exitCode, result.stderr)
	}
	created := runITO(t, repo, itoHome, "new", "--json", "--title", "Marker target")
	if created.exitCode != 0 {
		t.Fatalf("ito new failed with exit %d\nstderr: %s", created.exitCode, created.stderr)
	}
	var issue issueJSON
	if err := json.Unmarshal([]byte(created.stdout), &issue); err != nil {
		t.Fatalf("stdout is not an issue JSON object: %v\nstdout: %s", err, created.stdout)
	}

	// `show --json -- <ID>`: the marker ends flag parsing; <ID> is positional.
	result := runITO(t, repo, itoHome, "show", "--json", "--", issue.ID)
	if result.exitCode != 0 {
		t.Fatalf("expected show to succeed past --, got exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	var shown issueJSON
	if err := json.Unmarshal([]byte(result.stdout), &shown); err != nil || shown.ID != issue.ID {
		t.Fatalf("show -- <ID> produced unexpected output: err=%v stdout=%s", err, result.stdout)
	}
}

func TestWantsJSONValueAwareness(t *testing.T) {
	newFlags := commandValueFlags("new")
	showFlags := commandValueFlags("show")
	tests := []struct {
		name      string
		args      []string
		valueFlag map[string]struct{}
		want      bool
	}{
		{name: "bare --json", args: []string{"--json"}, valueFlag: newFlags, want: true},
		{name: "bare -json", args: []string{"-json"}, valueFlag: newFlags, want: true},
		{name: "--json=true", args: []string{"--json=true"}, valueFlag: newFlags, want: true},
		{name: "--json=1", args: []string{"--json=1"}, valueFlag: newFlags, want: true},
		{name: "--json=false", args: []string{"--json=false"}, valueFlag: newFlags, want: false},
		{name: "--json=bad", args: []string{"--json=nope"}, valueFlag: newFlags, want: false},
		{name: "no json", args: []string{"--title", "x"}, valueFlag: newFlags, want: false},
		{name: "json as project value", args: []string{"--project", "-json", "TST-1"}, valueFlag: showFlags, want: false},
		{name: "json after value flag", args: []string{"--project", "p", "--json"}, valueFlag: showFlags, want: true},
		{name: "nil value flags", args: []string{"--json"}, valueFlag: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wantsJSON(tt.args, tt.valueFlag); got != tt.want {
				t.Fatalf("wantsJSON(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestWantsHelpValueAwareness(t *testing.T) {
	newFlags := commandValueFlags("new")
	showFlags := commandValueFlags("show")
	tests := []struct {
		name      string
		args      []string
		valueFlag map[string]struct{}
		want      bool
	}{
		{name: "--help", args: []string{"--help"}, valueFlag: newFlags, want: true},
		{name: "-h", args: []string{"-h"}, valueFlag: newFlags, want: true},
		{name: "-help", args: []string{"-help"}, valueFlag: newFlags, want: true},
		{name: "-h as title value", args: []string{"--title", "-h", "--json"}, valueFlag: newFlags, want: false},
		{name: "--help as project value", args: []string{"--project", "--help", "TST-1"}, valueFlag: showFlags, want: false},
		{name: "help after value flag", args: []string{"--title", "x", "--help"}, valueFlag: newFlags, want: true},
		{name: "no help", args: []string{"--title", "x"}, valueFlag: newFlags, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wantsHelp(tt.args, tt.valueFlag); got != tt.want {
				t.Fatalf("wantsHelp(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
