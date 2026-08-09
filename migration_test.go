package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	itoconfig "github.com/c3h/ito/internal/config"
	itostore "github.com/c3h/ito/internal/store"
)

func TestCopyDatabaseCopiesAllTablesInBatches(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "source.db")
	dstPath := filepath.Join(t.TempDir(), "destination.db")
	src := openMigrationTestDatabase(t, srcPath)
	dst := openMigrationTestDatabase(t, dstPath)
	defer src.Close()
	defer dst.Close()
	seedMigrationTestDatabase(t, src, "SRC", 205)

	if err := copyDatabase(src, dst, false); err != nil {
		t.Fatalf("copy database: %v", err)
	}

	for _, table := range migrationTables {
		srcCount, err := tableRowCount(src, table.name)
		if err != nil {
			t.Fatal(err)
		}
		dstCount, err := tableRowCount(dst, table.name)
		if err != nil {
			t.Fatal(err)
		}
		if dstCount != srcCount {
			t.Fatalf("%s count = %d, want %d", table.name, dstCount, srcCount)
		}
	}
	var matches int
	if err := dst.QueryRow(`SELECT count(*) FROM issues_fts WHERE issues_fts MATCH ?`, "migration").Scan(&matches); err != nil {
		t.Fatalf("query rebuilt search index: %v", err)
	}
	if matches != 205 {
		t.Fatalf("search matches = %d, want 205", matches)
	}
	var branch string
	if err := dst.QueryRow(`SELECT branch FROM issues WHERE id = 'SRC-1'`).Scan(&branch); err != nil {
		t.Fatalf("read copied branch: %v", err)
	}
	if branch != "feat/migration-1" {
		t.Fatalf("copied branch = %q, want %q", branch, "feat/migration-1")
	}
}

func TestCopyDatabaseRefusesNonEmptyDestinationAndForceReplacesIt(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "source.db")
	dstPath := filepath.Join(t.TempDir(), "destination.db")
	src := openMigrationTestDatabase(t, srcPath)
	dst := openMigrationTestDatabase(t, dstPath)
	defer src.Close()
	defer dst.Close()
	seedMigrationTestDatabase(t, src, "SRC", 3)
	seedMigrationTestDatabase(t, dst, "OLD", 2)

	err := copyDatabase(src, dst, false)
	if err == nil || !strings.Contains(err.Error(), "rerun with --force") {
		t.Fatalf("expected non-empty destination error, got %v", err)
	}
	var oldProjects int
	if err := dst.QueryRow(`SELECT count(*) FROM projects WHERE prefix = ?`, "OLD").Scan(&oldProjects); err != nil {
		t.Fatal(err)
	}
	if oldProjects != 1 {
		t.Fatal("non-force copy modified the destination")
	}

	if err := copyDatabase(src, dst, true); err != nil {
		t.Fatalf("force copy database: %v", err)
	}
	if err := dst.QueryRow(`SELECT count(*) FROM projects WHERE prefix = ?`, "OLD").Scan(&oldProjects); err != nil {
		t.Fatal(err)
	}
	if oldProjects != 0 {
		t.Fatal("force copy retained old destination data")
	}
	var copiedIssues int
	if err := dst.QueryRow(`SELECT count(*) FROM issues WHERE id LIKE ?`, "SRC-%").Scan(&copiedIssues); err != nil {
		t.Fatal(err)
	}
	if copiedIssues != 3 {
		t.Fatalf("copied issues = %d, want 3", copiedIssues)
	}
}

func TestCopyDatabaseDetectsPerTableRowCountMismatch(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "source.db")
	dstPath := filepath.Join(t.TempDir(), "destination.db")
	src := openMigrationTestDatabase(t, srcPath)
	dst := openMigrationTestDatabase(t, dstPath)
	defer src.Close()
	defer dst.Close()
	seedMigrationTestDatabase(t, src, "SRC", 2)
	if _, err := dst.Exec(`
CREATE TRIGGER ignore_migrated_labels
BEFORE INSERT ON issue_labels
BEGIN
  SELECT RAISE(IGNORE);
END
`); err != nil {
		t.Fatal(err)
	}

	err := copyDatabase(src, dst, false)
	if err == nil || !strings.Contains(err.Error(), `row count mismatch for table "issue_labels"`) {
		t.Fatalf("expected issue_labels count mismatch, got %v", err)
	}
}

