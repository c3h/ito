package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/c3h/ito/internal/ledger"
)

// pullPageSize bounds one Ledger read; each Entry is still applied in its own
// transaction, so a connection dropping between pages or entries leaves the
// store at a consistent position to resume from.
const pullPageSize = 500

type SyncResult struct {
	Pushed int `json:"pushed"`
	Pulled int `json:"pulled"`
}

// issueState is the Issue row as it travels inside a Change: the columns of
// the issues table, never the Device-local row_id, project_id or batch_id.
type issueState struct {
	Title       string `json:"title"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	Category    string `json:"category"`
	TriageState string `json:"triage_state"`
	Branch      string `json:"branch"`
	Body        string `json:"body"`
	Created     string `json:"created"`
	Updated     string `json:"updated"`
}

// logIssueChangesTx appends one Change per named Issue, reading each row's
// current state inside the mutating transaction so the log and the row cannot
// disagree.
func logIssueChangesTx(tx *sql.Tx, p Project, ids []string) error {
	for _, id := range ids {
		var state issueState
		if err := tx.QueryRow(`
SELECT title, status, priority, category, triage_state, branch, body, created, updated
FROM issues WHERE project_id = ? AND id = ?`, p.ID, id).Scan(
			&state.Title, &state.Status, &state.Priority, &state.Category, &state.TriageState, &state.Branch, &state.Body, &state.Created, &state.Updated,
		); err != nil {
			return err
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO changes(kind, project, key, deleted, updated, state)
VALUES (?, ?, ?, 0, ?, ?)`, ledger.KindIssue, p.Name, id, state.Updated, string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// logIssueTombstonesTx appends a tombstone per deleted Issue, stamped with the
// deletion time so a deletion outranks any edit that preceded it. Only local
// deletions log; a pulled deletion is applied without echoing a Change back.
func logIssueTombstonesTx(tx *sql.Tx, p Project, rows []issueDeletionRow) error {
	deletedAt := clock().UTC().Format(time.RFC3339)
	for _, row := range rows {
		if _, err := tx.Exec(`
INSERT INTO changes(kind, project, key, deleted, updated, state)
VALUES (?, ?, ?, 1, ?, NULL)`, ledger.KindIssue, p.Name, row.id, deletedAt); err != nil {
			return err
		}
	}
	return nil
}

// Sync pushes this Device's pending Changes to the Ledger, then pulls and
// applies the Changes it has not seen, skipping its own. Rows converge by
// last-writer-wins on updated.
func (s *Store) Sync(l ledger.Ledger) (SyncResult, error) {
	device, err := s.deviceID()
	if err != nil {
		return SyncResult{}, err
	}
	pushed, err := s.push(l, device)
	if err != nil {
		return SyncResult{}, fmt.Errorf("push: %w", err)
	}
	pulled, err := s.pull(l, device)
	if err != nil {
		return SyncResult{Pushed: pushed, Pulled: pulled}, fmt.Errorf("pull: %w", err)
	}
	return SyncResult{Pushed: pushed, Pulled: pulled}, nil
}

func (s *Store) deviceID() (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	device, found, err := syncStateTx(tx, "device")
	if err != nil {
		return "", err
	}
	if found {
		return device, nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	device = hex.EncodeToString(raw[:])
	if err := setSyncStateTx(tx, "device", device); err != nil {
		return "", err
	}
	return device, tx.Commit()
}

func syncStateTx(q rowQuerier, key string) (string, bool, error) {
	var value string
	if err := q.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func setSyncStateTx(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO sync_state(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) push(l ledger.Ledger, device string) (int, error) {
	rows, err := s.db.Query(`SELECT seq, kind, project, key, deleted, updated, state FROM changes WHERE pushed = 0 ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	pending := []ledger.Change{}
	for rows.Next() {
		var change ledger.Change
		var state sql.NullString
		if err := rows.Scan(&change.Sequence, &change.Kind, &change.Project, &change.Key, &change.Deleted, &change.Updated, &state); err != nil {
			rows.Close()
			return 0, err
		}
		change.Device = device
		if state.Valid {
			change.State = json.RawMessage(state.String)
		}
		pending = append(pending, change)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}
	// The Ledger deduplicates by Device and Sequence, so a push whose marking
	// below fails is simply resent next time. Exactly the sent sequences are
	// marked: a writer committing a lower seq after the read above stays
	// pending.
	if _, err := l.Append(pending); err != nil {
		return 0, err
	}
	args := make([]any, 0, len(pending))
	for _, change := range pending {
		args = append(args, change.Sequence)
	}
	if _, err := s.db.Exec(`UPDATE changes SET pushed = 1 WHERE seq IN (`+sqlPlaceholders(len(pending))+`)`, args...); err != nil {
		return 0, err
	}
	return len(pending), nil
}

func (s *Store) pull(l ledger.Ledger, device string) (int, error) {
	position, err := s.lastPosition()
	if err != nil {
		return 0, err
	}
	pulled := 0
	for {
		entries, err := l.ReadAfter(position, pullPageSize)
		if err != nil {
			return pulled, err
		}
		if len(entries) == 0 {
			return pulled, nil
		}
		for _, entry := range entries {
			applied, err := s.applyEntry(entry, device)
			if err != nil {
				return pulled, fmt.Errorf("apply Change at position %d: %w", entry.Position, err)
			}
			if applied {
				pulled++
			}
			position = entry.Position
		}
	}
}

func (s *Store) lastPosition() (int64, error) {
	value, found, err := syncStateTx(s.db, "position")
	if err != nil || !found {
		return 0, err
	}
	var position int64
	if _, err := fmt.Sscan(value, &position); err != nil {
		return 0, fmt.Errorf("sync position %q is malformed", value)
	}
	return position, nil
}

// applyEntry applies one Change and records its position in the same
// transaction, so an interrupted pull never re-applies or skips an Entry.
func (s *Store) applyEntry(entry ledger.Entry, device string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	applied := false
	if entry.Device != device {
		switch entry.Kind {
		case ledger.KindIssue:
			var err error
			if applied, err = applyIssueChangeTx(tx, entry.Change); err != nil {
				return false, err
			}
		default:
			// Failing loudly beats skipping: an older build must not drop rows a
			// newer one pushed.
			return false, fmt.Errorf("row kind %q is unknown to this build of ito; upgrade ito and sync again", entry.Kind)
		}
	}
	if err := setSyncStateTx(tx, "position", fmt.Sprint(entry.Position)); err != nil {
		return false, err
	}
	return applied, tx.Commit()
}

// applyIssueChangeTx applies one Issue Change and reports whether it changed
// the local row.
func applyIssueChangeTx(tx *sql.Tx, change ledger.Change) (bool, error) {
	p, err := ensureProjectTx(tx, change.Project, change.Key)
	if err != nil {
		return false, err
	}
	var rowID int64
	var currentTitle, currentBody, currentUpdated string
	err = tx.QueryRow(`SELECT row_id, title, body, updated FROM issues WHERE project_id = ? AND id = ?`, p.ID, change.Key).
		Scan(&rowID, &currentTitle, &currentBody, &currentUpdated)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// Last-writer-wins: a local row at least as new as the Change stays.
	// RFC3339 stamps in UTC compare correctly as strings.
	if exists && currentUpdated >= change.Updated {
		return false, nil
	}

	if change.Deleted {
		if !exists {
			return false, nil
		}
		_, err := deleteIssueRowsTx(tx, p, []issueDeletionRow{{rowID: rowID, id: change.Key, title: currentTitle, body: currentBody}})
		return err == nil, err
	}

	var state issueState
	if err := json.Unmarshal(change.State, &state); err != nil {
		return false, fmt.Errorf("decode issue state: %w", err)
	}
	if exists {
		if _, err := tx.Exec(`
UPDATE issues
SET title = ?, status = ?, priority = ?, category = ?, triage_state = ?, branch = ?, body = ?, created = ?, updated = ?
WHERE row_id = ?`, state.Title, state.Status, state.Priority, state.Category, state.TriageState, state.Branch, state.Body, state.Created, state.Updated, rowID); err != nil {
			return false, err
		}
		if state.Title != currentTitle || state.Body != currentBody {
			if _, err := tx.Exec(`INSERT INTO issues_fts(issues_fts, rowid, title, body) VALUES ('delete', ?, ?, ?)`, rowID, currentTitle, currentBody); err != nil {
				return false, err
			}
			if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, state.Title, state.Body); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	result, err := tx.Exec(`
INSERT INTO issues(project_id, id, title, status, priority, category, triage_state, branch, body, created, updated)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, change.Key, state.Title, state.Status, state.Priority, state.Category, state.TriageState, state.Branch, state.Body, state.Created, state.Updated)
	if err != nil {
		return false, err
	}
	rowID, err = result.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, state.Title, state.Body); err != nil {
		return false, err
	}
	// Keep the advisory local counter past every number seen, so a Device
	// numbering locally never reuses a pulled ID.
	_, number, err := splitIssueID(change.Key)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(`UPDATE projects SET last_id = max(last_id, ?) WHERE id = ?`, number, p.ID)
	return err == nil, err
}

// ensureProjectTx resolves the Change's Project by name, creating it detached
// (no root_path) when this Device has never seen it; ito init attaches it.
func ensureProjectTx(tx *sql.Tx, name, issueID string) (Project, error) {
	prefix, _, err := splitIssueID(issueID)
	if err != nil {
		return Project{}, err
	}
	p, found, err := findProjectWhere(tx, `SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	if err != nil {
		return Project{}, err
	}
	if found {
		if p.Prefix != prefix {
			return Project{}, fmt.Errorf("project %q uses prefix %s here but %s on the Device that made the Change; rename one of them before syncing", name, p.Prefix, prefix)
		}
		return p, nil
	}
	if taken, err := valueExists(tx, `SELECT 1 FROM projects WHERE prefix = ?`, prefix); err != nil {
		return Project{}, err
	} else if taken {
		return Project{}, fmt.Errorf("prefix %s already belongs to another local project, so project %q cannot be created for it", prefix, name)
	}
	result, err := tx.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, NULL)`, name, prefix)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Prefix: prefix}, nil
}

func splitIssueID(id string) (string, int64, error) {
	match := IssueIDPattern.FindStringSubmatch(id)
	if match == nil {
		return "", 0, fmt.Errorf("issue %q is not a valid ID", id)
	}
	var number int64
	_, err := fmt.Sscan(match[2], &number)
	return match[1], number, err
}
