package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	itoconfig "github.com/c3h/ito/internal/config"
	"github.com/c3h/ito/internal/ledger"
	itostore "github.com/c3h/ito/internal/store"
)

const (
	testLedgerURL   = "libsql://ito-example.turso.io"
	testLedgerToken = "s3cret-token"
)

// useMemoryLedger routes the CLI's Ledger dialing to one in-memory Ledger and
// checks the configured URL and token reach it.
func useMemoryLedger(t *testing.T) *ledger.Memory {
	t.Helper()
	shared := ledger.NewMemory()
	old := openLedger
	openLedger = func(cfg itoconfig.Ledger) (ledger.Ledger, error) {
		if cfg.URL != testLedgerURL || cfg.Token != testLedgerToken {
			t.Fatalf("dialed with %+v, want the configured URL and token", cfg)
		}
		return shared, nil
	}
	t.Cleanup(func() { openLedger = old })
	return shared
}

// pushFromAnotherDevice populates the Ledger with a Project and one Issue made
// elsewhere.
func pushFromAnotherDevice(t *testing.T, shared ledger.Ledger) {
	t.Helper()
	db, err := itostore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := itostore.New(db)
	p, err := st.CreateProject("remote", "RMT", filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIssue(p, "From the other Device", "todo", "medium", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sync(shared); err != nil {
		t.Fatal(err)
	}
}

func createLocalIssue(t *testing.T, itoHome string) {
	t.Helper()
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := itostore.New(db)
	p, err := st.CreateProject("local", "LCL", filepath.Join(t.TempDir(), "local"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIssue(p, "Made here", "todo", "medium", nil, ""); err != nil {
		t.Fatal(err)
	}
}

func countIssues(t *testing.T, itoHome string) int {
	t.Helper()
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	issues, err := itostore.New(db).ListIssues(itostore.ListOptions{AllProjects: true, IncludeDone: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(issues)
}

func connectArgs(extra ...string) []string {
	return append([]string{"ledger", "connect", "--url", testLedgerURL, "--token", testLedgerToken}, extra...)
}

func TestLedgerConnectOnEmptyStorePullsEverything(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	pushFromAnotherDevice(t, shared)

	stdout, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs("--json")) })
	if code != 0 {
		t.Fatalf("connect exit = %d\nstderr: %s", code, stderr)
	}
	var result struct {
		URL    string `json:"url"`
		Device string `json:"device"`
		Pulled int    `json:"pulled"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if result.URL != testLedgerURL || result.Device == "" || result.Pulled != 2 {
		t.Fatalf("connect result = %+v", result)
	}
	if got := countIssues(t, itoHome); got != 1 {
		t.Fatalf("issues after connect = %d, want 1", got)
	}

	cfg, err := itoconfig.Load()
	if err != nil || cfg.Ledger == nil || cfg.Ledger.URL != testLedgerURL || cfg.Ledger.Token != testLedgerToken {
		t.Fatalf("config after connect = %#v, %v", cfg.Ledger, err)
	}
	info, err := os.Stat(itoconfig.Path(itoHome))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", got)
	}

	// The position is recorded: a second sync has nothing left to pull.
	stdout, _, code = captureOutput(t, func() int { return runCLI([]string{"sync"}) })
	if code != 0 || stdout != "Pushed 0, pulled 0.\n" {
		t.Fatalf("sync after connect: exit %d, stdout %q", code, stdout)
	}

	// Connecting twice is refused: the Device is already connected.
	_, stderr, code = captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code == 0 || !strings.Contains(stderr, "already connected") || !strings.Contains(stderr, "ito ledger disconnect") {
		t.Fatalf("second connect: exit %d, stderr %q", code, stderr)
	}
}

func TestLedgerConnectRefusesTwoPopulatedSidesWithoutForce(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	pushFromAnotherDevice(t, shared)
	createLocalIssue(t, itoHome)

	for _, args := range [][]string{connectArgs(), connectArgs("--json")} {
		stdout, stderr, code := captureOutput(t, func() int { return runCLI(args) })
		if code != exitGeneric {
			t.Fatalf("%v exit = %d, want %d\nstdout: %s\nstderr: %s", args, code, exitGeneric, stdout, stderr)
		}
		if !strings.Contains(stderr, "--force") {
			t.Fatalf("%v must name --force, got stderr %q", args, stderr)
		}
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("a refused connect must not write the config: %#v, %v", cfg.Ledger, err)
	}
	if got := countIssues(t, itoHome); got != 1 {
		t.Fatalf("issues after refused connect = %d, want 1", got)
	}

	stdout, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs("--force")) })
	if code != 0 {
		t.Fatalf("forced connect exit = %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Connected to "+testLedgerURL) {
		t.Fatalf("forced connect stdout = %q", stdout)
	}
	if got := countIssues(t, itoHome); got != 2 {
		t.Fatalf("issues after forced connect = %d, want both sides", got)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger == nil {
		t.Fatalf("config after forced connect = %#v, %v", cfg.Ledger, err)
	}
}

// seedLocalTracker fills the local store with every row kind, then wipes the
// change log so the rows look like they predate it.
func seedLocalTracker(t *testing.T, itoHome string, issues int) {
	t.Helper()
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := itostore.New(db)
	p, err := st.CreateProject("local", "LCL", filepath.Join(t.TempDir(), "local"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBatch(p, "wave-1"); err != nil {
		t.Fatal(err)
	}
	var previous string
	for i := 0; i < issues; i++ {
		issue, err := st.CreateIssueInBatch(p, fmt.Sprintf("Café %d", i), "todo", "medium", []string{"feature"}, "espresso", "wave-1")
		if err != nil {
			t.Fatal(err)
		}
		if previous != "" {
			if _, err := st.Edit(p, issue.ID, itostore.EditIssueOptions{LinkOps: []itostore.LinkEditOp{{Action: "add", Kind: "blocked_by", Target: previous}}}); err != nil {
				t.Fatal(err)
			}
		}
		previous = issue.ID
	}
	if _, err := db.Exec(`DELETE FROM changes`); err != nil {
		t.Fatal(err)
	}
}

func rowCounts(t *testing.T, itoHome string) map[string]int {
	t.Helper()
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	counts, err := itostore.New(db).RowCounts()
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestLedgerConnectSnapshotsAPopulatedStoreIntoAnEmptyLedger(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	seedLocalTracker(t, itoHome, 300)

	stdout, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs("--json")) })
	if code != 0 {
		t.Fatalf("connect exit = %d\nstderr: %s", code, stderr)
	}
	var result struct {
		Pushed int `json:"pushed"`
		Pulled int `json:"pulled"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	// project + batch + 300 issues + 300 labels + 299 links
	if result.Pushed != 901 || result.Pulled != 0 {
		t.Fatalf("connect result = %+v, want 901 pushed and nothing pulled", result)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger == nil {
		t.Fatalf("config after snapshot = %#v, %v", cfg.Ledger, err)
	}

	// A fresh Device bootstraps identical rows from the Ledger, search included.
	freshHome := t.TempDir()
	db, err := itostore.Open(freshHome)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fresh := itostore.New(db)
	if _, err := fresh.Sync(shared); err != nil {
		t.Fatal(err)
	}
	if local, remote := rowCounts(t, itoHome), rowCounts(t, freshHome); !maps.Equal(local, remote) {
		t.Fatalf("fresh Device holds %v, origin %v", remote, local)
	}
	p, found, err := fresh.FindProjectByName("local")
	if err != nil || !found {
		t.Fatalf("project on the fresh Device: found=%v err=%v", found, err)
	}
	last, err := fresh.FindIssue(p, "LCL-300")
	if err != nil {
		t.Fatal(err)
	}
	if last.Batch == nil || *last.Batch != "wave-1" || len(last.Labels) != 1 || len(last.BlockedBy) != 1 || last.BlockedBy[0] != "LCL-299" {
		t.Fatalf("pulled %#v, want batch, label and link intact", last)
	}
	if hits, err := fresh.ListIssues(itostore.ListOptions{ProjectID: p.ID, Search: "cafe"}); err != nil || len(hits) != 300 {
		t.Fatalf("search on the fresh Device found %d, %v", len(hits), err)
	}
}

// failingLedger fails the first n Appends after storing part of the batch,
// the shape of a connection dropping mid-push.
type failingLedger struct {
	*ledger.Memory
	failures int
}

func (f *failingLedger) Append(changes []ledger.Change) ([]int64, error) {
	if f.failures > 0 {
		f.failures--
		half := changes[:len(changes)/2]
		if _, err := f.Memory.Append(half); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("connection reset")
	}
	return f.Memory.Append(changes)
}

func TestLedgerConnectSnapshotFailureLeavesNoConfigAndRetriesWithoutDuplicates(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := &failingLedger{Memory: ledger.NewMemory(), failures: 1}
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) { return shared, nil }
	t.Cleanup(func() { openLedger = old })
	seedLocalTracker(t, itoHome, 10)

	_, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code != exitGeneric || !strings.Contains(stderr, "could not push the snapshot") {
		t.Fatalf("first connect: exit %d, stderr %q", code, stderr)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("a failed snapshot must not write the config: %#v, %v", cfg.Ledger, err)
	}
	if entries, err := shared.ReadAfter(0, 1000); err != nil || len(entries) != 15 {
		t.Fatalf("the Ledger holds %d entries after the interrupted push, want the 15 that landed", len(entries))
	}

	stdout, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code != 0 {
		t.Fatalf("retry: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "pushed 31,") {
		t.Fatalf("retry summary = %q, want the whole snapshot pushed", stdout)
	}
	entries, err := shared.ReadAfter(0, 1000)
	if err != nil || len(entries) != 31 {
		t.Fatalf("the Ledger holds %d entries after the retry, want 31 with no duplicates", len(entries))
	}
	inventory, err := ledger.TakeInventory(shared)
	if err != nil || !maps.Equal(inventory.Rows, rowCounts(t, itoHome)) {
		t.Fatalf("ledger rows %v, local %v", inventory.Rows, rowCounts(t, itoHome))
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger == nil {
		t.Fatalf("config after the retry = %#v, %v", cfg.Ledger, err)
	}
}

// lossyLedger acknowledges every Change but keeps none: the snapshot must
// notice through the count validation.
type lossyLedger struct{ *ledger.Memory }

func (l lossyLedger) Append(changes []ledger.Change) ([]int64, error) {
	return make([]int64, len(changes)), nil
}

func TestLedgerConnectRefusesToRecordAnUnvalidatedSnapshot(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) { return lossyLedger{ledger.NewMemory()}, nil }
	t.Cleanup(func() { openLedger = old })
	seedLocalTracker(t, itoHome, 3)

	_, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code != exitGeneric || !strings.Contains(stderr, "did not validate") || !strings.Contains(stderr, "the Ledger reports 0 project") {
		t.Fatalf("connect: exit %d, stderr %q", code, stderr)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("an unvalidated snapshot must not write the config: %#v, %v", cfg.Ledger, err)
	}
}

func TestLedgerDisconnectKeepsLocalRowsAndSyncThenFails(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	pushFromAnotherDevice(t, shared)
	if _, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) }); code != 0 {
		t.Fatalf("connect exit = %d\nstderr: %s", code, stderr)
	}

	stdout, stderr, code := captureOutput(t, func() int { return runCLI([]string{"ledger", "disconnect"}) })
	if code != 0 {
		t.Fatalf("disconnect exit = %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Disconnected from "+testLedgerURL) {
		t.Fatalf("disconnect stdout = %q", stdout)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("config after disconnect = %#v, %v", cfg.Ledger, err)
	}
	if got := countIssues(t, itoHome); got != 1 {
		t.Fatalf("issues after disconnect = %d, want the pulled row kept", got)
	}

	_, stderr, code = captureOutput(t, func() int { return runCLI([]string{"sync"}) })
	if code != exitGeneric || !strings.Contains(stderr, "no Ledger is connected") || !strings.Contains(stderr, "ito ledger connect") {
		t.Fatalf("sync after disconnect: exit %d, stderr %q", code, stderr)
	}

	_, stderr, code = captureOutput(t, func() int { return runCLI([]string{"ledger", "disconnect", "--json"}) })
	if code != exitGeneric || !strings.Contains(stderr, "no Ledger is connected") {
		t.Fatalf("second disconnect: exit %d, stderr %q", code, stderr)
	}
}

func TestConfigShowsLedgerURLAndDeviceButNeverTheToken(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	useMemoryLedger(t)

	stdout, _, code := captureOutput(t, func() int { return runCLI([]string{"config", "--json"}) })
	if code != 0 {
		t.Fatalf("config --json exit = %d", code)
	}
	var before struct {
		LocalDBPath string          `json:"local_db_path"`
		Ledger      json.RawMessage `json:"ledger"`
	}
	if err := json.Unmarshal([]byte(stdout), &before); err != nil {
		t.Fatalf("config --json output: %v\n%s", err, stdout)
	}
	if string(before.Ledger) != "null" {
		t.Fatalf("disconnected config must carry ledger: null, got %s", before.Ledger)
	}

	if _, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) }); code != 0 {
		t.Fatalf("connect exit = %d\nstderr: %s", code, stderr)
	}

	stdout, _, code = captureOutput(t, func() int { return runCLI([]string{"config"}) })
	if code != 0 {
		t.Fatalf("config exit = %d", code)
	}
	if !strings.Contains(stdout, "Ledger: "+testLedgerURL) || !strings.Contains(stdout, "Device: ") {
		t.Fatalf("config stdout = %q", stdout)
	}
	if strings.Contains(stdout, testLedgerToken) {
		t.Fatalf("config leaks the token: %q", stdout)
	}

	stdout, _, code = captureOutput(t, func() int { return runCLI([]string{"config", "--json"}) })
	if code != 0 {
		t.Fatalf("config --json exit = %d", code)
	}
	var after struct {
		Ledger struct {
			URL    string `json:"url"`
			Device string `json:"device"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal([]byte(stdout), &after); err != nil {
		t.Fatalf("config --json output: %v\n%s", err, stdout)
	}
	if after.Ledger.URL != testLedgerURL || len(after.Ledger.Device) != 32 {
		t.Fatalf("config --json ledger = %+v", after.Ledger)
	}
	if strings.Contains(stdout, testLedgerToken) || strings.Contains(stdout, "token") {
		t.Fatalf("config --json leaks the token: %q", stdout)
	}
}

func TestLedgerCommandUsage(t *testing.T) {
	itoHome := t.TempDir()
	for _, args := range [][]string{
		{"ledger"},
		{"ledger", "--help"},
		{"ledger", "connect", "--help"},
		{"ledger", "disconnect", "--help"},
	} {
		result := runITO(t, t.TempDir(), itoHome, args...)
		if result.exitCode != 0 || !strings.Contains(result.stdout, "usage: ito ledger") {
			t.Fatalf("%v: exit %d, stdout %q", args, result.exitCode, result.stdout)
		}
	}
	for _, args := range [][]string{
		{"ledger", "connect"},
		{"ledger", "connect", "--url", testLedgerURL},
		{"ledger", "connect", "--token", testLedgerToken},
		{"ledger", "connect", "--url", testLedgerURL, "--token", testLedgerToken, "extra"},
		{"ledger", "disconnect", "extra"},
		{"ledger", "bogus"},
	} {
		result := runITO(t, t.TempDir(), itoHome, args...)
		if result.exitCode != exitBadUsage {
			t.Fatalf("%v: exit %d, want %d\nstderr: %s", args, result.exitCode, exitBadUsage, result.stderr)
		}
	}
	if _, err := os.Stat(itoconfig.Path(itoHome)); !os.IsNotExist(err) {
		t.Fatalf("usage errors must not write the config: %v", err)
	}
}

func TestLedgerConnectAfterALostConfigAgainstASharedLedgerNeedsForce(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	seedLocalTracker(t, itoHome, 2)
	if _, _, code := captureOutput(t, func() int { return runCLI(connectArgs()) }); code != 0 {
		t.Fatalf("first connect exit = %d", code)
	}
	pushFromAnotherDevice(t, shared)
	if err := os.Remove(itoconfig.Path(itoHome)); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code != exitGeneric || !strings.Contains(stderr, "--force") {
		t.Fatalf("reconnect: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs("--force")) })
	if code != 0 {
		t.Fatalf("forced reconnect: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "pulled 2.") || countIssues(t, itoHome) != 3 {
		t.Fatalf("forced reconnect = %q, issues %d; want the other Device's Issue merged in", stdout, countIssues(t, itoHome))
	}
	if entries, err := shared.ReadAfter(0, 1000); err != nil || len(entries) != 9 {
		t.Fatalf("the Ledger holds %d entries after the forced reconnect, want 7 + 2 with no duplicates", len(entries))
	}
}
