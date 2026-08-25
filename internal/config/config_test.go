package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileYieldsEmptyConfig(t *testing.T) {
	t.Setenv("ITO_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if cfg.Ledger != nil {
		t.Fatalf("expected no ledger section, got %#v", cfg.Ledger)
	}
}

func TestLoadAcceptsLegacyLocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"backend":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("load legacy local config: %v", err)
	}
}

func TestLoadRejectsRetiredCloudBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"backend":"cloud","cloud":{"url":"libsql://example","token":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected retired cloud backend error")
	}
	for _, want := range []string{"cloud backend", "no longer supported", "ito ledger connect"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error must not leak the token: %q", err)
	}
}

func TestLoadRejectsUnknownBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"backend":"mars"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed config error, got %v", err)
	}
}

func TestLoadRejectsMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"backend":`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected malformed config error")
	}
}

func TestWriteUsesPrivatePermissionsAndDropsLegacyFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	path := Path(home)
	if err := os.WriteFile(path, []byte(`{"backend":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(Config{}); err != nil {
		t.Fatalf("write config: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected config permissions 0600, got %04o", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "backend") {
		t.Fatalf("written config still carries the retired backend field: %s", data)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("load written config: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(home, ".config-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary config files left behind: %v", matches)
	}
}

func TestWriteRoundTripsLedgerSection(t *testing.T) {
	t.Setenv("ITO_HOME", t.TempDir())
	if err := Write(Config{Ledger: &Ledger{URL: "libsql://ito-example.turso.io", Token: "secret"}}); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Ledger == nil || cfg.Ledger.URL != "libsql://ito-example.turso.io" || cfg.Ledger.Token != "secret" {
		t.Fatalf("ledger section = %#v", cfg.Ledger)
	}
	if err := Write(Config{}); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(); err != nil || cfg.Ledger != nil {
		t.Fatalf("after dropping the section: %#v, %v", cfg.Ledger, err)
	}
}
