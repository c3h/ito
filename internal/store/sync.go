package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/c3h/ito/internal/ledger"
)

// pullPageSize bounds one Ledger read; each Entry is still applied in its own
// transaction, so a connection dropping between pages or entries leaves the
// store at a consistent position to resume from.
const pullPageSize = 500

// markPageSize bounds one "mark as pushed" statement, so a snapshot-sized
// push stays under SQLite's bind-parameter limit.
const markPageSize = 500

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
	Assignee    string `json:"assignee"`
	Body        string `json:"body"`
	// Batch is the member Batch's name, empty when the Issue sits in none.
	Batch   string `json:"batch,omitempty"`
	Created string `json:"created"`
	Updated string `json:"updated"`
}

// projectState travels only the Prefix: root_path and last_id are Device-local.
type projectState struct {
	Prefix string `json:"prefix"`
}

// batchState carries the immutable created stamp; on a rename, RenamedFrom
// names the Batch the receiving Device should re-label so memberships stay put.
type batchState struct {
	// Prefix identifies the Project across Devices, whatever it is named there.
	Prefix      string `json:"prefix"`
	Created     string `json:"created"`
	RenamedFrom string `json:"renamed_from,omitempty"`
}

func appendChangeTx(tx *sql.Tx, kind, project, key string, deleted bool, updated string, state any) error {
	var encoded any
	if state != nil {
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		encoded = string(raw)
	}
	_, err := tx.Exec(`
INSERT INTO changes(kind, project, key, deleted, updated, state)
VALUES (?, ?, ?, ?, ?, ?)`, kind, project, key, deleted, updated, encoded)
	return err
}

func logProjectChangeTx(tx *sql.Tx, name, prefix string) error {
	return appendChangeTx(tx, ledger.KindProject, name, name, false, nowStamp(), projectState{Prefix: prefix})
}

func logBatchChangeTx(tx *sql.Tx, p Project, name string, state batchState) error {
	state.Prefix = p.Prefix
	return appendChangeTx(tx, ledger.KindBatch, p.Name, name, false, nowStamp(), state)
}

func logBatchTombstoneTx(tx *sql.Tx, p Project, name, deletedAt string) error {
	return appendChangeTx(tx, ledger.KindBatch, p.Name, name, true, deletedAt, batchState{Prefix: p.Prefix})
}

// Label and Link keys are the rows themselves; the Change carries no state.
func logLabelChangeTx(tx *sql.Tx, p Project, issueID, label string, deleted bool, updated string) error {
	return appendChangeTx(tx, ledger.KindLabel, p.Name, labelChangeKey(issueID, label), deleted, updated, nil)
}

func logLinkChangeTx(tx *sql.Tx, p Project, linkKey string, deleted bool, updated string) error {
	return appendChangeTx(tx, ledger.KindLink, p.Name, linkKey, deleted, updated, nil)
}

func labelChangeKey(issueID, label string) string {
	return issueID + "|" + label
}

// tombstoneSetRowTx remembers when a Label or Link row was removed, so an
// older insert arriving later does not resurrect it.
func tombstoneSetRowTx(tx *sql.Tx, kind string, projectID int64, key, updated string) error {
	_, err := tx.Exec(`
INSERT INTO set_tombstones(kind, project_id, key, updated) VALUES (?, ?, ?, ?)
ON CONFLICT(kind, project_id, key) DO UPDATE SET updated = max(updated, excluded.updated)`, kind, projectID, key, updated)
	return err
}

// setRelation describes one of the set-like relations a Change can carry:
// the table it lives in and the columns its key splits into, in key order.
// issueColumns indexes the columns that name an Issue both ends of an insert
// need; the first also identifies the owning Project.
type setRelation struct {
	kind         string
	table        string
	columns      []string
	issueColumns []int
}

var (
	labelRelation = setRelation{ledger.KindLabel, "issue_labels", []string{"issue_id", "label"}, []int{0}}
	linkRelation  = setRelation{ledger.KindLink, "issue_links", []string{"source_id", "target_id", "kind"}, []int{0, 1}}
)

// rowWhere matches the single row a Change key names, within its Project.
func (r setRelation) rowWhere() string {
	clauses := make([]string, 0, len(r.columns)+1)
	clauses = append(clauses, "project_id = ?")
	for _, column := range r.columns {
		clauses = append(clauses, column+" = ?")
	}
	return strings.Join(clauses, " AND ")
}