func TestMigrateCloudWritesConfigOnlyAfterValidation(t *testing.T) {
	t.Run("failure keeps local config", func(t *testing.T) {
		home := t.TempDir()
		dstPath := filepath.Join(t.TempDir(), "destination.db")
		t.Setenv("ITO_HOME", home)
		src := openMigrationTestDatabase(t, itoconfig.LocalDBPath(home))
		seedMigrationTestDatabase(t, src, "SRC", 2)
		src.Close()
		dst := openMigrationTestDatabase(t, dstPath)
		if _, err := dst.Exec(`
CREATE TRIGGER ignore_migrated_labels
BEFORE INSERT ON issue_labels
BEGIN
  SELECT RAISE(IGNORE);
END
`); err != nil {
			t.Fatal(err)
		}
		dst.Close()
		if err := itoconfig.Write(itoconfig.Config{Backend: itoconfig.BackendLocal}); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(itoconfig.Path(home))
		if err != nil {
			t.Fatal(err)
		}

		err = migrateCloud("libsql://example.turso.io", "secret", false, sqliteCloudOpener(dstPath))
		if err == nil || !strings.Contains(err.Error(), "row count mismatch") {
			t.Fatalf("expected validation failure, got %v", err)
		}
		after, err := os.ReadFile(itoconfig.Path(home))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("config changed on failure:\nbefore: %s\nafter: %s", before, after)
		}
	})

	t.Run("success activates cloud and keeps source", func(t *testing.T) {
		home := t.TempDir()
		dstPath := filepath.Join(t.TempDir(), "destination.db")
		t.Setenv("ITO_HOME", home)
		src := openMigrationTestDatabase(t, itoconfig.LocalDBPath(home))
		seedMigrationTestDatabase(t, src, "SRC", 3)
		src.Close()
		dst := openMigrationTestDatabase(t, dstPath)
		dst.Close()

		if err := migrateCloud("libsql://example.turso.io", "secret", false, sqliteCloudOpener(dstPath)); err != nil {
			t.Fatalf("migrate cloud: %v", err)
		}
		cfg, err := itoconfig.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backend != itoconfig.BackendCloud || cfg.Cloud == nil ||
			cfg.Cloud.URL != "libsql://example.turso.io" || cfg.Cloud.Token != "secret" {
			t.Fatalf("unexpected config: %#v", cfg)
		}
		info, err := os.Stat(itoconfig.Path(home))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
		}

		sourceAfter, err := sql.Open("sqlite", "file:"+itoconfig.LocalDBPath(home)+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer sourceAfter.Close()
		var sourceIssues int
		if err := sourceAfter.QueryRow(`SELECT count(*) FROM issues`).Scan(&sourceIssues); err != nil {
			t.Fatal(err)
		}
		if sourceIssues != 3 {
			t.Fatalf("local source issues = %d, want 3", sourceIssues)
		}
	})
}

