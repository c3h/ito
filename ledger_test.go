package main

import (
	"encoding/json"
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

func TestLedgerConnectRefusesPopulatedStoreAgainstEmptyLedger(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	useMemoryLedger(t)
	createLocalIssue(t, itoHome)

	_, stderr, code := captureOutput(t, func() int { return runCLI(connectArgs()) })
	if code != exitGeneric || !strings.Contains(stderr, "empty Ledger") {
		t.Fatalf("connect exit = %d, stderr %q", code, stderr)
	}
	if cfg, err := itoconfig.Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("a refused connect must not write the config: %#v, %v", cfg.Ledger, err)
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
