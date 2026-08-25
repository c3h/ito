package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	itoconfig "github.com/c3h/ito/internal/config"
	itostore "github.com/c3h/ito/internal/store"
	"github.com/c3h/ito/internal/tui"
)

// firstRunTTY drives bare ito as if on a terminal with the given keystrokes
// and records what reached the TUI launcher.
func firstRunTTY(t *testing.T, input string) (gotTUI *bool, gotSync *tui.SyncFunc) {
	t.Helper()
	oldIsTerminal, oldRunTUI, oldStdin := isTerminal, runTUI, stdin
	t.Cleanup(func() {
		isTerminal = oldIsTerminal
		runTUI = oldRunTUI
		stdin = oldStdin
	})
	isTerminal = func(uintptr) bool { return true }
	stdin = strings.NewReader(input)
	gotTUI, gotSync = new(bool), new(tui.SyncFunc)
	runTUI = func(_ *itostore.Store, _ itostore.Project, opts tui.Options) error {
		*gotTUI = true
		*gotSync = opts.Sync
		return nil
	}
	return gotTUI, gotSync
}

// stayLocal records the "start empty" choice for a home, so bare ito on a
// terminal opens the TUI without asking.
func stayLocal(t *testing.T, itoHome string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(itoHome, "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBareITOWithoutTTYAndWithoutConfigNamesTheConnectCommand(t *testing.T) {
	result := runITO(t, t.TempDir(), t.TempDir())
	if result.exitCode == 0 {
		t.Fatalf("expected bare ito without a config to fail outside a TTY\nstdout: %s", result.stdout)
	}
	if result.stdout != "" || !strings.Contains(result.stderr, "ito ledger connect --url <url> --token <token>") {
		t.Fatalf("expected the connect command on stderr\nstdout: %s\nstderr: %s", result.stdout, result.stderr)
	}
}

func TestFirstRunStartEmptyOpensALocalTUIAndRemembersTheChoice(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	gotTUI, gotSync := firstRunTTY(t, "1\n")

	stdout, _, code := captureOutput(t, func() int { return runCLI(nil) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !*gotTUI || *gotSync != nil {
		t.Fatalf("expected a local TUI (opened=%v, sync=%v)", *gotTUI, *gotSync != nil)
	}
	if !strings.Contains(stdout, "Start empty") || !strings.Contains(stdout, "Connect") {
		t.Fatalf("expected both choices on stdout, got %q", stdout)
	}
	if _, err := os.Stat(itoconfig.Path(itoHome)); err != nil {
		t.Fatalf("expected the choice to be remembered in a config file: %v", err)
	}

	// The next launch must not ask again: an empty stdin would otherwise fail.
	gotTUI, _ = firstRunTTY(t, "")
	if code := runCLI(nil); code != 0 || !*gotTUI {
		t.Fatalf("second launch: exit = %d, opened = %v", code, *gotTUI)
	}
}

func TestFirstRunConnectPullsTheTrackerAndOpensTheTUI(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	shared := useMemoryLedger(t)
	pushFromAnotherDevice(t, shared)
	gotTUI, gotSync := firstRunTTY(t, "2\n"+testLedgerURL+"\n"+testLedgerToken+"\n")

	stdout, stderr, code := captureOutput(t, func() int { return runCLI(nil) })
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	if !*gotTUI || *gotSync == nil {
		t.Fatalf("expected a TUI with a sync (opened=%v, sync=%v)", *gotTUI, *gotSync != nil)
	}
	if strings.Contains(stdout, testLedgerToken) {
		t.Fatalf("the token leaked to stdout: %q", stdout)
	}
	if n := countIssues(t, itoHome); n != 1 {
		t.Fatalf("expected the pulled Issue in the local store, got %d", n)
	}
	cfg, err := itoconfig.Load()
	if err != nil || cfg.Ledger == nil || cfg.Ledger.URL != testLedgerURL {
		t.Fatalf("expected the config to record the Ledger, got %+v (%v)", cfg, err)
	}
}

func TestFirstRunRejectsAnUnknownChoiceOrAClosedStdin(t *testing.T) {
	for _, input := range []string{"3\n", ""} {
		t.Setenv("ITO_HOME", t.TempDir())
		gotTUI, _ := firstRunTTY(t, input)
		_, stderr, code := captureOutput(t, func() int { return runCLI(nil) })
		if code == 0 || *gotTUI {
			t.Fatalf("input %q: exit = %d, opened = %v", input, code, *gotTUI)
		}
		if !strings.Contains(stderr, "ito ledger connect") {
			t.Fatalf("input %q: expected the connect command in %q", input, stderr)
		}
	}
}

func TestFirstRunConnectFailureLeavesNoConfigBehind(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	useMemoryLedger(t)
	createLocalIssue(t, itoHome) // populated store against an empty Ledger is refused
	gotTUI, _ := firstRunTTY(t, "2\n"+testLedgerURL+"\n"+testLedgerToken+"\n")

	_, stderr, code := captureOutput(t, func() int { return runCLI(nil) })
	if code == 0 || *gotTUI {
		t.Fatalf("exit = %d, opened = %v", code, *gotTUI)
	}
	if !strings.Contains(stderr, "empty Ledger") {
		t.Fatalf("expected the connect policy message, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(itoHome, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no config after a refused connect, stat err = %v", err)
	}
}