func TestMigrateCloudCommandValidationAndBackendGuard(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	missingToken := runITO(t, repo, home, "migrate", "cloud", "--url", "libsql://example.turso.io")
	if missingToken.exitCode != exitBadUsage || !strings.Contains(missingToken.stderr, "requires --token") {
		t.Fatalf("unexpected missing-token result: %#v", missingToken)
	}

	t.Setenv("ITO_HOME", home)
	if err := itoconfig.Write(itoconfig.Config{
		Backend: itoconfig.BackendCloud,
		Cloud:   &itoconfig.Cloud{URL: "libsql://active.turso.io", Token: "active-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	alreadyCloud := runITO(t, repo, home, "migrate", "cloud", "--url", "libsql://new.turso.io", "--token", "new-secret")
	if alreadyCloud.exitCode != exitGeneric || !strings.Contains(alreadyCloud.stderr, "run 'ito migrate local' first") {
		t.Fatalf("unexpected already-cloud result: %#v", alreadyCloud)
	}
	if strings.Contains(alreadyCloud.stderr, "active-secret") || strings.Contains(alreadyCloud.stderr, "new-secret") {
		t.Fatalf("migration error exposed a token: %s", alreadyCloud.stderr)
	}
}

func TestMigrateLocalRefusesAlreadyLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := itoconfig.Write(itoconfig.Config{Backend: itoconfig.BackendLocal}); err != nil {
		t.Fatal(err)
	}

	openedCloud := false
	err := migrateLocal(false, func(string, string) (*sql.DB, error) {
		openedCloud = true
		return nil, fmt.Errorf("cloud opener should not be called")
	})
	if err == nil || !strings.Contains(err.Error(), "active backend is already local") ||
		!strings.Contains(err.Error(), "ito migrate cloud") {
		t.Fatalf("expected actionable already-local error, got %v", err)
	}
	if openedCloud {
		t.Fatal("opened the cloud database while the backend was already local")
	}
}

func TestMigrateLocalForceReplacesDestinationAndDropsCloudCredentials(t *testing.T) {
	home := t.TempDir()
	srcPath := filepath.Join(t.TempDir(), "cloud.db")
	t.Setenv("ITO_HOME", home)

	src := openMigrationTestDatabase(t, srcPath)
	seedMigrationTestDatabase(t, src, "SRC", 3)
	src.Close()
	dst := openMigrationTestDatabase(t, itoconfig.LocalDBPath(home))
	seedMigrationTestDatabase(t, dst, "OLD", 2)
	dst.Close()
	if err := itoconfig.Write(itoconfig.Config{
		Backend: itoconfig.BackendCloud,
		Cloud:   &itoconfig.Cloud{URL: "libsql://example.turso.io", Token: "secret"},
	}); err != nil {
		t.Fatal(err)
	}

	err := migrateLocal(false, sqliteCloudOpener(srcPath))
	if err == nil || !strings.Contains(err.Error(), "rerun with --force") {
		t.Fatalf("expected non-empty local destination error, got %v", err)
	}
	cfg, err := itoconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != itoconfig.BackendCloud || cfg.Cloud == nil {
		t.Fatalf("failed migration changed config: %#v", cfg)
	}

	if err := migrateLocal(true, sqliteCloudOpener(srcPath)); err != nil {
		t.Fatalf("migrate local with force: %v", err)
	}
	cfg, err = itoconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != itoconfig.BackendLocal || cfg.Cloud != nil {
		t.Fatalf("unexpected local config: %#v", cfg)
	}
	configData, err := os.ReadFile(itoconfig.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "cloud") ||
		strings.Contains(string(configData), "libsql://example.turso.io") ||
		strings.Contains(string(configData), "secret") {
		t.Fatalf("local config retained cloud credentials: %s", configData)
	}
	info, err := os.Stat(itoconfig.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
	}

	sourceAfter := openMigrationTestDatabase(t, srcPath)
	localAfter := openMigrationTestDatabase(t, itoconfig.LocalDBPath(home))
	defer sourceAfter.Close()
	defer localAfter.Close()
	for _, table := range migrationTables {
		srcCount, err := tableRowCount(sourceAfter, table.name)
		if err != nil {
			t.Fatal(err)
		}
		localCount, err := tableRowCount(localAfter, table.name)
		if err != nil {
			t.Fatal(err)
		}
		if localCount != srcCount {
			t.Fatalf("%s count = %d, want %d", table.name, localCount, srcCount)
		}
	}
	var oldProjects int
	if err := localAfter.QueryRow(`SELECT count(*) FROM projects WHERE prefix = ?`, "OLD").Scan(&oldProjects); err != nil {
		t.Fatal(err)
	}
	if oldProjects != 0 {
		t.Fatal("forced local migration retained old destination data")
	}
}

func TestMigrateLocalRedactsCloudToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITO_HOME", home)
	if err := itoconfig.Write(itoconfig.Config{
		Backend: itoconfig.BackendCloud,
		Cloud:   &itoconfig.Cloud{URL: "libsql://example.turso.io", Token: "active-secret"},
	}); err != nil {
		t.Fatal(err)
	}

	err := migrateLocal(false, func(string, string) (*sql.DB, error) {
		return nil, fmt.Errorf("connection rejected token active-secret")
	})
	if err == nil || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected redacted cloud error, got %v", err)
	}
	if strings.Contains(err.Error(), "active-secret") {
		t.Fatalf("migration error exposed the token: %v", err)
	}
}