func (r setRelation) upsert() string {
	columns := strings.Join(r.columns, ", ")
	return `INSERT INTO ` + r.table + `(project_id, ` + columns + `, updated) VALUES (` + sqlPlaceholders(len(r.columns)+2) + `)
ON CONFLICT(project_id, ` + columns + `) DO UPDATE SET updated = excluded.updated`
}

// setRowOutranked reports whether a local row or tombstone for the key is at
// least as new as the Change, in which case the Change is stale.
func setRowOutranked(tx *sql.Tx, r setRelation, projectID int64, key string, rowArgs []any, updated string) (bool, error) {
	var current string
	err := tx.QueryRow(`SELECT updated FROM `+r.table+` WHERE `+r.rowWhere(), rowArgs...).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && current >= updated {
		return true, nil
	}
	err = tx.QueryRow(`SELECT updated FROM set_tombstones WHERE kind = ? AND project_id = ? AND key = ?`, r.kind, projectID, key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return err == nil && current >= updated, nil
}

func splitChangeKey(key string, parts int) ([]string, error) {
	fields := strings.Split(key, "|")
	if len(fields) != parts {
		return nil, fmt.Errorf("change key %q is malformed", key)
	}
	return fields, nil
}

// logIssueChangesTx appends one Change per named Issue, reading each row's
// current state inside the mutating transaction so the log and the row cannot
// disagree.
func logIssueChangesTx(tx *sql.Tx, p Project, ids []string) error {
	for _, id := range ids {
		var state issueState
		var batch sql.NullString
		if err := tx.QueryRow(`
SELECT issues.title, issues.status, issues.priority, issues.category, issues.triage_state, issues.branch, issues.assignee, issues.body, batches.name, issues.created, issues.updated
FROM issues
LEFT JOIN batches ON batches.id = issues.batch_id
WHERE issues.project_id = ? AND issues.id = ?`, p.ID, id).Scan(
			&state.Title, &state.Status, &state.Priority, &state.Category, &state.TriageState, &state.Branch, &state.Assignee, &state.Body, &batch, &state.Created, &state.Updated,
		); err != nil {
			return err
		}
		state.Batch = batch.String
		if err := appendChangeTx(tx, ledger.KindIssue, p.Name, id, false, state.Updated, state); err != nil {
			return err
		}
	}
	return nil
}

// logIssueTombstonesTx appends a tombstone per deleted Issue, stamped with the
// deletion time so a deletion outranks any edit that preceded it. Only local
// deletions log; a pulled deletion is applied without echoing a Change back.
func logIssueTombstonesTx(tx *sql.Tx, p Project, ids []string) error {
	deletedAt := nowStamp()
	for _, id := range ids {
		if err := appendChangeTx(tx, ledger.KindIssue, p.Name, id, true, deletedAt, nil); err != nil {
			return err
		}
	}
	return nil
}

// Sync pushes this Device's pending Changes to the Ledger, then pulls and
// applies the Changes it has not seen, skipping its own. Rows converge by
// last-writer-wins on updated.
func (s *Store) Sync(l ledger.Ledger) (SyncResult, error) {
	device, err := s.DeviceID()
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

// Push sends the pending Changes to the Ledger without pulling; a writing
// command calls it right after committing so the other Devices see the
// mutation on their next Sync. Whatever fails stays pending for the next push.
func (s *Store) Push(l ledger.Ledger) (int, error) {
	device, err := s.DeviceID()
	if err != nil {
		return 0, err
	}
	return s.push(l, device)
}

// DeviceID returns this Device's stable identity, generating it on first use.
func (s *Store) DeviceID() (string, error) {
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

// Empty reports whether the store holds no work yet — no Issues and no
// Batches. Projects alone do not count: "ito init" on a fresh machine must
// still leave it free to pull a Ledger.
func (s *Store) Empty() (bool, error) {
	var populated bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM issues) OR EXISTS(SELECT 1 FROM batches)`).Scan(&populated)
	return !populated, err
}

// ResetSync forgets everything the store knows about a Ledger: the last
// applied position, so the next pull starts from the beginning, and the
// pushed marks, so every local Change is offered again. A Ledger that already
// holds a Change keeps its position, so resending to the same Ledger is
// harmless and resending to a new one is what makes the local history travel.
func (s *Store) ResetSync() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sync_state WHERE key = 'position'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE changes SET pushed = 0 WHERE pushed = 1`); err != nil {
		return err
	}
	return tx.Commit()
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
	// Marked in the same bounded chunks the Ledger is appended in: a whole
	// snapshot binds one parameter per Change, well past SQLite's limit.
	for start := 0; start < len(pending); start += markPageSize {
		chunk := pending[start:min(start+markPageSize, len(pending))]
		args := make([]any, len(chunk))
		for i, change := range chunk {
			args[i] = change.Sequence
		}
		if _, err := s.db.Exec(`UPDATE changes SET pushed = 1 WHERE seq IN (`+sqlPlaceholders(len(chunk))+`)`, args...); err != nil {
			return 0, err
		}
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
	position, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
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
		var err error
		switch entry.Kind {
		case ledger.KindIssue:
			applied, err = applyIssueChangeTx(tx, entry.Change)
		case ledger.KindProject:
			applied, err = applyProjectChangeTx(tx, entry.Change)
		case ledger.KindBatch:
			applied, err = applyBatchChangeTx(tx, entry.Change)
		case ledger.KindLabel:
			applied, err = applySetRowChangeTx(tx, labelRelation, entry.Change)
		case ledger.KindLink:
			applied, err = applySetRowChangeTx(tx, linkRelation, entry.Change)
		default:
			// Failing loudly beats skipping: an older build must not drop rows a
			// newer one pushed.
			return false, fmt.Errorf("row kind %q is unknown to this build of ito; upgrade ito and sync again", entry.Kind)
		}
		if err != nil {
			return false, err
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
	// A Batch this Device does not hold (deleted or renamed here since the
	// Change was made) leaves the Issue unassigned rather than failing the pull.
	var batchID sql.NullInt64
	if state.Batch != "" {
		id, found, err := findBatchID(tx, p.ID, state.Batch)
		if err != nil {
			return false, err
		}
		batchID = sql.NullInt64{Int64: id, Valid: found}
	}
	if exists {
		if _, err := tx.Exec(`
UPDATE issues
SET title = ?, status = ?, priority = ?, category = ?, triage_state = ?, branch = ?, assignee = ?, body = ?, batch_id = ?, created = ?, updated = ?
WHERE row_id = ?`, state.Title, state.Status, state.Priority, state.Category, state.TriageState, state.Branch, state.Assignee, state.Body, batchID, state.Created, state.Updated, rowID); err != nil {
			return false, err
		}
		if err := reindexIssueFTSTx(tx, rowID, currentTitle, currentBody, state.Title, state.Body); err != nil {
			return false, err
		}
		return true, nil
	}

	result, err := tx.Exec(`
INSERT INTO issues(project_id, id, title, status, priority, category, triage_state, branch, assignee, body, batch_id, created, updated)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, change.Key, state.Title, state.Status, state.Priority, state.Category, state.TriageState, state.Branch, state.Assignee, state.Body, batchID, state.Created, state.Updated)
	if err != nil {
		return false, err
	}
	rowID, err = result.LastInsertId()
	if err != nil {
		return false, err
	}
	if err := insertIssueFTSTx(tx, rowID, state.Title, state.Body); err != nil {
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

// ensureProjectTx resolves the Change's Project, creating it detached (no
// root_path) when this Device has never seen it; ito init attaches it. The
// Prefix is the durable identity across Devices — a name the Change still
// carries may have been renamed here since — so it is matched first.
func ensureProjectTx(tx *sql.Tx, name, issueID string) (Project, error) {
	prefix, _, err := splitIssueID(issueID)
	if err != nil {
		return Project{}, err
	}
	return ensureProjectByPrefixTx(tx, name, prefix)
}

func ensureProjectByPrefixTx(tx *sql.Tx, name, prefix string) (Project, error) {
	p, found, err := findProjectByPrefix(tx, prefix)
	if err != nil {
		return Project{}, err
	}
	if found {
		return p, nil
	}
	p, found, err = findProjectByName(tx, name)
	if err != nil {
		return Project{}, err
	}
	if found {
		return Project{}, fmt.Errorf("project %q uses prefix %s here but %s on the Device that made the Change; rename one of them before syncing", name, p.Prefix, prefix)
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
	number, err := strconv.ParseInt(match[2], 10, 64)
	return match[1], number, err
}

// applyProjectChangeTx creates or renames the Project by Prefix; root_path
// and last_id stay whatever this Device holds.
func applyProjectChangeTx(tx *sql.Tx, change ledger.Change) (bool, error) {
	if change.Deleted {
		return false, fmt.Errorf("project %q: projects are never deleted through sync", change.Key)
	}
	var state projectState
	if err := json.Unmarshal(change.State, &state); err != nil {
		return false, fmt.Errorf("decode project state: %w", err)
	}
	p, found, err := findProjectByPrefix(tx, state.Prefix)
	if err != nil {
		return false, err
	}
	if !found {
		_, err := ensureProjectByPrefixTx(tx, change.Key, state.Prefix)
		return err == nil, err
	}
	if p.Name == change.Key {
		return false, nil
	}
	if taken, err := projectNameExistsForAnotherID(tx, change.Key, p.ID); err != nil {
		return false, err
	} else if taken {
		return false, fmt.Errorf("project %q (prefix %s) was renamed to %q on another Device, but that name belongs to another local project; rename one of them before syncing", p.Name, p.Prefix, change.Key)
	}
	_, err = tx.Exec(`UPDATE projects SET name = ? WHERE id = ?`, change.Key, p.ID)
	return err == nil, err
}

// applyBatchChangeTx creates, renames or deletes the Batch named by the key.
// A rename re-labels the local row so its memberships stay attached; when the
// new name already exists locally the two Batches merge into it. Batch and
// Project Changes apply in Ledger order rather than by updated: their rows
// are rarely touched on two Devices between Syncs, and a stale rename is
// harmless where a stale row state would not be.
func applyBatchChangeTx(tx *sql.Tx, change ledger.Change) (bool, error) {
	var state batchState
	if err := json.Unmarshal(change.State, &state); err != nil {
		return false, fmt.Errorf("decode batch state: %w", err)
	}
	p, err := ensureProjectByPrefixTx(tx, change.Project, state.Prefix)
	if err != nil {
		return false, err
	}
	id, exists, err := findBatchID(tx, p.ID, change.Key)
	if err != nil {
		return false, err
	}
	if change.Deleted {
		if !exists {
			return false, nil
		}
		if _, err := tx.Exec(`UPDATE issues SET batch_id = NULL WHERE batch_id = ?`, id); err != nil {
			return false, err
		}
		_, err := tx.Exec(`DELETE FROM batches WHERE id = ?`, id)
		return err == nil, err
	}
	if state.RenamedFrom != "" {
		oldID, oldExists, err := findBatchID(tx, p.ID, state.RenamedFrom)
		if err != nil {
			return false, err
		}
		if oldExists && oldID != id {
			if !exists {
				_, err := tx.Exec(`UPDATE batches SET name = ? WHERE id = ?`, change.Key, oldID)
				return err == nil, err
			}
			if _, err := tx.Exec(`UPDATE issues SET batch_id = ? WHERE batch_id = ?`, id, oldID); err != nil {
				return false, err
			}
			_, err := tx.Exec(`DELETE FROM batches WHERE id = ?`, oldID)
			return err == nil, err
		}
	}
	if exists {
		return false, nil
	}
	_, err = tx.Exec(`INSERT INTO batches(project_id, name, created) VALUES (?, ?, ?)`, p.ID, change.Key, state.Created)
	return err == nil, err
}

// applySetRowChangeTx inserts or deletes one Label or Link row by
// last-writer-wins on the row's own updated, remembering deletions as
// tombstones. The Issue's own Change precedes it in the Ledger; an Issue
// since deleted here has nothing to label or link, and a removal for a
// Project unknown here has nothing to remove.
func applySetRowChangeTx(tx *sql.Tx, r setRelation, change ledger.Change) (bool, error) {
	fields, err := splitChangeKey(change.Key, len(r.columns))
	if err != nil {
		return false, err
	}
	p, found, err := findProjectByIssueIDTx(tx, fields[r.issueColumns[0]])
	if err != nil || !found {
		return false, err
	}
	rowArgs := make([]any, 0, len(fields)+2)
	rowArgs = append(rowArgs, p.ID)
	for _, field := range fields {
		rowArgs = append(rowArgs, field)
	}
	outranked, err := setRowOutranked(tx, r, p.ID, change.Key, rowArgs, change.Updated)
	if err != nil || outranked {
		return false, err
	}
	if change.Deleted {
		if err := tombstoneSetRowTx(tx, r.kind, p.ID, change.Key, change.Updated); err != nil {
			return false, err
		}
		return execChanged(tx, `DELETE FROM `+r.table+` WHERE `+r.rowWhere(), rowArgs...)
	}
	for _, column := range r.issueColumns {
		if exists, err := issueExistsTx(tx, p.ID, fields[column]); err != nil || !exists {
			return false, err
		}
	}
	return execChanged(tx, r.upsert(), append(rowArgs, change.Updated)...)
}

func execChanged(tx *sql.Tx, query string, args ...any) (bool, error) {
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
