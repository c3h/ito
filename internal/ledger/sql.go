package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// DB is the slice of database/sql the SQL Ledger needs; *sql.DB satisfies it.
type DB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// appendChunkSize bounds one INSERT: eight parameters per row keeps a chunk
// far below SQLite's parameter limit.
const appendChunkSize = 500

// SQL is the Ledger as a pair of tables on any database/sql handle: Turso in
// production, a local SQLite file in tests. The remote round-trip is the cost
// that matters, so every call is a fixed number of statements per page of
// work and nothing here opens a transaction.
type SQL struct {
	db DB
}

// NewSQL wraps an open handle; call EnsureSchema once before using it.
func NewSQL(db DB) *SQL {
	return &SQL{db: db}
}

// OpenTurso dials a Turso Ledger with the token as credential. Nothing is sent
// until the first statement, and the token never appears in an error.
func OpenTurso(address, token string) (*SQL, error) {
	dsn := address + "?" + url.Values{"authToken": {token}}.Encode()
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open the Ledger at %s", address)
	}
	return NewSQL(db), nil
}

// EnsureSchema creates the Ledger's tables when they do not exist yet; it
// runs once, when a Device connects. The position is a plain rowid rather
// than AUTOINCREMENT: the Ledger never deletes, so rowids stay monotonic, and
// AUTOINCREMENT would burn a number on every resend the insert ignores.
func (s *SQL) EnsureSchema() error {
	statements := []string{`
CREATE TABLE IF NOT EXISTS changes (
  position INTEGER PRIMARY KEY,
  device   TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  kind     TEXT NOT NULL,
  project  TEXT NOT NULL,
  key      TEXT NOT NULL,
  deleted  INTEGER NOT NULL DEFAULT 0,
  updated  TEXT NOT NULL,
  state    TEXT,
  UNIQUE (device, sequence)
)`, `
CREATE TABLE IF NOT EXISTS counters (
  project TEXT PRIMARY KEY, -- the Project's Prefix
  last    INTEGER NOT NULL DEFAULT 0
)`}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// Append inserts each chunk with one statement, ignoring Changes the Ledger
// already holds, then reads the positions back with a second one.
func (s *SQL) Append(changes []Change) ([]int64, error) {
	positions := make([]int64, 0, len(changes))
	for start := 0; start < len(changes); start += appendChunkSize {
		chunk := changes[start:min(start+appendChunkSize, len(changes))]
		chunkPositions, err := s.appendChunk(chunk)
		if err != nil {
			return nil, err
		}
		positions = append(positions, chunkPositions...)
	}
	return positions, nil
}

func (s *SQL) appendChunk(chunk []Change) ([]int64, error) {
	var insert strings.Builder
	insert.WriteString(`INSERT INTO changes(device, sequence, kind, project, key, deleted, updated, state) VALUES `)
	insertArgs := make([]any, 0, 8*len(chunk))
	var lookup strings.Builder
	lookup.WriteString(`SELECT device, sequence, position FROM changes WHERE (device, sequence) IN (`)
	lookupArgs := make([]any, 0, 2*len(chunk))
	for i, change := range chunk {
		if i > 0 {
			insert.WriteString(", ")
			lookup.WriteString(", ")
		}
		insert.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
		var state any
		if change.State != nil {
			state = string(change.State)
		}
		insertArgs = append(insertArgs, change.Device, change.Sequence, change.Kind, change.Project, change.Key, change.Deleted, change.Updated, state)
		lookup.WriteString("(?, ?)")
		lookupArgs = append(lookupArgs, change.Device, change.Sequence)
	}
	insert.WriteString(` ON CONFLICT(device, sequence) DO NOTHING`)
	lookup.WriteString(`)`)

	if _, err := s.db.Exec(insert.String(), insertArgs...); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(lookup.String(), lookupArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assigned := make(map[deviceSequence]int64, len(chunk))
	for rows.Next() {
		var key deviceSequence
		var position int64
		if err := rows.Scan(&key.device, &key.sequence, &position); err != nil {
			return nil, err
		}
		assigned[key] = position
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	positions := make([]int64, 0, len(chunk))
	for _, change := range chunk {
		position, ok := assigned[deviceSequence{change.Device, change.Sequence}]
		if !ok {
			return nil, fmt.Errorf("the Ledger did not record Change %d of Device %s", change.Sequence, change.Device)
		}
		positions = append(positions, position)
	}
	return positions, nil
}

// ReadAfter is one statement per page.
func (s *SQL) ReadAfter(position int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.Query(`
SELECT position, device, sequence, kind, project, key, deleted, updated, state
FROM changes WHERE position > ? ORDER BY position LIMIT ?`, position, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var entry Entry
		var state sql.NullString
		if err := rows.Scan(&entry.Position, &entry.Device, &entry.Sequence, &entry.Kind, &entry.Project, &entry.Key, &entry.Deleted, &entry.Updated, &state); err != nil {
			return nil, err
		}
		if state.Valid {
			entry.State = []byte(state.String)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ReserveIssueNumber is one statement: an upsert that returns the new count.
func (s *SQL) ReserveIssueNumber(prefix string, floor int64) (int64, error) {
	var number int64
	err := s.db.QueryRow(`
INSERT INTO counters(project, last) VALUES (?, ?)
ON CONFLICT(project) DO UPDATE SET last = max(last, excluded.last - 1) + 1
RETURNING last`, prefix, floor+1).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("the Ledger did not hand out an Issue number")
	}
	return number, err
}
