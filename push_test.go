package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	itoconfig "github.com/c3h/ito/internal/config"
	"github.com/c3h/ito/internal/ledger"
	itostore "github.com/c3h/ito/internal/store"
	"github.com/c3h/ito/internal/tui"
)

// connectedHome prepares an ITO_HOME whose config points at the test Ledger
// and whose store holds Project "local" with Issue LCL-1.
func connectedHome(t *testing.T) string {
	t.Helper()
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	if err := itoconfig.Write(itoconfig.Config{Ledger: &itoconfig.Ledger{URL: testLedgerURL, Token: testLedgerToken}}); err != nil {
		t.Fatal(err)
	}
	createLocalIssue(t, itoHome)
	return itoHome
}

// stubLedger wraps a Memory Ledger so a test can make Append fail or hang.
type stubLedger struct {
	*ledger.Memory
	appendErr error
	// block holds Append until closed; released reports that it returned.
	block    chan struct{}
	released chan struct{}
}

func (s *stubLedger) Append(changes []ledger.Change) ([]int64, error) {
	if s.block != nil {
		<-s.block
		defer close(s.released)
	}
	if s.appendErr != nil {
		return nil, s.appendErr
	}
	return s.Memory.Append(changes)
}

func useStubLedger(t *testing.T, stub *stubLedger) {
	t.Helper()
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) { return stub, nil }
	t.Cleanup(func() { openLedger = old })
}

func TestWritingCommandsPushRightAfterCommitting(t *testing.T) {
	connectedHome(t)
	shared := useMemoryLedger(t)

	commands := [][]string{
		{"edit", "LCL-1", "--project", "local", "--title", "Edited on A"},
		{"move", "LCL-1", "--project", "local", "in_progress"},
		{"new", "--project", "local", "--title", "Second on A"},
		{"batch", "new", "wave", "--project", "local"},
		{"edit", "LCL-1", "--project", "local", "--batch", "wave"},
		{"batch", "rename", "wave", "tide", "--project", "local"},
		{"batch", "move", "tide", "todo", "--project", "local"},
		{"batch", "rm", "tide", "--project", "local"},
		{"rename", "elsewhere", "--project", "local"},
		{"rm", "LCL-2", "--project", "elsewhere"},
		{"prune", "--project", "elsewhere", "--status", "todo", "--yes"},
	}
	for _, args := range commands {
		if args[0] == "rm" {
			// Stamps have second resolution and a tombstone must outrank the
			// row it deletes, so the deletions land in a later second.
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
		}
		before, _ := shared.ReadAfter(0, 0)
		_, stderr, code := captureOutput(t, func() int { return runCLI(args) })
		if code != 0 {
			t.Fatalf("%v exit = %d\nstderr: %s", args, code, stderr)
		}
		if stderr != "" {
			t.Fatalf("%v wrote to stderr with a reachable Ledger: %q", args, stderr)
		}
		after, _ := shared.ReadAfter(0, 0)
		if len(after) <= len(before) {
			t.Fatalf("%v left the Ledger at %d entries; the command must push its Changes", args, len(after))
		}
	}

	// Device B pulls everything A pushed, without A ever running "ito sync".
	otherDB, err := itostore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other := itostore.New(otherDB)
	if _, err := other.Sync(shared); err != nil {
		t.Fatal(err)
	}
	p, found, err := other.FindProjectByName("elsewhere")
	if err != nil || !found {
		t.Fatalf("renamed Project missing on B: found=%v err=%v", found, err)
	}
	if _, err := other.FindIssue(p, "LCL-1"); !errors.Is(err, itostore.ErrNotFound) {
		t.Fatalf("LCL-1 was pruned on A but B has err=%v", err)
	}
}

