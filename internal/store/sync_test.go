package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
CREATE TABLE issue_links (
  project_id INTEGER NOT NULL,
  source_id  TEXT NOT NULL,
  target_id  TEXT NOT NULL,
  kind       TEXT NOT NULL,
  PRIMARY KEY (project_id, source_id, target_id, kind)
);
CREATE TABLE issue_labels (
  project_id INTEGER NOT NULL,
  issue_id   TEXT NOT NULL,
  label      TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_id, label)
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
	assertSchemaVersion(t, db, 6)
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

	// A pushes its project, the issue and its label; B, holding the same
	// project already, pulls the issue and the label.
	results := syncDevices(t, l, a, b)
	if results[0].Pushed != 3 || results[0].Pulled != 0 {
		t.Fatalf("A first sync = %+v, want pushed 3 pulled 0", results[0])
	}
	if results[1].Pushed != 1 || results[1].Pulled != 2 {
		t.Fatalf("B first sync = %+v, want pushed 1 pulled 2", results[1])
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
	// Applying the pulled deletion must not echo a tombstone back.
	results = syncDevices(t, l, b, a)
	for i, result := range results {
		if result.Pushed != 0 || result.Pulled != 0 {
			t.Fatalf("sync %d after a pulled delete = %+v, want nothing moved", i, result)
		}
	}
}

func TestSyncKeepsSearchInStepWithPulledUpdatesAndDeletes(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T14:00:00Z")
	kept := createStoreIssue(t, a.st, a.p, "Grinder burrs", "todo", "medium")
	dropped := createStoreIssue(t, a.st, a.p, "Grinder hopper", "todo", "medium")
	syncDevices(t, l, a, b)

	setClock(t, "2026-08-24T14:01:00Z")
	if _, err := a.st.Edit(a.p, kept.ID, EditIssueOptions{TitleSet: true, Title: "Tamper handle"}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.DeleteIssue(a.p, dropped.ID); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a, b)

	search := func(term string) []string {
		found, err := b.st.ListIssues(ListOptions{ProjectID: b.p.ID, Search: term})
		if err != nil {
			t.Fatal(err)
		}
		return storeIssueIDs(found)
	}
	if got := search("grinder"); len(got) != 0 {
		t.Fatalf("search for the old title and the deleted issue = %v, want none", got)
	}
	if got := search("tamper"); len(got) != 1 || got[0] != kept.ID {
		t.Fatalf("search for the pulled title = %v, want %s", got, kept.ID)
	}
}

