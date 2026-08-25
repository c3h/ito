package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	itoconfig "github.com/c3h/ito/internal/config"
	"github.com/c3h/ito/internal/ledger"
)

func TestNewReservesConsecutiveNumbersFromTheLedgerAcrossDevices(t *testing.T) {
	shared := useMemoryLedger(t)
	// Device A holds LCL-1 locally and shares it; Device B starts empty and
	// pulls it on connect.
	homeA := connectedHome(t)
	if _, code := captureStdout(t, func() int { return runCLI([]string{"sync"}) }); code != 0 {
		t.Fatalf("sync A exit = %d", code)
	}
	homeB := t.TempDir()
	t.Setenv("ITO_HOME", homeB)
	if _, code := captureStdout(t, func() int { return runCLI(connectArgs()) }); code != 0 {
		t.Fatalf("connect B exit = %d", code)
	}

	var ids []string
	for i, home := range []string{homeA, homeB, homeA, homeB} {
		t.Setenv("ITO_HOME", home)
		stdout, stderr, code := captureOutput(t, func() int {
			return runCLI([]string{"new", "--project", "local", "--title", "Numbered", "--json"})
		})
		if code != 0 {
			t.Fatalf("new %d exit = %d; stderr: %s", i, code, stderr)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(stdout), &detail); err != nil {
			t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout)
		}
		ids = append(ids, detail["id"].(string))
	}
	want := []string{"LCL-2", "LCL-3", "LCL-4", "LCL-5"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	entries, _ := shared.ReadAfter(0, 0)
	var seen int
	for _, entry := range entries {
		if entry.Kind == ledger.KindIssue && entry.Key != "LCL-1" {
			seen++
		}
	}
	if seen != 4 {
		t.Fatalf("the Ledger holds %d new Issue Changes, want 4", seen)
	}
}

func TestNewWithoutALedgerNumbersLocally(t *testing.T) {
	itoHome := t.TempDir()
	t.Setenv("ITO_HOME", itoHome)
	createLocalIssue(t, itoHome)
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) {
		t.Fatal("dialed a Ledger with none connected")
		return nil, nil
	}
	t.Cleanup(func() { openLedger = old })

	stdout, _, code := captureOutput(t, func() int {
		return runCLI([]string{"new", "--project", "local", "--title", "Local", "--json"})
	})
	if code != 0 || !strings.Contains(stdout, `"id":"LCL-2"`) {
		t.Fatalf("exit = %d, stdout = %s; want LCL-2", code, stdout)
	}
}

func TestNewWithAnUnreachableLedgerFailsAndWritesNothing(t *testing.T) {
	itoHome := connectedHome(t)
	old := openLedger
	openLedger = func(itoconfig.Ledger) (ledger.Ledger, error) { return nil, errors.New("connection refused") }
	t.Cleanup(func() { openLedger = old })

	stdout, stderr, code := captureOutput(t, func() int {
		return runCLI([]string{"new", "--project", "local", "--title", "Never lands"})
	})
	if code == 0 {
		t.Fatalf("exit = 0, want failure; stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "connection refused") || !strings.Contains(stderr, "Issue number") {
		t.Fatalf("stderr must name the cause and the reservation, got %q", stderr)
	}
	if n := countIssues(t, itoHome); n != 1 {
		t.Fatalf("store holds %d issues, want only LCL-1", n)
	}
	// Nothing is pending: a later sync pushes only what connectedHome left.
	shared := useMemoryLedger(t)
	if _, code := captureStdout(t, func() int { return runCLI([]string{"sync"}) }); code != 0 {
		t.Fatalf("sync exit = %d", code)
	}
	entries, _ := shared.ReadAfter(0, 0)
	for _, entry := range entries {
		if entry.Kind == ledger.KindIssue && entry.Key != "LCL-1" {
			t.Fatalf("a failed creation left Change %s pending", entry.Key)
		}
	}
}
