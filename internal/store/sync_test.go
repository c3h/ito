package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/c3h/ito/internal/ledger"
)

func TestMigrateV5AddsChangeLogToV4Database(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v4.db"))
	if err != nil {
		t.Fatalf("open v4 database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (4);
CREATE TABLE projects (
  id        INTEGER PRIMARY KEY,
  name      TEXT UNIQUE NOT NULL,
  root_path TEXT UNIQUE,
  prefix    TEXT UNIQUE NOT NULL,
  last_id   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE issues (
  row_id       INTEGER PRIMARY KEY,
  project_id   INTEGER NOT NULL,
  id           TEXT NOT NULL,
  title        TEXT NOT NULL,
  status       TEXT NOT NULL,
  priority     TEXT NOT NULL,
  body         TEXT NOT NULL DEFAULT '',
  created      TEXT NOT NULL,
  updated      TEXT NOT NULL,
  batch_id     INTEGER,
  category     TEXT NOT NULL DEFAULT 'uncategorized',
  triage_state TEXT NOT NULL DEFAULT 'needs-triage',
  branch       TEXT NOT NULL DEFAULT ''
);
INSERT INTO projects(name, prefix, last_id) VALUES ('legacy', 'LEG', 1);
INSERT INTO issues(project_id, id, title, status, priority, created, updated)
VALUES (1, 'LEG-1', 'Existing issue', 'todo', 'medium', '2026-08-09T10:00:00Z', '2026-08-09T10:00:00Z');
`); err != nil {
		t.Fatalf("seed v4 database: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate v4 database: %v", err)
	}
	assertSchemaVersion(t, db, 5)
	var pending int
	if err := db.QueryRow(`SELECT count(*) FROM changes WHERE pushed = 0`).Scan(&pending); err != nil {
		t.Fatalf("read change log: %v", err)
	}
	if pending != 0 {
		t.Fatalf("an upgraded database starts with an empty change log, got %d pending", pending)
	}
	if _, err := db.Exec(`INSERT INTO sync_state(key, value) VALUES ('probe', 'x')`); err != nil {
		t.Fatalf("sync_state table missing: %v", err)
	}
}

type syncDevice struct {
	db *sql.DB
	st *Store
	p  Project
}

func openSyncDevice(t *testing.T, name string) syncDevice {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	st := New(db)
	p, err := st.CreateProject("shared", "SHR", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("create project on %s: %v", name, err)
	}
	return syncDevice{db: db, st: st, p: p}
}

func syncDevices(t *testing.T, l ledger.Ledger, devices ...syncDevice) []SyncResult {
	t.Helper()
	results := make([]SyncResult, 0, len(devices))
	for _, device := range devices {
		result, err := device.st.Sync(l)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		results = append(results, result)
	}
	return results
}

func setClock(t *testing.T, stamp string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	old := clock
	clock = func() time.Time { return parsed }
	t.Cleanup(func() { clock = old })
}

func TestSyncConvergesTwoStoresThroughOneLedger(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T10:00:00Z")
	created, err := a.st.CreateIssue(a.p, "Created on A", "todo", "high", []string{"feature"}, "body from A")
	if err != nil {
		t.Fatalf("create on A: %v", err)
	}

	results := syncDevices(t, l, a, b)
	if results[0].Pushed != 1 || results[0].Pulled != 0 {
		t.Fatalf("A first sync = %+v, want pushed 1 pulled 0", results[0])
	}
	if results[1].Pushed != 0 || results[1].Pulled != 1 {
		t.Fatalf("B first sync = %+v, want pushed 0 pulled 1", results[1])
	}
	onB, err := b.st.FindIssue(b.p, created.ID)
	if err != nil {
		t.Fatalf("issue missing on B after sync: %v", err)
	}
	if onB.Title != created.Title || onB.Status != created.Status || onB.Priority != created.Priority || onB.Body != created.Body || onB.Created != created.Created {
		t.Fatalf("issue on B = %#v, want the row created on A %#v", onB, created)
	}
	if onB.Updated != created.Updated {
		t.Fatalf("pulled issue keeps the originating updated %q, got %q", created.Updated, onB.Updated)
	}

	// Edit on both: the later updated wins on both Devices.
	setClock(t, "2026-08-24T10:05:00Z")
	if _, err := b.st.Edit(b.p, created.ID, EditIssueOptions{TitleSet: true, Title: "Older edit on B"}); err != nil {
		t.Fatalf("edit on B: %v", err)
	}
	setClock(t, "2026-08-24T10:10:00Z")
	if _, err := a.st.Edit(a.p, created.ID, EditIssueOptions{TitleSet: true, Title: "Newer edit on A"}); err != nil {
		t.Fatalf("edit on A: %v", err)
	}
	syncDevices(t, l, a, b, a)
	for name, device := range map[string]syncDevice{"A": a, "B": b} {
		issue, err := device.st.FindIssue(device.p, created.ID)
		if err != nil {
			t.Fatalf("find on %s: %v", name, err)
		}
		if issue.Title != "Newer edit on A" || issue.Updated != "2026-08-24T10:10:00Z" {
			t.Fatalf("%s converged to %q at %s, want the newer edit", name, issue.Title, issue.Updated)
		}
	}

	// A second sync in a row moves nothing.
	results = syncDevices(t, l, a, b)
	for i, result := range results {
		if result.Pushed != 0 || result.Pulled != 0 {
			t.Fatalf("idle sync %d = %+v, want nothing pushed or pulled", i, result)
		}
	}

	// Delete on A: gone on B.
	setClock(t, "2026-08-24T10:20:00Z")
	if err := a.st.DeleteIssue(a.p, created.ID); err != nil {
		t.Fatalf("delete on A: %v", err)
	}
	syncDevices(t, l, a, b)
	if _, err := b.st.FindIssue(b.p, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted issue must be gone on B, got err %v", err)
	}
}

func TestSyncMoveBatchMoveAndPruneTravel(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T11:00:00Z")
	if _, err := a.st.CreateBatch(a.p, "wave"); err != nil {
		t.Fatal(err)
	}
	first := createStoreIssueInBatch(t, a.st, a.p, "First", "todo", "medium", "wave")
	second := createStoreIssueInBatch(t, a.st, a.p, "Second", "todo", "medium", "wave")
	third := createStoreIssue(t, a.st, a.p, "Third", "todo", "low")
	syncDevices(t, l, a, b)

	setClock(t, "2026-08-24T11:01:00Z")
	if _, err := a.st.Move(a.p, third.ID, "done"); err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T11:02:00Z")
	if _, err := a.st.MoveBatch(a.p, "wave", "in_progress"); err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T11:03:00Z")
	if _, err := a.st.DeleteIssuesByStatus(a.p, "done"); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a, b)

	for _, id := range []string{first.ID, second.ID} {
		issue, err := b.st.FindIssue(b.p, id)
		if err != nil {
			t.Fatalf("find %s on B: %v", id, err)
		}
		if issue.Status != "in_progress" || issue.Updated != "2026-08-24T11:02:00Z" {
			t.Fatalf("%s on B = %s at %s, want in_progress from the batch move", id, issue.Status, issue.Updated)
		}
	}
	if _, err := b.st.FindIssue(b.p, third.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned issue must be gone on B, got err %v", err)
	}
}

// pagingLedger hands out one Entry per read and fails the read after a chosen
// number of calls, to stand in for a connection dropping mid-pull.
type pagingLedger struct {
	*ledger.Memory
	reads     int
	failAfter int
}

var errDropped = errors.New("connection dropped")

func (p *pagingLedger) ReadAfter(position int64, limit int) ([]ledger.Entry, error) {
	p.reads++
	if p.failAfter > 0 && p.reads > p.failAfter {
		return nil, errDropped
	}
	return p.Memory.ReadAfter(position, 1)
}

func TestSyncResumesAnInterruptedPullWithoutDuplicatesOrGaps(t *testing.T) {
	l := &pagingLedger{Memory: ledger.NewMemory()}
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T12:00:00Z")
	ids := []string{}
	for _, title := range []string{"one", "two", "three", "four"} {
		ids = append(ids, createStoreIssue(t, a.st, a.p, title, "todo", "medium").ID)
	}
	syncDevices(t, l, a)

	l.reads, l.failAfter = 0, 2
	result, err := b.st.Sync(l)
	if !errors.Is(err, errDropped) {
		t.Fatalf("expected the dropped connection to surface, got result %+v err %v", result, err)
	}
	listed, err := b.st.ListIssues(ListOptions{ProjectID: b.p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("an interrupted pull keeps what it applied, got %d issues on B", len(listed))
	}

	l.failAfter = 0
	result, err = b.st.Sync(l)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Pulled != 2 {
		t.Fatalf("resumed pull = %+v, want exactly the 2 remaining Changes", result)
	}
	listed, err = b.st.ListIssues(ListOptions{ProjectID: b.p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := storeIssueIDs(listed); len(got) != 4 {
		t.Fatalf("B must hold every issue exactly once, got %v (want %v)", got, ids)
	}
}

func TestSyncBootstrapsAnEmptyDeviceAndKeepsSearchWorking(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")

	setClock(t, "2026-08-24T13:00:00Z")
	if _, err := a.st.CreateIssue(a.p, "Café latte machine", "todo", "medium", nil, "espresso body"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateIssue(a.p, "Grinder", "backlog", "low", nil, ""); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a)

	emptyDB, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer emptyDB.Close()
	empty := New(emptyDB)
	result, err := empty.Sync(l)
	if err != nil {
		t.Fatalf("bootstrap sync: %v", err)
	}
	if result.Pulled != 2 {
		t.Fatalf("bootstrap = %+v, want 2 pulled", result)
	}

	p, found, err := empty.FindProjectByName("shared")
	if err != nil || !found {
		t.Fatalf("project must be created detached on the empty Device, found=%v err=%v", found, err)
	}
	if p.RootPath != nil || p.Prefix != "SHR" {
		t.Fatalf("detached project = %#v, want no root_path and prefix SHR", p)
	}
	origin, err := a.st.ListIssues(ListOptions{ProjectID: a.p.ID, IncludeDone: true})
	if err != nil {
		t.Fatal(err)
	}
	pulled, err := empty.ListIssues(ListOptions{ProjectID: p.ID, IncludeDone: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled) != len(origin) {
		t.Fatalf("empty Device holds %d issues, origin %d", len(pulled), len(origin))
	}
	for i := range origin {
		if origin[i].ID != pulled[i].ID || origin[i].Title != pulled[i].Title || origin[i].Updated != pulled[i].Updated || origin[i].Created != pulled[i].Created {
			t.Fatalf("row %d differs: origin %#v pulled %#v", i, origin[i], pulled[i])
		}
	}

	found2, err := empty.ListIssues(ListOptions{ProjectID: p.ID, Search: "cafe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found2) != 1 || found2[0].Title != "Café latte machine" {
		t.Fatalf("search on pulled data = %v, want the café issue", storeIssueIDs(found2))
	}

	// Local numbering on the bootstrapped Device continues after the pulled IDs.
	next := createStoreIssue(t, empty, p, "Local", "todo", "medium")
	if next.ID != "SHR-3" {
		t.Fatalf("next local ID = %s, want SHR-3", next.ID)
	}
}
