package ledger

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Every Ledger implementation must satisfy the same contract; the SQL one runs
// against a local SQLite file standing in for Turso.
var implementations = map[string]func(t *testing.T) Ledger{
	"memory": func(t *testing.T) Ledger { return NewMemory() },
	"sql": func(t *testing.T) Ledger {
		l := NewSQL(openSQLite(t))
		if err := l.EnsureSchema(); err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
		return l
	},
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAppendAssignsPositionsAndDeduplicatesResends(t *testing.T) {
	for name, open := range implementations {
		t.Run(name, func(t *testing.T) {
			l := open(t)
			first := []Change{
				{Sequence: 1, Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-1", Updated: "2026-08-24T10:00:00Z", State: json.RawMessage(`{"title":"a"}`)},
				{Sequence: 2, Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-2", Deleted: true, Updated: "2026-08-24T10:00:01Z"},
			}
			positions, err := l.Append(first)
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			if len(positions) != 2 || positions[0] != 1 || positions[1] != 2 {
				t.Fatalf("positions = %v, want [1 2]", positions)
			}

			// A resend of an already appended Change (same Device and Sequence) keeps
			// its original position and does not grow the Ledger.
			positions, err = l.Append([]Change{first[1], {Sequence: 1, Device: "vps", Kind: KindIssue, Project: "ito", Key: "ITO-3", Updated: "2026-08-24T10:00:02Z"}})
			if err != nil {
				t.Fatalf("append resend: %v", err)
			}
			if len(positions) != 2 || positions[0] != 2 || positions[1] != 3 {
				t.Fatalf("positions after resend = %v, want [2 3]", positions)
			}

			entries, err := l.ReadAfter(0, 10)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(entries) != 3 {
				t.Fatalf("expected 3 entries, got %d", len(entries))
			}
			// Entries come back whole: state, tombstone flag and stamps survive.
			if got := entries[0]; got.Position != 1 || got.Device != "mac" || got.Kind != KindIssue || got.Project != "ito" || got.Key != "ITO-1" || got.Deleted || got.Updated != "2026-08-24T10:00:00Z" || string(got.State) != `{"title":"a"}` {
				t.Fatalf("entry 1 = %#v", got)
			}
			if got := entries[1]; !got.Deleted || got.State != nil {
				t.Fatalf("tombstone entry = %#v", got)
			}
			entries, err = l.ReadAfter(2, 10)
			if err != nil {
				t.Fatalf("read after 2: %v", err)
			}
			if len(entries) != 1 || entries[0].Position != 3 || entries[0].Device != "vps" {
				t.Fatalf("entries after 2 = %#v", entries)
			}
			entries, err = l.ReadAfter(0, 2)
			if err != nil {
				t.Fatalf("read limited: %v", err)
			}
			if len(entries) != 2 {
				t.Fatalf("limit must cap the page, got %d entries", len(entries))
			}
			if _, err := l.Append(nil); err != nil {
				t.Fatalf("empty append must be a no-op: %v", err)
			}
		})
	}
}

func TestReserveIssueNumberIsSequentialPerProject(t *testing.T) {
	for name, open := range implementations {
		t.Run(name, func(t *testing.T) {
			l := open(t)
			for want := int64(1); want <= 3; want++ {
				got, err := l.ReserveIssueNumber("ito")
				if err != nil {
					t.Fatalf("reserve: %v", err)
				}
				if got != want {
					t.Fatalf("reserved %d, want %d", got, want)
				}
			}
			got, err := l.ReserveIssueNumber("other")
			if err != nil {
				t.Fatalf("reserve other: %v", err)
			}
			if got != 1 {
				t.Fatalf("other project must count from 1, got %d", got)
			}
		})
	}
}

func TestSQLEnsureSchemaIsIdempotentAndKeepsRows(t *testing.T) {
	db := openSQLite(t)
	l := NewSQL(db)
	if err := l.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append([]Change{{Sequence: 1, Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-1", Updated: "2026-08-24T10:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if err := NewSQL(db).EnsureSchema(); err != nil {
		t.Fatalf("second ensure schema: %v", err)
	}
	entries, err := l.ReadAfter(0, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries after re-ensuring schema = %v, %v", entries, err)
	}
}

// countingDB counts round-trips: on Turso every statement is one. It holds
// the handle rather than embedding it, so the SQL Ledger can only reach the
// three calls it counts.
type countingDB struct {
	db         *sql.DB
	statements int
}

func (c *countingDB) Exec(query string, args ...any) (sql.Result, error) {
	c.statements++
	return c.db.Exec(query, args...)
}

func (c *countingDB) Query(query string, args ...any) (*sql.Rows, error) {
	c.statements++
	return c.db.Query(query, args...)
}

func (c *countingDB) QueryRow(query string, args ...any) *sql.Row {
	c.statements++
	return c.db.QueryRow(query, args...)
}

func TestSQLUsesBoundedStatementsPerOperation(t *testing.T) {
	db := &countingDB{db: openSQLite(t)}
	l := NewSQL(db)
	if err := l.EnsureSchema(); err != nil {
		t.Fatal(err)
	}

	changes := make([]Change, 0, 40)
	for i := range 40 {
		changes = append(changes, Change{Sequence: int64(i + 1), Device: "mac", Kind: KindIssue, Project: "ito", Key: "ITO-1", Updated: "2026-08-24T10:00:00Z"})
	}
	db.statements = 0
	if _, err := l.Append(changes); err != nil {
		t.Fatal(err)
	}
	if db.statements > 2 {
		t.Fatalf("append of 40 Changes took %d statements, want at most 2", db.statements)
	}

	db.statements = 0
	if _, err := l.ReadAfter(0, 500); err != nil {
		t.Fatal(err)
	}
	if db.statements != 1 {
		t.Fatalf("read took %d statements, want 1", db.statements)
	}

	db.statements = 0
	if _, err := l.ReserveIssueNumber("ito"); err != nil {
		t.Fatal(err)
	}
	if db.statements != 1 {
		t.Fatalf("reserve took %d statements, want 1", db.statements)
	}
}
