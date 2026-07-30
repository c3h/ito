package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsToLocal(t *testing.T) {
	t.Setenv("ITO_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if cfg.Backend != BackendLocal {
		t.Fatalf("expected local backend, got %q", cfg.Backend)
	}
}

func TestLoadDefaultsMissingBackendToLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"cloud":{"url":"libsql://example","token":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config without backend: %v", err)
	}
	if cfg.Backend != BackendLocal {
		t.Fatalf("expected local backend, got %q", cfg.Backend)
	}
}

func TestLoadCloudConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := os.WriteFile(Path(home), []byte(`{"backend":"cloud","cloud":{"url":"libsql://example","token":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load cloud config: %v", err)
	}
	if cfg.Backend != BackendCloud || cfg.Cloud == nil {
		t.Fatalf("expected cloud backend, got %#v", cfg)
	}
	if cfg.Cloud.URL != "libsql://example" || cfg.Cloud.Token != "secret" {
		t.Fatalf("unexpected cloud config: %#v", cfg.Cloud)
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

func TestWriteUsesPrivatePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	path := Path(home)
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := Config{
		Backend: BackendCloud,
		Cloud:   &Cloud{URL: "libsql://example", Token: "secret"},
	}
	if err := Write(want); err != nil {
		t.Fatalf("write config: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected config permissions 0600, got %04o", got)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if got.Backend != want.Backend || got.Cloud == nil || *got.Cloud != *want.Cloud {
		t.Fatalf("written config mismatch: got %#v want %#v", got, want)
	}

	matches, err := filepath.Glob(filepath.Join(home, ".config-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary config files left behind: %v", matches)
	}
}