func TestMigrateLocalCommandValidationAndBackendGuard(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	positional := runITO(t, repo, home, "migrate", "local", "extra")
	if positional.exitCode != exitBadUsage || !strings.Contains(positional.stderr, "takes no positional arguments") {
		t.Fatalf("unexpected positional result: %#v", positional)
	}

	alreadyLocal := runITO(t, repo, home, "migrate", "local")
	if alreadyLocal.exitCode != exitGeneric ||
		!strings.Contains(alreadyLocal.stderr, "active backend is already local") ||
		!strings.Contains(alreadyLocal.stderr, "usually requires --force") {
		t.Fatalf("unexpected already-local result: %#v", alreadyLocal)
	}
}

func TestMigrateHelp(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	tests := []struct {
		args []string
		want []string
	}{
		{
			args: []string{"migrate", "--help"},
			want: []string{"usage: ito migrate <command>", "cloud", "local"},
		},
		{
			args: []string{"migrate", "cloud", "--help"},
			want: []string{"usage: ito migrate cloud", "--url", "--token", "--force"},
		},
		{
			args: []string{"migrate", "local", "--help"},
			want: []string{"usage: ito migrate local", "--force", "--json"},
		},
	}
	for _, tt := range tests {
		result := runITO(t, repo, home, tt.args...)
		if result.exitCode != 0 || result.stderr != "" {
			t.Fatalf("%v failed with exit %d\nstdout: %s\nstderr: %s", tt.args, result.exitCode, result.stdout, result.stderr)
		}
		for _, want := range tt.want {
			if !strings.Contains(result.stdout, want) {
				t.Fatalf("%v help does not contain %q\nstdout: %s", tt.args, want, result.stdout)
			}
		}
	}
}

func openMigrationTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := itostore.Migrate(db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func seedMigrationTestDatabase(t *testing.T, db *sql.DB, prefix string, issueCount int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO projects(id, name, root_path, prefix, last_id) VALUES (?, ?, ?, ?, ?)`,
		1, strings.ToLower(prefix), "/tmp/"+strings.ToLower(prefix), prefix, issueCount,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO batches(id, project_id, name, created) VALUES (?, ?, ?, ?)`,
		1, 1, "migration-batch", "2026-07-29T12:00:00Z",
	); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= issueCount; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if _, err := tx.Exec(`
INSERT INTO issues(row_id, project_id, id, title, status, priority, body, created, updated, batch_id, category, triage_state, branch)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, i, 1, id, fmt.Sprintf("Migration issue %d", i), "todo", "medium", "copy body", "2026-07-29T12:00:00Z", "2026-07-29T12:00:00Z", 1, "enhancement", "ready-for-agent", fmt.Sprintf("feat/migration-%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, i, fmt.Sprintf("Migration issue %d", i), "copy body"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label) VALUES (?, ?, ?)`, 1, id, "feature"); err != nil {
			t.Fatal(err)
		}
	}
	if issueCount >= 2 {
		if _, err := tx.Exec(
			`INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, ?, ?, ?)`,
			1, prefix+"-2", prefix+"-1", "blocked_by",
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func sqliteCloudOpener(path string) cloudDatabaseOpener {
	return func(string, string) (*sql.DB, error) {
		return sql.Open("sqlite", path)
	}
}
