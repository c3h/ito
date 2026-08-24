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

func TestSyncWithoutLedgerFailsWithConnectHint(t *testing.T) {
	itoHome := t.TempDir()
	for _, args := range [][]string{{"sync"}, {"sync", "--json"}} {
		result := runITO(t, t.TempDir(), itoHome, args...)
		if result.exitCode != exitGeneric {
			t.Fatalf("%v exit = %d, want %d\nstderr: %s", args, result.exitCode, exitGeneric, result.stderr)
		}
		if !strings.Contains(result.stderr, "no Ledger is connected") || !strings.Contains(result.stderr, "ito ledger connect") {
			t.Fatalf("%v must name the connect command, got stderr %q", args, result.stderr)
		}
	}
}

func TestSyncReportsPushedAndPulledCounts(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	if err := itoconfig.Write(itoconfig.Config{Ledger: &itoconfig.Ledger{}}); err != nil {
		t.Fatal(err)
	}
	shared := ledger.NewMemory()
	oldOpenLedger := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) { return shared, nil }
	t.Cleanup(func() { openLedger = oldOpenLedger })

	// Another Device already pushed one Issue.
	otherDB, err := itostore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other := itostore.New(otherDB)
	otherProject, err := other.CreateProject("remote", "RMT", filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateIssue(otherProject, "From the other Device", "todo", "medium", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Sync(shared); err != nil {
		t.Fatal(err)
	}

	// This Device holds one pending Issue of its own.
	db, err := itostore.Open(itoHome)
	if err != nil {
		t.Fatal(err)
	}
	st := itostore.New(db)
	local, err := st.CreateProject("local", "LCL", filepath.Join(t.TempDir(), "local"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIssue(local, "Pending here", "todo", "medium", nil, ""); err != nil {
		t.Fatal(err)
	}
	db.Close()

	stdout, code := captureStdout(t, func() int { return runCLI([]string{"sync", "--json"}) })
	if code != 0 {
		t.Fatalf("ito sync --json exit = %d", code)
	}
	var result itostore.SyncResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a sync result: %v\nstdout: %s", err, stdout)
	}
	if result.Pushed != 1 || result.Pulled != 1 {
		t.Fatalf("sync result = %+v, want pushed 1 pulled 1", result)
	}

	stdout, code = captureStdout(t, func() int { return runCLI([]string{"sync"}) })
	if code != 0 {
		t.Fatalf("ito sync exit = %d", code)
	}
	if stdout != "Pushed 0, pulled 0.\n" {
		t.Fatalf("idle sync output = %q", stdout)
	}
}

func captureStdout(t *testing.T, run func() int) (string, int) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	code := run()
	os.Stdout = old
	writer.Close()
	output := <-done
	reader.Close()
	return output, code
}
