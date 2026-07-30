package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

type statementCounter struct {
	value atomic.Int64
}

type countingDriver struct {
	inner   driver.Driver
	counter *statementCounter
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, counter: d.counter}, nil
}

type countingConn struct {
	driver.Conn
	counter *statementCounter
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.counter.value.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.counter.value.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

var countingDriverSequence atomic.Int64

func openCountingStore(t *testing.T) (*Store, Project, *statementCounter) {
	t.Helper()

	counter := &statementCounter{}
	driverName := fmt.Sprintf("ito-counting-sqlite-%d", countingDriverSequence.Add(1))
	sql.Register(driverName, countingDriver{inner: &sqlite.Driver{}, counter: counter})

	db, err := sql.Open(driverName, filepath.Join(t.TempDir(), "ito.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate counting store: %v", err)
	}

	st := New(db)
	project, err := st.CreateProject("counting-app", "CNT", filepath.Join(t.TempDir(), "counting"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return st, project, counter
}

func TestShowBatchUsesThreeStatementsRegardlessOfWaveCount(t *testing.T) {
	st, project, counter := openCountingStore(t)
	if _, err := st.CreateBatch(project, "release"); err != nil {
		t.Fatalf("create batch: %v", err)
	}

	issues := make([]Issue, 5)
	for i := range issues {
		issues[i] = createStoreIssueInBatch(t, st, project, fmt.Sprintf("Wave %d", i+1), "todo", "medium", "release")
		if i > 0 {
			addStoreLink(t, st, project, issues[i].ID, "blocked_by", issues[i-1].ID)
		}
	}

	counter.value.Store(0)
	plan, err := st.ShowBatch(project, "release")
	if err != nil {
		t.Fatalf("show batch: %v", err)
	}
	if len(plan.Waves) != len(issues) {
		t.Fatalf("waves = %d, want %d", len(plan.Waves), len(issues))
	}
	if got := counter.value.Load(); got != 3 {
		t.Fatalf("ShowBatch statements = %d, want 3", got)
	}
}