func TestWritingCommandWithUnreachableLedgerWarnsOnceAndSucceeds(t *testing.T) {
	itoHome := connectedHome(t)
	useStubLedger(t, &stubLedger{Memory: ledger.NewMemory(), appendErr: errors.New("connection refused")})

	stdout, stderr, code := captureOutput(t, func() int {
		return runCLI([]string{"edit", "LCL-1", "--project", "local", "--title", "Still edits", "--json"})
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(stdout), &detail); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout)
	}
	if detail["title"] != "Still edits" {
		t.Fatalf("stdout = %s, want the edited Issue", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "connection refused") || !strings.Contains(lines[0], "ito sync") {
		t.Fatalf("stderr must be one warning naming the cause and 'ito sync', got %q", stderr)
	}

	// The Change stays pending: a later sync pushes it.
	shared := useMemoryLedger(t)
	if _, code := captureStdout(t, func() int { return runCLI([]string{"sync"}) }); code != 0 {
		t.Fatalf("sync exit = %d", code)
	}
	entries, _ := shared.ReadAfter(0, 0)
	var resent bool
	for _, entry := range entries {
		resent = resent || (entry.Kind == ledger.KindIssue && entry.Key == "LCL-1" && strings.Contains(string(entry.State), "Still edits"))
	}
	if !resent {
		t.Fatalf("the failed push must leave the edit pending in %s; Ledger holds %d entries without it", itoHome, len(entries))
	}
}

func TestWritingCommandBoundsTheTimeAnUnreachableLedgerCosts(t *testing.T) {
	connectedHome(t)
	stub := &stubLedger{Memory: ledger.NewMemory(), block: make(chan struct{}), released: make(chan struct{})}
	useStubLedger(t, stub)
	old := pushTimeout
	pushTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pushTimeout = old })

	start := time.Now()
	stdout, stderr, code := captureOutput(t, func() int {
		return runCLI([]string{"move", "LCL-1", "--project", "local", "done"})
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("command took %s with a hanging Ledger; the push must time out", elapsed)
	}
	if code != 0 || !strings.Contains(stdout, "moved") {
		t.Fatalf("exit = %d stdout = %q; the command's own result must stand", code, stdout)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q, want a timeout warning", stderr)
	}
	// Let the abandoned push finish before the store and its directory go.
	close(stub.block)
	<-stub.released
}

func TestReadOnlyCommandsNeverDialTheLedger(t *testing.T) {
	connectedHome(t)
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) {
		t.Fatal("a read-only command dialed the Ledger")
		return nil, nil
	}
	t.Cleanup(func() { openLedger = old })

	for _, args := range [][]string{
		{"show", "LCL-1", "--project", "local"},
		{"list", "--project", "local"},
		{"batch", "list", "--project", "local"},
		{"config"},
	} {
		if _, stderr, code := captureOutput(t, func() int { return runCLI(args) }); code != 0 {
			t.Fatalf("%v exit = %d\nstderr: %s", args, code, stderr)
		}
	}
}

func TestBareITOHandsTheTUIASyncOnlyWithALedgerConnected(t *testing.T) {
	oldIsTerminal, oldRunTUI := isTerminal, runTUI
	t.Cleanup(func() {
		isTerminal = oldIsTerminal
		runTUI = oldRunTUI
	})
	isTerminal = func(uintptr) bool { return true }
	var gotSync bool
	var result itostore.SyncResult
	var syncErr error
	// The store closes with runCLI, so the sync runs where the TUI would run it.
	runTUI = func(_ *itostore.Store, _ itostore.Project, opts tui.Options) error {
		gotSync = opts.Sync != nil
		if gotSync {
			result, syncErr = opts.Sync()
		}
		return nil
	}

	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	createLocalIssue(t, itoHome)
	if code := runCLI(nil); code != 0 {
		t.Fatalf("bare ito exit = %d", code)
	}
	if gotSync {
		t.Fatal("the TUI got a sync without a Ledger connected")
	}

	connectedHome(t)
	stub := &stubLedger{Memory: ledger.NewMemory()}
	useStubLedger(t, stub)
	if code := runCLI(nil); code != 0 {
		t.Fatalf("bare ito exit = %d", code)
	}
	if !gotSync {
		t.Fatal("the TUI got no sync with a Ledger connected")
	}
	if syncErr != nil {
		t.Fatalf("sync: %v", syncErr)
	}
	if result.Pushed == 0 {
		t.Fatal("the sync pushed nothing, so it never reached the Ledger")
	}
}