func TestSyncRefusesAProjectWhosePrefixDiffersLocally(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	setClock(t, "2026-08-24T15:00:00Z")
	createStoreIssue(t, a.st, a.p, "From A", "todo", "medium")
	syncDevices(t, l, a)

	otherDB, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other := New(otherDB)
	if _, err := other.CreateProject("shared", "OTH", filepath.Join(t.TempDir(), "other")); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Sync(l); err == nil || !strings.Contains(err.Error(), "prefix OTH here but SHR") {
		t.Fatalf("expected an actionable prefix mismatch, got %v", err)
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

	// Entries: A's project, then four issues; three reads land two issues.
	l.reads, l.failAfter = 0, 3
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
	if result.Pulled != 3 {
		t.Fatalf("bootstrap = %+v, want the project and 2 issues pulled", result)
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

func TestSyncLinksAndLabelsRoundTripIncludingRemovals(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T16:00:00Z")
	one := createStoreIssue(t, a.st, a.p, "One", "todo", "medium")
	two := createStoreIssue(t, a.st, a.p, "Two", "todo", "medium")
	three, err := a.st.CreateIssue(a.p, "Three", "todo", "medium", []string{"feature", "docs"}, "")
	if err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T16:01:00Z")
	if _, err := a.st.Edit(a.p, two.ID, EditIssueOptions{
		LinkOps: []LinkEditOp{
			{Action: "add", Kind: "blocked_by", Target: one.ID},
			{Action: "add", Kind: "relates_to", Target: three.ID},
			{Action: "add", Kind: "conflicts_with", Target: three.ID},
		},
		LabelOps: []LabelEditOp{{Kind: "add", Label: "bug"}},
	}); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a, b)

	onB, err := b.st.FindIssue(b.p, two.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := onB.BlockedBy; len(got) != 1 || got[0] != one.ID {
		t.Fatalf("blocked_by on B = %v, want [%s]", got, one.ID)
	}
	if got := onB.RelatesTo; len(got) != 1 || got[0] != three.ID {
		t.Fatalf("relates_to on B = %v, want [%s]", got, three.ID)
	}
	if got := onB.ConflictsWith; len(got) != 1 || got[0] != three.ID {
		t.Fatalf("conflicts_with on B = %v, want [%s]", got, three.ID)
	}
	if got := onB.Labels; len(got) != 1 || got[0] != "bug" {
		t.Fatalf("labels on B = %v, want [bug]", got)
	}
	threeOnB, err := b.st.FindIssue(b.p, three.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := threeOnB.Labels; len(got) != 2 || got[0] != "docs" || got[1] != "feature" {
		t.Fatalf("labels set at creation on B = %v, want [docs feature]", got)
	}

	// Removals on B travel back to A.
	setClock(t, "2026-08-24T16:02:00Z")
	if _, err := b.st.Edit(b.p, two.ID, EditIssueOptions{
		LinkOps:  []LinkEditOp{{Action: "remove", Kind: "blocked_by", Target: one.ID}},
		LabelOps: []LabelEditOp{{Kind: "remove", Label: "bug"}},
	}); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, b, a)
	onA, err := a.st.FindIssue(a.p, two.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(onA.BlockedBy) != 0 || len(onA.Labels) != 0 {
		t.Fatalf("removals must reach A, got blocked_by %v labels %v", onA.BlockedBy, onA.Labels)
	}
	if len(onA.RelatesTo) != 1 || len(onA.ConflictsWith) != 1 {
		t.Fatalf("untouched links must survive on A, got %#v", onA)
	}
}

func TestSyncBatchesRoundTripAndWavesMatch(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T17:00:00Z")
	if _, err := a.st.CreateBatch(a.p, "plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateBatch(a.p, "doomed"); err != nil {
		t.Fatal(err)
	}
	first := createStoreIssueInBatch(t, a.st, a.p, "First", "todo", "medium", "plan")
	second := createStoreIssueInBatch(t, a.st, a.p, "Second", "todo", "medium", "plan")
	loose := createStoreIssue(t, a.st, a.p, "Loose", "todo", "medium")
	setClock(t, "2026-08-24T17:01:00Z")
	if _, err := a.st.Edit(a.p, second.ID, EditIssueOptions{LinkOps: []LinkEditOp{{Action: "add", Kind: "blocked_by", Target: first.ID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.Edit(a.p, loose.ID, EditIssueOptions{BatchSet: true, Batch: "plan"}); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a, b)

	batchesOnB, err := b.st.ListBatches(b.p)
	if err != nil {
		t.Fatal(err)
	}
	if len(batchesOnB) != 2 {
		t.Fatalf("batches on B = %+v, want plan and doomed", batchesOnB)
	}
	for _, batch := range batchesOnB {
		if batch.Created != "2026-08-24T17:00:00Z" {
			t.Fatalf("batch %s on B keeps the origin created, got %s", batch.Name, batch.Created)
		}
		if batch.Name == "plan" && batch.Total != 3 {
			t.Fatalf("plan on B has %d members, want 3", batch.Total)
		}
	}
	planA, err := a.st.ShowBatch(a.p, "plan")
	if err != nil {
		t.Fatal(err)
	}
	planB, err := b.st.ShowBatch(b.p, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(planA.Waves) != len(planB.Waves) || len(planA.Waves) != 2 {
		t.Fatalf("waves differ: A %d, B %d, want 2", len(planA.Waves), len(planB.Waves))
	}
	for i := range planA.Waves {
		if got, want := storeIssueIDs(planB.Waves[i].Issues), storeIssueIDs(planA.Waves[i].Issues); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("wave %d on B = %v, want %v", i+1, got, want)
		}
	}

	// Rename, membership removal and deletion travel, including from B.
	setClock(t, "2026-08-24T17:02:00Z")
	if _, err := b.st.RenameBatch(b.p, "plan", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.st.Edit(b.p, loose.ID, EditIssueOptions{BatchSet: true, Batch: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.st.DeleteBatch(b.p, "doomed"); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, b, a)
	batchesOnA, err := a.st.ListBatches(a.p)
	if err != nil {
		t.Fatal(err)
	}
	if len(batchesOnA) != 1 || batchesOnA[0].Name != "renamed" || batchesOnA[0].Total != 2 {
		t.Fatalf("batches on A after rename/removal/delete = %+v, want only renamed with 2 members", batchesOnA)
	}
	looseOnA, err := a.st.FindIssue(a.p, loose.ID)
	if err != nil {
		t.Fatal(err)
	}
	if looseOnA.Batch != nil {
		t.Fatalf("loose issue must leave the batch on A, got %v", *looseOnA.Batch)
	}
	// Renaming keeps the members attached and further edits resolve the new name.
	firstOnA, err := a.st.FindIssue(a.p, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstOnA.Batch == nil || *firstOnA.Batch != "renamed" {
		t.Fatalf("first issue on A should sit in renamed, got %v", firstOnA.Batch)
	}
}

func TestSyncProjectsArriveDetachedAndKeepLocalRoot(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	setClock(t, "2026-08-24T18:00:00Z")
	other, err := a.st.CreateProject("other", "OTH", filepath.Join(t.TempDir(), "other-on-a"))
	if err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a)

	emptyDB, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer emptyDB.Close()
	b := New(emptyDB)
	if _, err := b.Sync(l); err != nil {
		t.Fatal(err)
	}
	onB, found, err := b.FindProjectByName("other")
	if err != nil || !found {
		t.Fatalf("project created on A must arrive on B, found=%v err=%v", found, err)
	}
	if onB.RootPath != nil || onB.Prefix != "OTH" {
		t.Fatalf("project on B = %#v, want detached with prefix OTH", onB)
	}

	// ito init attaches by name on B; A's later Changes never touch that path.
	rootOnB := filepath.Join(t.TempDir(), "other-on-b")
	onB.RootPath = &rootOnB
	if _, err := b.UpdateProjectRoot(onB); err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T18:01:00Z")
	if _, err := a.st.RenameProject(other, "renamed"); err != nil {
		t.Fatal(err)
	}
	createStoreIssue(t, a.st, other, "In other", "todo", "medium")
	syncDevices(t, l, a)
	if _, err := b.Sync(l); err != nil {
		t.Fatal(err)
	}
	renamed, found, err := b.FindProjectByName("renamed")
	if err != nil || !found {
		t.Fatalf("rename must reach B, found=%v err=%v", found, err)
	}
	if renamed.ID != onB.ID || renamed.RootPath == nil || *renamed.RootPath != rootOnB {
		t.Fatalf("project on B = %#v, want the same row with root %s", renamed, rootOnB)
	}
	issues, err := b.ListIssues(ListOptions{ProjectID: renamed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ID != "OTH-1" {
		t.Fatalf("issues in the renamed project on B = %v, want OTH-1", storeIssueIDs(issues))
	}
}

func TestSyncAppliesBatchesForProjectsThatPredateProjectChanges(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	// Projects created before project Changes existed have none in the Ledger.
	if _, err := a.db.Exec(`DELETE FROM changes`); err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T19:00:00Z")
	if _, err := a.st.CreateBatch(a.p, "early"); err != nil {
		t.Fatal(err)
	}
	createStoreIssueInBatch(t, a.st, a.p, "Member", "todo", "medium", "early")
	syncDevices(t, l, a)

	emptyDB, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer emptyDB.Close()
	b := New(emptyDB)
	if _, err := b.Sync(l); err != nil {
		t.Fatalf("a batch Change must create its project detached: %v", err)
	}
	p, _, err := b.FindProjectByName("shared")
	if err != nil {
		t.Fatal(err)
	}
	batches, err := b.ListBatches(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0].Total != 1 {
		t.Fatalf("batches on B = %+v, want early with 1 member", batches)
	}
}

func TestSyncConcurrentLabelAndLinkEditsConvergeOnTheLaterOne(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")

	setClock(t, "2026-08-24T20:00:00Z")
	issue, err := a.st.CreateIssue(a.p, "Labelled", "todo", "medium", []string{"bug"}, "")
	if err != nil {
		t.Fatal(err)
	}
	other := createStoreIssue(t, a.st, a.p, "Other", "todo", "medium")
	if _, err := a.st.Edit(a.p, issue.ID, EditIssueOptions{LinkOps: []LinkEditOp{{Action: "add", Kind: "blocked_by", Target: other.ID}}}); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, a, b)

	// B removes earlier, A re-affirms later, but B pushes first.
	setClock(t, "2026-08-24T20:01:00Z")
	if _, err := b.st.Edit(b.p, issue.ID, EditIssueOptions{
		LabelOps: []LabelEditOp{{Kind: "remove", Label: "bug"}},
		LinkOps:  []LinkEditOp{{Action: "remove", Kind: "blocked_by", Target: other.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	setClock(t, "2026-08-24T20:02:00Z")
	if _, err := a.st.Edit(a.p, issue.ID, EditIssueOptions{
		LabelOps: []LabelEditOp{{Kind: "remove", Label: "bug"}, {Kind: "add", Label: "bug"}, {Kind: "add", Label: "docs"}},
	}); err != nil {
		t.Fatal(err)
	}
	syncDevices(t, l, b, a, b)
	for name, device := range map[string]syncDevice{"A": a, "B": b} {
		got, err := device.st.FindIssue(device.p, issue.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Labels) != 2 || got.Labels[0] != "bug" || got.Labels[1] != "docs" {
			t.Fatalf("%s labels = %v, want the later edit [bug docs]", name, got.Labels)
		}
		if len(got.BlockedBy) != 0 {
			t.Fatalf("%s blocked_by = %v, want B's removal (A never re-touched the link)", name, got.BlockedBy)
		}
	}
}

func TestMigrateV6StampsSetRowsOnV5Database(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v5.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (5);
CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, root_path TEXT UNIQUE, prefix TEXT UNIQUE NOT NULL, last_id INTEGER NOT NULL DEFAULT 0);
CREATE TABLE issues (row_id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL, id TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL, priority TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', created TEXT NOT NULL, updated TEXT NOT NULL, batch_id INTEGER, category TEXT NOT NULL DEFAULT 'uncategorized', triage_state TEXT NOT NULL DEFAULT 'needs-triage', branch TEXT NOT NULL DEFAULT '');
CREATE TABLE issue_links (project_id INTEGER NOT NULL, source_id TEXT NOT NULL, target_id TEXT NOT NULL, kind TEXT NOT NULL, PRIMARY KEY (project_id, source_id, target_id, kind));
CREATE TABLE issue_labels (project_id INTEGER NOT NULL, issue_id TEXT NOT NULL, label TEXT NOT NULL, PRIMARY KEY (project_id, issue_id, label));
CREATE TABLE changes (seq INTEGER PRIMARY KEY, kind TEXT NOT NULL, project TEXT NOT NULL, key TEXT NOT NULL, deleted INTEGER NOT NULL DEFAULT 0, updated TEXT NOT NULL, state TEXT, pushed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO projects(name, prefix, last_id) VALUES ('legacy', 'LEG', 1);
INSERT INTO issues(project_id, id, title, status, priority, created, updated) VALUES (1, 'LEG-1', 'Existing', 'todo', 'medium', '2026-08-09T10:00:00Z', '2026-08-09T10:00:00Z');
INSERT INTO issue_labels(project_id, issue_id, label) VALUES (1, 'LEG-1', 'bug');
`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate v5 database: %v", err)
	}
	assertSchemaVersion(t, db, 6)
	var updated string
	if err := db.QueryRow(`SELECT updated FROM issue_labels WHERE issue_id = 'LEG-1'`).Scan(&updated); err != nil || updated != "" {
		t.Fatalf("legacy label rows carry the empty stamp, got %q err %v", updated, err)
	}
	if _, err := db.Exec(`INSERT INTO set_tombstones(kind, project_id, key, updated) VALUES ('label', 1, 'x', '')`); err != nil {
		t.Fatalf("set_tombstones table missing: %v", err)
	}
}
