package main

import (
	"database/sql"
	"fmt"
	"strings"

	itoconfig "github.com/c3h/ito/internal/config"
	itostore "github.com/c3h/ito/internal/store"
)

const migrationBatchSize = 200

type migrationTable struct {
	name    string
	columns []string
	orderBy string
}

var migrationTables = []migrationTable{
	{name: "projects", columns: []string{"id", "name", "root_path", "prefix", "last_id"}, orderBy: "id"},
	{name: "batches", columns: []string{"id", "project_id", "name", "created"}, orderBy: "id"},
	{name: "issues", columns: []string{"row_id", "project_id", "id", "title", "status", "priority", "body", "created", "updated", "batch_id", "category", "triage_state"}, orderBy: "row_id"},
	{name: "issue_links", columns: []string{"project_id", "source_id", "target_id", "kind"}, orderBy: "project_id, source_id, target_id, kind"},
	{name: "issue_labels", columns: []string{"project_id", "issue_id", "label"}, orderBy: "project_id, issue_id, label"},
}

type cloudDatabaseOpener func(url, token string) (*sql.DB, error)

func openCloudDatabase(url, token string) (*sql.DB, error) {
	return sql.Open("libsql", itostore.CloudDSN(url, token))
}

func migrateCloud(url, token string, force bool, openCloud cloudDatabaseOpener) error {
	cfg, err := itoconfig.Load()
	if err != nil {
		return fmt.Errorf("could not read the config: %w", err)
	}
	if cfg.Backend == itoconfig.BackendCloud {
		return fmt.Errorf("the active backend is already cloud; run 'ito migrate local' first")
	}

	home, err := itoconfig.HomeDir()
	if err != nil {
		return fmt.Errorf("could not resolve the ito home: %w", err)
	}
	src, err := sql.Open("sqlite", "file:"+itoconfig.LocalDBPath(home)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("could not open the local source database: %w", err)
	}
	defer src.Close()

	dst, err := openCloud(url, token)
	if err != nil {
		return fmt.Errorf("could not open the cloud destination: %s", redactSecret(err.Error(), token))
	}
	defer dst.Close()

	if err := itostore.Migrate(dst); err != nil {
		return fmt.Errorf("could not initialize the cloud destination: %s", redactSecret(err.Error(), token))
	}
	// Best-effort cache pre-warm; a miss only costs one redundant Migrate later.
	itostore.MarkSchemaCurrent(home, url)
	if err := copyDatabase(src, dst, force); err != nil {
		return fmt.Errorf("could not copy the local database to cloud: %s", redactSecret(err.Error(), token))
	}
	if err := itoconfig.Write(itoconfig.Config{
		Backend: itoconfig.BackendCloud,
		Cloud:   &itoconfig.Cloud{URL: url, Token: token},
	}); err != nil {
		return fmt.Errorf("could not activate the cloud backend: %w", err)
	}
	return nil
}

func migrateLocal(force bool, openCloud cloudDatabaseOpener) error {
	cfg, err := itoconfig.Load()
	if err != nil {
		return fmt.Errorf("could not read the config: %w", err)
	}
	if cfg.Backend == itoconfig.BackendLocal {
		return fmt.Errorf("the active backend is already local; run 'ito migrate cloud --url <url> --token <token>' first")
	}

	home, err := itoconfig.HomeDir()
	if err != nil {
		return fmt.Errorf("could not resolve the ito home: %w", err)
	}
	src, err := openCloud(cfg.Cloud.URL, cfg.Cloud.Token)
	if err != nil {
		return fmt.Errorf("could not open the cloud source: %s", redactSecret(err.Error(), cfg.Cloud.Token))
	}
	defer src.Close()

	dst, err := itostore.Open(home)
	if err != nil {
		return fmt.Errorf("could not open the local destination: %s", redactSecret(err.Error(), cfg.Cloud.Token))
	}
	defer dst.Close()

	if err := copyDatabase(src, dst, force); err != nil {
		return fmt.Errorf("could not copy the cloud database to local: %s", redactSecret(err.Error(), cfg.Cloud.Token))
	}
	if err := itoconfig.Write(itoconfig.Config{Backend: itoconfig.BackendLocal}); err != nil {
		return fmt.Errorf("could not activate the local backend: %w", err)
	}
	return nil
}

