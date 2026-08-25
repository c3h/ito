package store

import (
	"database/sql"
	"strings"

	"github.com/c3h/ito/internal/ledger"
)

// Snapshot completes the change log once per store: every current Project,
// Batch, Issue, Label and Link gets a Change at the tail, parents before the
// rows that reference them, so pushing the log hands a Ledger the whole
// store in an order it can apply. Rows that predate the log (migrateV5 never
// backfilled) are the reason; rows already logged are logged again rather
// than skipped, because an earlier Change may sit ahead of a parent the
// snapshot only logs now. A sync_state mark makes later calls no-ops, which
// is what keeps a retried connect from duplicating Changes. Returns how many
// Changes were appended.
func (s *Store) Snapshot() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, done, err := syncStateTx(tx, "snapshot"); err != nil || done {
		return 0, err
	}
	projects, err := listProjects(tx)
	if err != nil {
		return 0, err
	}
	logged := 0
	for _, p := range projects {
		// Parents first: the Project owns the Batches, which the Issues
		// reference, which the Labels and Links hang off.
		if err := logProjectChangeTx(tx, p.Name, p.Prefix); err != nil {
			return 0, err
		}
		logged++
		for _, log := range snapshotLoggers {
			n, err := log(tx, p)
			if err != nil {
				return 0, err
			}
			logged += n
		}
	}
	if err := setSyncStateTx(tx, "snapshot", "done"); err != nil {
		return 0, err
	}
	return logged, tx.Commit()
}

// snapshotLoggers run in order per Project, parents before the rows that
// reference them.
var snapshotLoggers = []func(*sql.Tx, Project) (int, error){logAllBatchesTx, logAllIssuesTx, logAllLabelsTx, logAllLinksTx}

func logAllBatchesTx(tx *sql.Tx, p Project) (int, error) {
	logged := 0
	err := forEachRow(tx, `SELECT name, created FROM batches WHERE project_id = ? ORDER BY id`, []any{p.ID}, func(rows *sql.Rows) error {
		var name, created string
		if err := rows.Scan(&name, &created); err != nil {
			return err
		}
		logged++
		return logBatchChangeTx(tx, p, name, batchState{Created: created})
	})
	return logged, err
}

func logAllIssuesTx(tx *sql.Tx, p Project) (int, error) {
	ids, err := stringColumn(tx, `SELECT id FROM issues WHERE project_id = ? ORDER BY row_id`, p.ID)
	if err != nil {
		return 0, err
	}
	return len(ids), logIssueChangesTx(tx, p, ids)
}

// Legacy Label and Link rows carry the empty stamp; the owning Issue's
// updated stands in so the Change ranks like the row it belongs to.
func logAllLabelsTx(tx *sql.Tx, p Project) (int, error) {
	logged := 0
	err := forEachRow(tx, `
SELECT l.issue_id, l.label, CASE WHEN l.updated = '' THEN i.updated ELSE l.updated END
FROM issue_labels l JOIN issues i ON i.project_id = l.project_id AND i.id = l.issue_id
WHERE l.project_id = ? ORDER BY l.issue_id, l.label`, []any{p.ID}, func(rows *sql.Rows) error {
		var issueID, label, updated string
		if err := rows.Scan(&issueID, &label, &updated); err != nil {
			return err
		}
		logged++
		return logLabelChangeTx(tx, p, issueID, label, false, updated)
	})
	return logged, err
}

func logAllLinksTx(tx *sql.Tx, p Project) (int, error) {
	logged := 0
	err := forEachRow(tx, `
SELECT k.source_id, k.target_id, k.kind, CASE WHEN k.updated = '' THEN i.updated ELSE k.updated END
FROM issue_links k JOIN issues i ON i.project_id = k.project_id AND i.id = k.source_id
WHERE k.project_id = ? ORDER BY k.source_id, k.target_id, k.kind`, []any{p.ID}, func(rows *sql.Rows) error {
		var sourceID, targetID, kind, updated string
		if err := rows.Scan(&sourceID, &targetID, &kind, &updated); err != nil {
			return err
		}
		logged++
		return logLinkChangeTx(tx, p, issueLinkKey(sourceID, targetID, kind), false, updated)
	})
	return logged, err
}

// RowCounts reports how many live rows the store holds per Change kind — the
// figure a Ledger's Inventory must match after a snapshot. Kinds with no rows
// are absent; the comparison reads the map by kind, so that is a detail.
func (s *Store) RowCounts() (map[string]int, error) {
	tables := []struct{ kind, table string }{
		{ledger.KindProject, "projects"},
		{ledger.KindBatch, "batches"},
		{ledger.KindIssue, "issues"},
		{ledger.KindLabel, "issue_labels"},
		{ledger.KindLink, "issue_links"},
	}
	selects := make([]string, len(tables))
	targets := make([]any, len(tables))
	found := make([]int, len(tables))
	for i, t := range tables {
		selects[i] = `(SELECT count(*) FROM ` + t.table + `)`
		targets[i] = &found[i]
	}
	if err := s.db.QueryRow(`SELECT ` + strings.Join(selects, ", ")).Scan(targets...); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(tables))
	for i, t := range tables {
		if found[i] > 0 {
			counts[t.kind] = found[i]
		}
	}
	return counts, nil
}