func copyDatabase(src, dst *sql.DB, force bool) error {
	if err := ensureDestinationReady(dst, force); err != nil {
		return err
	}
	copied := make(map[string]int64, len(migrationTables))
	for _, table := range migrationTables {
		count, err := copyTable(src, dst, table)
		if err != nil {
			return err
		}
		copied[table.name] = count
	}
	for _, table := range migrationTables {
		dstCount, err := tableRowCount(dst, table.name)
		if err != nil {
			return fmt.Errorf("could not count destination table %q: %w", table.name, err)
		}
		if copied[table.name] != dstCount {
			return fmt.Errorf("row count mismatch for table %q: source has %d rows and destination has %d", table.name, copied[table.name], dstCount)
		}
	}
	if _, err := dst.Exec(`INSERT INTO issues_fts(issues_fts) VALUES (?)`, "rebuild"); err != nil {
		return fmt.Errorf("could not rebuild the destination search index: %w", err)
	}
	return nil
}

func ensureDestinationReady(dst *sql.DB, force bool) error {
	if force {
		for i := len(migrationTables) - 1; i >= 0; i-- {
			table := migrationTables[i]
			if _, err := dst.Exec(`DELETE FROM ` + table.name); err != nil {
				return fmt.Errorf("could not clear destination table %q: %w", table.name, err)
			}
		}
		return nil
	}
	for _, table := range migrationTables {
		count, err := tableRowCount(dst, table.name)
		if err != nil {
			return fmt.Errorf("could not inspect destination table %q: %w", table.name, err)
		}
		if count > 0 {
			return fmt.Errorf("destination table %q contains %d rows; rerun with --force to replace the destination", table.name, count)
		}
	}
	return nil
}

// copyTable streams the source table in one query, inserting client-side
// batches as it scans (OFFSET pagination would re-scan copied rows each page).
// It returns the number of rows copied.
func copyTable(src, dst *sql.DB, table migrationTable) (int64, error) {
	query := fmt.Sprintf(
		"SELECT %s FROM %s ORDER BY %s",
		strings.Join(table.columns, ", "),
		table.name,
		table.orderBy,
	)
	rows, err := src.Query(query)
	if err != nil {
		return 0, fmt.Errorf("could not read source table %q: %w", table.name, err)
	}
	defer rows.Close()

	var copied int64
	batch := make([][]any, 0, migrationBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertMigrationBatch(dst, table, batch); err != nil {
			return fmt.Errorf("could not write destination table %q: %w", table.name, err)
		}
		copied += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		values := make([]any, len(table.columns))
		destinations := make([]any, len(table.columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return 0, fmt.Errorf("could not read source table %q: %w", table.name, err)
		}
		// Copy []byte values: the driver may reuse the buffer on the next scan.
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), bytes...)
			}
		}
		batch = append(batch, values)
		if len(batch) == migrationBatchSize {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("could not read source table %q: %w", table.name, err)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return copied, nil
}

func insertMigrationBatch(dst *sql.DB, table migrationTable, batch [][]any) error {
	rowPlaceholders := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(table.columns)), ", ") + ")"
	args := make([]any, 0, len(batch)*len(table.columns))
	for _, row := range batch {
		args = append(args, row...)
	}
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES %s",
		table.name,
		strings.Join(table.columns, ", "),
		strings.TrimSuffix(strings.Repeat(rowPlaceholders+", ", len(batch)), ", "),
	)
	_, err := dst.Exec(query, args...)
	return err
}

func tableRowCount(db *sql.DB, table string) (int64, error) {
	var count int64
	err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count)
	return count, err
}

func redactSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[redacted]")
}
