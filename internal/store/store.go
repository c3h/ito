package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	itoconfig "github.com/c3h/ito/internal/config"
	"github.com/c3h/ito/internal/ledger"
	_ "github.com/tursodatabase/libsql-client-go/libsql" // Ledger transport (decision 0005)
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

// The canonical identifier formats. The store owns them; the CLI validates its
// input against these same patterns so each format lives in exactly one place.
var (
	ProjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	PrefixPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,7}$`)
	IssueIDPattern     = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,7})-([1-9][0-9]*)$`)
)

var (
	searchTermPattern = regexp.MustCompile(`[\pL\pN]+`)
	// asciiFold decomposes (NFD) and strips combining marks, transliterating
	// Latin accents to ASCII (café → cafe). Scripts with no decomposition to
	// ASCII (Cyrillic, CJK) pass through intact and are discarded downstream.
	asciiFold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
)

// The canonical domain vocabularies, in flow/precedence order. The store owns
// them; the CLI derives its validation sets and the TUI derives its surfaces
// from these slices so the vocabulary lives in exactly one place.
var (
	Statuses     = []string{"backlog", "todo", "in_progress", "in_review", "done"}
	Priorities   = []string{"urgent", "high", "medium", "low"}
	Categories   = []string{"uncategorized", "bug", "enhancement", "docs", "tests", "refactor", "chore", "research", "infra"}
	TriageStates = []string{"needs-triage", "ready-for-agent", "ready-for-human", "wontfix"}
	Labels       = []string{"feature", "bug", "docs", "tests", "refactor", "chore", "research", "infra"}
)

// The list ordering derives its precedence from the canonical slices above, so
// adding a vocabulary value automatically ranks it — values are package
// constants, never user input.
var (
	statusOrderSQL   = orderCase("issues.status", Statuses)
	priorityOrderSQL = orderCase("issues.priority", Priorities)
)

// unknownOrderRank sorts a value outside its enumeration last, in SQL and in
// the client-side derivations that have to agree with it.
const unknownOrderRank = 99

func orderCase(column string, values []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CASE %s", column)
	for i, value := range values {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", value, i+1)
	}
	fmt.Fprintf(&b, " ELSE %d END", unknownOrderRank)
	return b.String()
}

var (
	ErrNotRegistered = errors.New("project not registered")
	ErrDetached      = errors.New("project detached")
	ErrNotFound      = errors.New("not found")
	ErrBatchNotFound = errors.New("batch not found")
	ErrBatchCycle    = errors.New("batch dependency cycle")
	ErrCrossProject  = errors.New("cross-project link")
	ErrNameExists    = errors.New("project name already exists")
	ErrPrefixExists  = errors.New("project prefix already exists")
	ErrBatchExists   = errors.New("batch name already exists")
)

// LinkTargetNotFoundError distinguishes a missing link *target* from a missing
// source Issue, so the CLI can name the right Issue in its error. It unwraps to
// ErrNotFound to keep the existing not-found handling working.
type LinkTargetNotFoundError struct {
	TargetID string
}

func (e *LinkTargetNotFoundError) Error() string {
	return fmt.Sprintf("%v: link target %q", ErrNotFound, e.TargetID)
}

func (e *LinkTargetNotFoundError) Unwrap() error {
	return ErrNotFound
}

type DetachedError struct {
	ProjectName string
}

func (e *DetachedError) Error() string {
	return fmt.Sprintf("%v: %s", ErrDetached, e.ProjectName)
}

func (e *DetachedError) Unwrap() error {
	return ErrDetached
}

type CrossProjectError struct {
	IssueID       string
	SourceProject string
	TargetProject string
}

func (e *CrossProjectError) Error() string {
	return fmt.Sprintf("%v: Issue %q belongs to Project %q, not to %q", ErrCrossProject, e.IssueID, e.TargetProject, e.SourceProject)
}

func (e *CrossProjectError) Unwrap() error {
	return ErrCrossProject
}

type BatchCycleError struct {
	Issues []string
}

func (e *BatchCycleError) Error() string {
	return fmt.Sprintf("%v: %s", ErrBatchCycle, strings.Join(e.Issues, ", "))
}

func (e *BatchCycleError) Unwrap() error {
	return ErrBatchCycle
}

type Project struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Prefix   string  `json:"prefix"`
	RootPath *string `json:"root_path"`
}

type Issue struct {
	ID            string   `json:"id"`
	Project       string   `json:"project"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	Priority      string   `json:"priority"`
	Category      string   `json:"category"`
	TriageState   string   `json:"triage_state"`
	Branch        string   `json:"branch"`
	Labels        []string `json:"labels"`
	BlockedBy     []string `json:"blocked_by"`
	RelatesTo     []string `json:"relates_to"`
	ConflictsWith []string `json:"conflicts_with"`
	Batch         *string  `json:"batch"`
	Body          string   `json:"body"`
	Created       string   `json:"created"`
	Updated       string   `json:"updated"`
}

type Batch struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Created string `json:"created"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
}

// Date renders the Batch's created timestamp as its calendar date (UTC) —
// chronology, never identity. It lives here so the CLI and the TUI show the
// same date from the same RFC3339 string; a timestamp that won't parse is
// returned unchanged.
func (b Batch) Date() string {
	parsed, err := time.Parse(time.RFC3339, b.Created)
	if err != nil {
		return b.Created
	}
	return parsed.UTC().Format("2006-01-02")
}

// Complete reports that every member is done, which an empty Batch is not: it
// has nothing to finish. The CLI and the TUI both branch on it, so the two
// surfaces cannot disagree about which Batches are behind them.
func (b Batch) Complete() bool {
	return b.Total > 0 && b.Done == b.Total
}

type DeleteBatchResult struct {
	Name           string
	MembersCleared int
}

type BatchPlan struct {
	Batch
	Waves   []BatchWave `json:"waves"`
	Waiting []Issue     `json:"waiting"`
}

type BatchWave struct {
	Wave   int     `json:"wave"`
	Ready  bool    `json:"ready"`
	Done   bool    `json:"done"`
	Issues []Issue `json:"issues"`
}

type ShowBatchOptions struct {
	IncludeDone bool
}

type ListOptions struct {
	ProjectID   int64
	AllProjects bool
	Status      string
	Priority    string
	Category    string
	TriageState string
	Labels      []string
	Search      string
	Ready       bool
	Batch       string
	// IncludeDone lifts the default "done is hidden" filter when no Status is
	// set, so a caller can load every status in one consistent query.
	IncludeDone bool
}

type EditIssueOptions struct {
	TitleSet       bool
	Title          string
	PrioritySet    bool
	Priority       string
	CategorySet    bool
	Category       string
	TriageStateSet bool
	TriageState    string
	BranchSet      bool
	Branch         string
	BodySet        bool
	Body           string
	BatchSet       bool
	Batch          string
	LabelOps       []LabelEditOp
	LinkOps        []LinkEditOp
}

type LabelEditOp struct {
	Kind  string
	Label string
}

type LinkEditOp struct {
	Kind   string
	Action string
	Target string
}

type issueDeletionRow struct {
	rowID int64
	id    string
	title string
	body  string
}

type MoveResult struct {
	Issue        Issue
	BeforeStatus string
	Changed      bool
}

type BatchMoveResult struct {
	Batch        string
	Project      string
	TargetStatus string
	Changed      []string
	Skipped      []string
}

type EditResult struct {
	Issue   Issue
	Changed bool
}

type Store struct {
	db *sql.DB
	// numberer hands out Issue numbers when set; nil numbers locally.
	numberer IssueNumberer
}

// IssueNumberer reserves the next Issue number for a Project, keyed by its
// Prefix — the identity that survives renames across Devices — and above
// floor, the highest number this Device holds. A connected Ledger satisfies
// it (decision 0005): numbers then stay globally sequential and the local
// counter only records the highest number seen.
type IssueNumberer interface {
	ReserveIssueNumber(prefix string, floor int64) (int64, error)
}

// ReservationError reports that the numberer refused a number, so nothing
// was written.
type ReservationError struct {
	Err error
}

func (e *ReservationError) Error() string {
	return "reserve an Issue number: " + e.Err.Error()
}

func (e *ReservationError) Unwrap() error { return e.Err }

// SetIssueNumberer routes every creation through n; every creation path —
// the CLI today, the TUI when it grows one — inherits it.
func (s *Store) SetIssueNumberer(n IssueNumberer) {
	s.numberer = n
}

// clock stamps every mutation; tests replace it to control updated.
var clock = time.Now

// nowStamp renders the current instant the one way every stamp is written:
// RFC3339 in UTC, the format the last-writer-wins comparison depends on.
func nowStamp() string { return clock().UTC().Format(time.RFC3339) }

// maxIdleConns keeps enough pooled connections warm for the widest concurrent
// read the surfaces issue, so a burst does not churn connections.
const maxIdleConns = 16

// Open opens (creating the directory if needed) the SQLite store under home and
// brings it to the latest schema. _txlock=immediate makes db.Begin() emit BEGIN
// IMMEDIATE so write transactions take the write lock up front, avoiding
// lock-upgrade deadlocks (read-then-write would otherwise fail with
// SQLITE_BUSY). WAL improves read/write concurrency. The _pragma settings apply
// to every pooled connection, unlike a single post-open db.Exec.
func Open(home string) (*sql.DB, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		itoconfig.LocalDBPath(home),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Concurrent readers — the TUI derives every open Batch at once — otherwise
	// overflow the default idle pool of two and pay a file open plus the DSN
	// pragmas for each surplus connection, on every refresh.
	db.SetMaxIdleConns(maxIdleConns)
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// TransliterateASCII decomposes Latin accents to their ASCII base (café → cafe)
// via asciiFold, leaving non-decomposable scripts intact. It is exported so the
// CLI shares the exact same folding the store uses for search.
func TransliterateASCII(input string) string {
	folded, _, err := transform.String(asciiFold, input)
	if err != nil {
		return input
	}
	return folded
}

func searchQuery(input string) string {
	terms := searchTermPattern.FindAllString(input, -1)
	if len(terms) == 0 {
		return ""
	}
	for i, term := range terms {
		terms[i] = strings.ToLower(term) + "*"
	}
	return strings.Join(terms, " ")
}

func (s *Store) CreateProject(name, prefix, rootPath string) (Project, error) {
	return insertProject(s.db, name, prefix, rootPath)
}

func (s *Store) CreateProjectWithGeneratedPrefix(name, baseInput, rootPath string) (Project, error) {
	return insertProjectWithGeneratedPrefix(s.db, name, baseInput, rootPath)
}

func (s *Store) FindProjectByRoot(rootPath string) (Project, bool, error) {
	return findProjectByRoot(s.db, rootPath)
}

func (s *Store) FindProjectByName(name string) (Project, bool, error) {
	return findProjectByName(s.db, name)
}

func (s *Store) FindProjectByPrefix(prefix string) (Project, bool, error) {
	return findProjectByPrefix(s.db, prefix)
}

func (s *Store) ListProjects() ([]Project, error) {
	return listProjects(s.db)
}

func (s *Store) FindDetachedProjectByName(name string, currentRoot string) (Project, bool, error) {
	return findDetachedProjectByName(s.db, name, currentRoot)
}

func (s *Store) FindClosestProjectAncestor(rootPath string) (Project, bool, error) {
	return findClosestProjectAncestor(s.db, rootPath)
}

func (s *Store) UpdateProjectRoot(p Project) (Project, error) {
	return updateProjectRoot(s.db, p)
}

func (s *Store) RenameProject(p Project, name string) (Project, error) {
	return updateProjectName(s.db, p, name)
}

func (s *Store) ProjectNameExistsForAnotherID(name string, id int64) (bool, error) {
	return projectNameExistsForAnotherID(s.db, name, id)
}

func (s *Store) ProjectNameExists(name string) (bool, error) {
	return valueExists(s.db, `SELECT 1 FROM projects WHERE name = ?`, name)
}

func (s *Store) ProjectPrefixExists(prefix string) (bool, error) {
	return valueExists(s.db, `SELECT 1 FROM projects WHERE prefix = ?`, prefix)
}

func (s *Store) CreateIssue(p Project, title, status, priority string, labels []string, body string) (Issue, error) {
	return s.CreateIssueInBatchWithMetadata(p, title, status, priority, "uncategorized", "needs-triage", labels, body, "")
}

func (s *Store) CreateIssueInBatch(p Project, title, status, priority string, labels []string, body string, batch string) (Issue, error) {
	return s.CreateIssueInBatchWithMetadata(p, title, status, priority, "uncategorized", "needs-triage", labels, body, batch)
}

func (s *Store) CreateIssueInBatchWithMetadata(p Project, title, status, priority, category, triageState string, labels []string, body string, batch string) (Issue, error) {
	return insertIssue(s.db, s.numberer, p, title, status, priority, category, triageState, labels, body, batch)
}

func (s *Store) CreateBatch(p Project, name string) (Batch, error) {
	return insertBatch(s.db, p, name)
}

// ListBatches returns the Project's Batches newest-first, each with its derived
// progress. The order is part of the contract: the TUI's Batches surface only
// partitions this list, so it inherits the ordering rather than re-deriving it.
func (s *Store) ListBatches(p Project) ([]Batch, error) {
	return listBatches(s.db, p)
}

func (s *Store) RenameBatch(p Project, oldName, newName string) (Batch, error) {
	return renameBatch(s.db, p, oldName, newName)
}

func (s *Store) DeleteBatch(p Project, name string) (DeleteBatchResult, error) {
	return deleteBatch(s.db, p, name)
}

func (s *Store) ShowBatch(p Project, name string) (BatchPlan, error) {
	return showBatch(s.db, p, name, ShowBatchOptions{})
}

func (s *Store) ShowBatchWithOptions(p Project, name string, opts ShowBatchOptions) (BatchPlan, error) {
	return showBatch(s.db, p, name, opts)
}

func (s *Store) FindIssue(p Project, id string) (Issue, error) {
	found, ok, err := findIssueByID(s.db, p, id)
	if err != nil {
		return Issue{}, err
	}
	if !ok {
		return Issue{}, ErrNotFound
	}
	return found, nil
}

func (s *Store) Move(p Project, id string, targetStatus string) (MoveResult, error) {
	moved, beforeStatus, changed, err := moveIssueStatus(s.db, p, id, targetStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MoveResult{}, ErrNotFound
		}
		return MoveResult{}, err
	}
	return MoveResult{Issue: moved, BeforeStatus: beforeStatus, Changed: changed}, nil
}

func (s *Store) MoveBatch(p Project, name string, targetStatus string) (BatchMoveResult, error) {
	return moveBatchStatus(s.db, p, name, targetStatus)
}

func (s *Store) Edit(p Project, id string, options EditIssueOptions) (EditResult, error) {
	edited, changed, err := editIssue(s.db, p, id, options)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EditResult{}, ErrNotFound
		}
		return EditResult{}, err
	}
	return EditResult{Issue: edited, Changed: changed}, nil
}

func (s *Store) DeleteIssue(p Project, id string) error {
	if err := deleteIssue(s.db, p, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Store) DeleteIssuesByStatus(p Project, status string) (int, error) {
	return deleteIssuesByStatus(s.db, p, status)
}

func (s *Store) ListIssues(options ListOptions) ([]Issue, error) {
	return listIssues(s.db, options)
}

func (s *Store) ResolveProject(rootPath string, inGit bool, explicitName string) (Project, error) {
	if explicitName != "" {
		p, found, err := findProjectByName(s.db, explicitName)
		if err != nil {
			return Project{}, err
		}
		if !found {
			return Project{}, ErrNotRegistered
		}
		return p, nil
	}

	p, found, err := findProjectByRoot(s.db, rootPath)
	if err != nil {
		return Project{}, err
	}
	if found {
		return p, nil
	}
	if !inGit {
		p, found, err := findClosestProjectAncestor(s.db, rootPath)
		if err != nil {
			return Project{}, err
		}
		if found {
			return p, nil
		}
	}
	name := NormalizeProjectName(filepath.Base(rootPath))
	detached, found, err := findDetachedProjectByName(s.db, name, rootPath)
	if err != nil {
		return Project{}, err
	}
	if found {
		return Project{}, &DetachedError{ProjectName: detached.Name}
	}
	return Project{}, ErrNotRegistered
}

// The config is loaded first so a retired cloud config fails before any file
// is created; it is returned so the caller need not read it again.
func OpenDefault() (*sql.DB, itoconfig.Config, error) {
	cfg, err := itoconfig.Load()
	if err != nil {
		return nil, itoconfig.Config{}, err
	}
	home, err := itoconfig.HomeDir()
	if err != nil {
		return nil, itoconfig.Config{}, err
	}
	db, err := Open(home)
	return db, cfg, err
}

var migrations = []struct {
	version int
	apply   func(*sql.Tx) error
}{
	{version: 1, apply: migrateV1},
	{version: 2, apply: migrateV2},
	{version: 3, apply: migrateV3},
	{version: 4, apply: migrateV4},
	{version: 5, apply: migrateV5},
	{version: 6, apply: migrateV6},
}

func Migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	version, err := currentSchemaVersion(db)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if version >= migration.version {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := migration.apply(tx); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES (?)`, migration.version); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = migration.version
	}
	return nil
}

func currentSchemaVersion(db *sql.DB) (int, error) {
	var version sql.NullInt64
	if err := db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
		return 0, err
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func migrateV1(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS projects (
  id        INTEGER PRIMARY KEY,
  name      TEXT UNIQUE NOT NULL,
  root_path TEXT UNIQUE,
  prefix    TEXT UNIQUE NOT NULL,
  last_id   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS issues (
  row_id     INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  id         TEXT NOT NULL,
  title      TEXT NOT NULL,
  status     TEXT NOT NULL,
  priority   TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  created    TEXT NOT NULL,
  updated    TEXT NOT NULL,
  UNIQUE (project_id, id)
);

CREATE VIRTUAL TABLE IF NOT EXISTS issues_fts USING fts5(
  title,
  body,
  content='issues',
  content_rowid='row_id',
  tokenize='unicode61',
  prefix='2 3 4'
);

CREATE TABLE IF NOT EXISTS issue_links (
  project_id INTEGER NOT NULL,
  source_id  TEXT NOT NULL,
  target_id  TEXT NOT NULL,
  kind       TEXT NOT NULL,
  PRIMARY KEY (project_id, source_id, target_id, kind),
  FOREIGN KEY (project_id, source_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  FOREIGN KEY (project_id, target_id) REFERENCES issues(project_id, id) ON DELETE CASCADE,
  CHECK (source_id != target_id)
);

CREATE TABLE IF NOT EXISTS issue_labels (
  project_id INTEGER NOT NULL,
  issue_id   TEXT NOT NULL,
  label      TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_id, label),
  FOREIGN KEY (project_id, issue_id) REFERENCES issues(project_id, id) ON DELETE CASCADE
);
`)
	return err
}

func migrateV2(tx *sql.Tx) error {
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS batches (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  created    TEXT NOT NULL,
  UNIQUE (project_id, name)
);
`); err != nil {
		return err
	}
	hasBatchID, err := columnExists(tx, "issues", "batch_id")
	if err != nil {
		return err
	}
	if !hasBatchID {
		if _, err := tx.Exec(`ALTER TABLE issues ADD COLUMN batch_id INTEGER REFERENCES batches(id) ON DELETE SET NULL`); err != nil {
			return err
		}
	}
	return nil
}

func migrateV3(tx *sql.Tx) error {
	hasCategory, err := columnExists(tx, "issues", "category")
	if err != nil {
		return err
	}
	if !hasCategory {
		if _, err := tx.Exec(`ALTER TABLE issues ADD COLUMN category TEXT NOT NULL DEFAULT 'uncategorized'`); err != nil {
			return err
		}
	}
	hasTriageState, err := columnExists(tx, "issues", "triage_state")
	if err != nil {
		return err
	}
	if !hasTriageState {
		if _, err := tx.Exec(`ALTER TABLE issues ADD COLUMN triage_state TEXT NOT NULL DEFAULT 'needs-triage'`); err != nil {
			return err
		}
	}
	return nil
}

func migrateV4(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE issues ADD COLUMN branch TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateV5 adds the local change log and the per-Device sync state
// (decision 0005). Existing rows are not backfilled: the Ledger starts from a
// snapshot written when a Device connects.
func migrateV5(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS changes (
  seq     INTEGER PRIMARY KEY,
  kind    TEXT NOT NULL,
  project TEXT NOT NULL,
  key     TEXT NOT NULL,
  deleted INTEGER NOT NULL DEFAULT 0,
  updated TEXT NOT NULL,
  state   TEXT,
  pushed  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS changes_pending ON changes(pushed, seq);

CREATE TABLE IF NOT EXISTS sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`)
	return err
}

// migrateV6 stamps Label and Link rows with their own updated and records
// their removals, so each set-like row converges by last-writer-wins on its
// own (ITO-52). Legacy rows get the empty stamp: any pulled Change beats them.
func migrateV6(tx *sql.Tx) error {
	for _, table := range []string{"issue_labels", "issue_links"} {
		has, err := columnExists(tx, table, "updated")
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN updated TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS set_tombstones (
  kind       TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  key        TEXT NOT NULL,
  updated    TEXT NOT NULL,
  PRIMARY KEY (kind, project_id, key)
);`)
	return err
}

func columnExists(q rowQuerier, table, column string) (bool, error) {
	rows, err := q.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// CanonicalPath resolves a path to its absolute, symlink-free form, falling
// back to the cleaned absolute path when symlinks cannot be resolved. It is
// exported so the CLI canonicalizes roots with the exact rule the store uses
// to compare them.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	return filepath.Clean(abs), nil
}

// NormalizeProjectName derives a valid Project name from arbitrary input
// (a directory basename), folding accents and squeezing separators to dashes.
// It is exported so the CLI derives names with the exact rule the store uses.
func NormalizeProjectName(input string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(TransliterateASCII(input)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "ito"
	}
	if len(name) == 1 {
		name += "0"
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
		if len(name) < 2 {
			name = "ito"
		}
	}
	return name
}

func nextGeneratedPrefixTx(tx *sql.Tx, input string) (string, error) {
	base := generatedPrefixBase(input)
	for i := 0; i < 1000; i++ {
		candidate := base
		if i > 0 {
			suffix := fmt.Sprintf("%d", i+1)
			stem := base
			if len(stem)+len(suffix) > 8 {
				stem = stem[:8-len(suffix)]
			}
			candidate = stem + suffix
		}
		exists, err := valueExists(tx, `SELECT 1 FROM projects WHERE prefix = ?`, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", errors.New("no prefix available")
}

func generatedPrefixBase(input string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(TransliterateASCII(input)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	prefix := b.String()
	if prefix == "" || prefix[0] < 'A' || prefix[0] > 'Z' {
		prefix = "ITO" + prefix
	}
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	if len(prefix) == 1 {
		prefix += "0"
	}
	return prefix
}

// findProjectWhere runs a single-Project lookup, mapping sql.ErrNoRows to a
// plain not-found instead of an error.
func findProjectWhere(q rowQuerier, query string, args ...any) (Project, bool, error) {
	var p Project
	if err := q.QueryRow(query, args...).Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
		return Project{}, false, err
	}
	return p, true, nil
}

func findProjectByRoot(db *sql.DB, rootPath string) (Project, bool, error) {
	return findProjectWhere(db, `SELECT id, name, prefix, root_path FROM projects WHERE root_path = ?`, rootPath)
}

func findProjectByName(db rowQuerier, name string) (Project, bool, error) {
	return findProjectWhere(db, `SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
}

func findProjectByPrefix(db rowQuerier, prefix string) (Project, bool, error) {
	return findProjectWhere(db, `SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, prefix)
}

func listProjects(db rowQuerier) ([]Project, error) {
	rows, err := db.Query(`SELECT id, name, prefix, root_path FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projects, nil
}

func findClosestProjectAncestor(db *sql.DB, cwd string) (Project, bool, error) {
	rows, err := db.Query(`SELECT id, name, prefix, root_path FROM projects WHERE root_path IS NOT NULL`)
	if err != nil {
		return Project{}, false, err
	}
	defer rows.Close()

	var best Project
	bestLen := -1
	for rows.Next() {
		var candidate Project
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.Prefix, &candidate.RootPath); err != nil {
			return Project{}, false, err
		}
		if candidate.RootPath == nil {
			continue
		}
		if isPathAncestor(*candidate.RootPath, cwd) && len(*candidate.RootPath) > bestLen {
			best = candidate
			bestLen = len(*candidate.RootPath)
		}
	}
	if err := rows.Err(); err != nil {
		return Project{}, false, err
	}
	if bestLen == -1 {
		return Project{}, false, nil
	}
	return best, true, nil
}

// findDetachedProjectByName reports whether a same-named Project exists whose
// stored root_path no longer matches the current resolved root. Detachment is a
// path-identity question, not an on-disk-existence one: a Project is detached
// when its root_path is NULL or, once canonicalized the same way callers
// canonicalize the current root, points somewhere other than currentRoot. This
// is a pure read; it never mutates the store (the NULL-clearing on the moved-repo
// path lives on the init write path, in runInit's detached branch).
func findDetachedProjectByName(db *sql.DB, name string, currentRoot string) (Project, bool, error) {
	p, found, err := findProjectWhere(db, `SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	if err != nil || !found {
		return Project{}, false, err
	}
	if p.RootPath == nil {
		return p, true, nil
	}
	// Canonicalize the stored root with the same EvalSymlinks-based rule the
	// callers apply to the current root, so the comparison is consistent. A
	// canonicalize error (ENOTDIR, EACCES, stale NFS, gone) means we cannot
	// confirm the stored root still names the current root, which is itself a
	// detached signal rather than an aborting error.
	storedRoot, err := CanonicalPath(*p.RootPath)
	if err != nil || storedRoot != currentRoot {
		return p, true, nil
	}
	return Project{}, false, nil
}

func isPathAncestor(root, child string) bool {
	if root == child {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// insertProject checks name/prefix uniqueness and inserts inside a single
// IMMEDIATE transaction, so two concurrent inits cannot both pass the checks
// and surface a raw constraint error — the loser gets a typed error instead.
func insertProject(db *sql.DB, name, prefix, rootPath string) (Project, error) {
	tx, err := db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()

	if exists, err := valueExists(tx, `SELECT 1 FROM projects WHERE name = ?`, name); err != nil {
		return Project{}, err
	} else if exists {
		return Project{}, ErrNameExists
	}
	if exists, err := valueExists(tx, `SELECT 1 FROM projects WHERE prefix = ?`, prefix); err != nil {
		return Project{}, err
	} else if exists {
		return Project{}, ErrPrefixExists
	}
	result, err := tx.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, ?)`, name, prefix, rootPath)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	if err := logProjectChangeTx(tx, name, prefix); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Prefix: prefix, RootPath: &rootPath}, nil
}

// insertProjectWithGeneratedPrefix derives an auto-generated prefix and inserts
// the Project inside a single IMMEDIATE transaction, so two concurrent inits
// deriving the same base prefix cannot both pass the uniqueness check and race
// on the INSERT. The returned prefix reflects the deterministic auto-suffix.
func insertProjectWithGeneratedPrefix(db *sql.DB, name, baseInput, rootPath string) (Project, error) {
	tx, err := db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()

	if exists, err := valueExists(tx, `SELECT 1 FROM projects WHERE name = ?`, name); err != nil {
		return Project{}, err
	} else if exists {
		return Project{}, ErrNameExists
	}
	prefix, err := nextGeneratedPrefixTx(tx, baseInput)
	if err != nil {
		return Project{}, err
	}
	result, err := tx.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, ?)`, name, prefix, rootPath)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	if err := logProjectChangeTx(tx, name, prefix); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Prefix: prefix, RootPath: &rootPath}, nil
}

func insertBatch(db *sql.DB, p Project, name string) (Batch, error) {
	tx, err := db.Begin()
	if err != nil {
		return Batch{}, err
	}
	defer tx.Rollback()

	if exists, err := valueExists(tx, `SELECT 1 FROM batches WHERE project_id = ? AND name = ?`, p.ID, name); err != nil {
		return Batch{}, err
	} else if exists {
		return Batch{}, ErrBatchExists
	}
	created := nowStamp()
	if _, err := tx.Exec(`INSERT INTO batches(project_id, name, created) VALUES (?, ?, ?)`, p.ID, name, created); err != nil {
		return Batch{}, err
	}
	if err := logBatchChangeTx(tx, p, name, batchState{Created: created}); err != nil {
		return Batch{}, err
	}
	if err := tx.Commit(); err != nil {
		return Batch{}, err
	}
	return Batch{Name: name, Project: p.Name, Created: created, Total: 0, Done: 0}, nil
}

// batchProgressSQL selects a Batch row with its done/total progress; callers
// slot their WHERE clause between it and batchProgressGroupSQL.
const batchProgressSQL = `
SELECT batches.id, batches.name, projects.name, batches.created,
       count(issues.row_id) AS total,
       count(CASE WHEN issues.status = 'done' THEN 1 END) AS done
FROM batches
JOIN projects ON projects.id = batches.project_id
LEFT JOIN issues ON issues.batch_id = batches.id
`

const batchProgressGroupSQL = `
GROUP BY batches.id, batches.name, projects.name, batches.created`

// scanBatchProgress reads a batchProgressSQL row from a row or rows cursor.
func scanBatchProgress(s interface{ Scan(dest ...any) error }) (Batch, int64, error) {
	var batch Batch
	var batchID int64
	err := s.Scan(&batchID, &batch.Name, &batch.Project, &batch.Created, &batch.Total, &batch.Done)
	return batch, batchID, err
}

func listBatches(db *sql.DB, p Project) ([]Batch, error) {
	rows, err := db.Query(batchProgressSQL+
		`WHERE batches.project_id = ?`+batchProgressGroupSQL+`
ORDER BY batches.created DESC, batches.id DESC`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	batches := []Batch{}
	for rows.Next() {
		batch, _, err := scanBatchProgress(rows)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return batches, nil
}

func renameBatch(db *sql.DB, p Project, oldName, newName string) (Batch, error) {
	tx, err := db.Begin()
	if err != nil {
		return Batch{}, err
	}
	defer tx.Rollback()

	id, found, err := findBatchID(tx, p.ID, oldName)
	if err != nil {
		return Batch{}, err
	}
	if !found {
		return Batch{}, ErrBatchNotFound
	}
	if exists, err := valueExists(tx, `SELECT 1 FROM batches WHERE project_id = ? AND name = ? AND id != ?`, p.ID, newName, id); err != nil {
		return Batch{}, err
	} else if exists {
		return Batch{}, ErrBatchExists
	}
	result, err := tx.Exec(`UPDATE batches SET name = ? WHERE id = ?`, newName, id)
	if err != nil {
		return Batch{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Batch{}, err
	}
	if affected != 1 {
		return Batch{}, sql.ErrNoRows
	}
	renamed, found, err := findBatchByIDTx(tx, id)
	if err != nil {
		return Batch{}, err
	}
	if !found {
		return Batch{}, sql.ErrNoRows
	}
	if err := logBatchChangeTx(tx, p, newName, batchState{Created: renamed.Created, RenamedFrom: oldName}); err != nil {
		return Batch{}, err
	}
	if err := tx.Commit(); err != nil {
		return Batch{}, err
	}
	return renamed, nil
}

func deleteBatch(db *sql.DB, p Project, name string) (DeleteBatchResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return DeleteBatchResult{}, err
	}
	defer tx.Rollback()

	id, found, err := findBatchID(tx, p.ID, name)
	if err != nil {
		return DeleteBatchResult{}, err
	}
	if !found {
		return DeleteBatchResult{}, ErrBatchNotFound
	}
	members, err := stringColumn(tx, `SELECT id FROM issues WHERE project_id = ? AND batch_id = ? ORDER BY row_id`, p.ID, id)
	if err != nil {
		return DeleteBatchResult{}, err
	}
	now := nowStamp()
	result, err := tx.Exec(`UPDATE issues SET batch_id = NULL, updated = ? WHERE project_id = ? AND batch_id = ?`, now, p.ID, id)
	if err != nil {
		return DeleteBatchResult{}, err
	}
	membersCleared, err := result.RowsAffected()
	if err != nil {
		return DeleteBatchResult{}, err
	}
	if err := logIssueChangesTx(tx, p, members); err != nil {
		return DeleteBatchResult{}, err
	}
	if err := logBatchTombstoneTx(tx, p, name, now); err != nil {
		return DeleteBatchResult{}, err
	}
	result, err = tx.Exec(`DELETE FROM batches WHERE project_id = ? AND id = ?`, p.ID, id)
	if err != nil {
		return DeleteBatchResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return DeleteBatchResult{}, err
	}
	if affected != 1 {
		return DeleteBatchResult{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return DeleteBatchResult{}, err
	}
	return DeleteBatchResult{Name: name, MembersCleared: int(membersCleared)}, nil
}

func showBatch(db *sql.DB, p Project, name string, opts ShowBatchOptions) (BatchPlan, error) {
	batch, batchID, members, err := queryBatchMembers(db, p, name)
	if err != nil {
		return BatchPlan{}, err
	}
	// A Batch with nothing left to place derives to an empty plan, so stop
	// before the relation read rather than pay for it and throw the result
	// away — the shape every completed Batch has on every listing and refresh.
	if batch.Total == 0 || (!opts.IncludeDone && batch.Complete()) {
		return BatchPlan{Batch: batch, Waves: []BatchWave{}, Waiting: []Issue{}}, nil
	}
	graph, err := loadBatchIssueRelations(db, p.ID, batchID, members)
	if err != nil {
		return BatchPlan{}, err
	}
	waves, waiting, err := deriveBatchWaves(members, graph, opts)
	if err != nil {
		return BatchPlan{}, err
	}
	markBatchWaveReadiness(waves)
	return BatchPlan{Batch: batch, Waves: waves, Waiting: waiting}, nil
}

func batchWaveDone(issues []Issue) bool {
	if len(issues) == 0 {
		return false
	}
	for _, issue := range issues {
		if issue.Status != "done" {
			return false
		}
	}
	return true
}

func markBatchWaveReadiness(waves []BatchWave) {
	for i := range waves {
		if !waves[i].Done {
			waves[i].Ready = true
			return
		}
	}
}

// queryBatchMembers reads the Batch, its progress and its ordered members in
// one statement. The LEFT JOIN keeps an empty Batch findable — it comes back as
// a single row whose Issue columns are blank.
func queryBatchMembers(db *sql.DB, p Project, name string) (Batch, int64, []listedIssue, error) {
	rows, err := db.Query(`
SELECT batches.id, batches.name, projects.name, batches.created,
       COALESCE(issues.id, ''), COALESCE(issues.title, ''),
       COALESCE(issues.status, ''), COALESCE(issues.priority, ''),
       COALESCE(issues.category, ''), COALESCE(issues.triage_state, ''),
       COALESCE(issues.branch, ''), COALESCE(issues.body, ''),
       COALESCE(issues.created, ''), COALESCE(issues.updated, '')
FROM batches
JOIN projects ON projects.id = batches.project_id
LEFT JOIN issues ON issues.batch_id = batches.id
WHERE batches.project_id = ? AND batches.name = ?
ORDER BY `+priorityOrderSQL+`,
issues.updated DESC,
issues.id ASC`, p.ID, name)
	if err != nil {
		return Batch{}, 0, nil, err
	}
	defer rows.Close()

	var batch Batch
	var batchID int64
	members := []listedIssue{}
	foundBatch := false
	for rows.Next() {
		foundBatch = true
		found := Issue{
			Labels:        []string{},
			BlockedBy:     []string{},
			RelatesTo:     []string{},
			ConflictsWith: []string{},
		}
		if err := rows.Scan(
			&batchID, &batch.Name, &batch.Project, &batch.Created,
			&found.ID, &found.Title, &found.Status, &found.Priority,
			&found.Category, &found.TriageState, &found.Branch,
			&found.Body, &found.Created, &found.Updated,
		); err != nil {
			return Batch{}, 0, nil, err
		}
		if found.ID == "" {
			continue
		}
		found.Project = batch.Project
		batchName := batch.Name
		found.Batch = &batchName
		members = append(members, listedIssue{Issue: found, ProjectID: p.ID})
		batch.Total++
		if found.Status == "done" {
			batch.Done++
		}
	}
	if err := rows.Err(); err != nil {
		return Batch{}, 0, nil, err
	}
	if !foundBatch {
		return Batch{}, 0, nil, ErrBatchNotFound
	}
	return batch, batchID, members, nil
}

// batchGraphIssue is what the readiness rules need to know about an Issue the
// derivation touches — a member, or an external blocker or conflict partner.
// InBatch is what separates the two: a member's completion is decided by the
// derivation (has an earlier Wave placed it?), an outsider's by its status.
type batchGraphIssue struct {
	Status   string
	Priority string
	InBatch  bool
}

type batchIssueGraph struct {
	issues        map[string]batchGraphIssue
	blockedBy     map[string][]string
	conflictsWith map[string][]string
}

// loadBatchIssueRelations hydrates every member's relations in two queries and,
// alongside them, the status and Batch membership of the external blockers and
// conflict partners the readiness rules consult. It scopes its own queries to
// the Batch but distributes the rows through the same helpers as the aggregate
// list loader.
func loadBatchIssueRelations(q rowQuerier, projectID, batchID int64, members []listedIssue) (batchIssueGraph, error) {
	graph := batchIssueGraph{
		issues:        make(map[string]batchGraphIssue),
		blockedBy:     make(map[string][]string),
		conflictsWith: make(map[string][]string),
	}
	if len(members) == 0 {
		return graph, nil
	}

	memberIndexes := make(map[issueRelationKey]int, len(members))
	memberArgs := make([]any, 0, len(members)+1)
	memberArgs = append(memberArgs, projectID)
	for i := range members {
		memberIndexes[issueRelationKey{ProjectID: projectID, IssueID: members[i].ID}] = i
		memberArgs = append(memberArgs, members[i].ID)
		graph.issues[members[i].ID] = batchGraphIssue{
			Status:   members[i].Status,
			Priority: members[i].Priority,
			InBatch:  true,
		}
	}

	// Scoped to the member IDs already in hand rather than joined back to
	// issues: the join reads every label row in the Project to keep a handful.
	if err := forEachRow(q, `
SELECT issue_id, label
FROM issue_labels
WHERE project_id = ? AND issue_id IN (`+sqlPlaceholders(len(members))+`)
ORDER BY issue_id, label`, memberArgs, func(rows *sql.Rows) error {
		var issueID, label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return err
		}
		if i, ok := memberIndexes[issueRelationKey{ProjectID: projectID, IssueID: issueID}]; ok {
			members[i].Labels = append(members[i].Labels, label)
		}
		return nil
	}); err != nil {
		return batchIssueGraph{}, err
	}

	if err := forEachRow(q, `
WITH batch_members AS (
  SELECT id
  FROM issues
  WHERE project_id = ? AND batch_id = ?
),
conflict_partners AS (
  SELECT CASE
    WHEN source_id IN (SELECT id FROM batch_members) THEN target_id
    ELSE source_id
  END AS id
  FROM issue_links
  WHERE project_id = ?
    AND kind = 'conflicts_with'
    AND (
      source_id IN (SELECT id FROM batch_members)
      OR target_id IN (SELECT id FROM batch_members)
    )
)
SELECT source.id, source.status, source.priority, source.batch_id,
       target.id, target.status, target.priority, target.batch_id,
       issue_links.kind
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ?
  AND (
    source.id IN (SELECT id FROM batch_members)
    OR (
      issue_links.kind IN ('relates_to', 'conflicts_with')
      AND target.id IN (SELECT id FROM batch_members)
    )
    OR (
      issue_links.kind = 'blocked_by'
      AND source.id IN (SELECT id FROM conflict_partners)
    )
  )
ORDER BY source.id, target.id, issue_links.kind`,
		[]any{projectID, batchID, projectID, projectID}, func(rows *sql.Rows) error {
			var sourceID, targetID, kind string
			var source, target batchGraphIssue
			var sourceBatchID, targetBatchID sql.NullInt64
			if err := rows.Scan(
				&sourceID, &source.Status, &source.Priority, &sourceBatchID,
				&targetID, &target.Status, &target.Priority, &targetBatchID,
				&kind,
			); err != nil {
				return err
			}
			source.InBatch = sourceBatchID.Valid && sourceBatchID.Int64 == batchID
			target.InBatch = targetBatchID.Valid && targetBatchID.Int64 == batchID
			graph.issues[sourceID] = source
			graph.issues[targetID] = target

			switch kind {
			case "blocked_by":
				graph.blockedBy[sourceID] = append(graph.blockedBy[sourceID], targetID)
			case "conflicts_with":
				graph.conflictsWith[sourceID] = append(graph.conflictsWith[sourceID], targetID)
				graph.conflictsWith[targetID] = append(graph.conflictsWith[targetID], sourceID)
			}
			distributeIssueLink(members, memberIndexes, projectID, sourceID, targetID, kind)
			return nil
		}); err != nil {
		return batchIssueGraph{}, err
	}

	sortListedIssueRelations(members)
	return graph, nil
}

func deriveBatchWaves(members []listedIssue, graph batchIssueGraph, opts ShowBatchOptions) ([]BatchWave, []Issue, error) {
	remaining := make(map[string]bool, len(members))
	for _, member := range members {
		if opts.IncludeDone || member.Status != "done" {
			remaining[member.ID] = true
		}
	}

	placed := make(map[string]bool, len(remaining))
	waves := []BatchWave{}
	for len(remaining) > 0 {
		issues := []Issue{}
		for _, member := range members {
			if remaining[member.ID] && batchIssueReady(member.ID, placed, graph, opts.IncludeDone) {
				issues = append(issues, member.Issue)
			}
		}
		if len(issues) == 0 {
			cycle := findBatchBlockedByCycle(remaining, graph)
			if len(cycle) > 0 {
				return nil, nil, &BatchCycleError{Issues: cycle}
			}
			waiting := make([]Issue, 0, len(remaining))
			for _, member := range members {
				if remaining[member.ID] {
					waiting = append(waiting, member.Issue)
				}
			}
			return waves, waiting, nil
		}

		waves = append(waves, BatchWave{
			Wave:   len(waves) + 1,
			Done:   batchWaveDone(issues),
			Issues: issues,
		})
		for _, issue := range issues {
			delete(remaining, issue.ID)
			placed[issue.ID] = true
		}
	}
	return waves, []Issue{}, nil
}

func batchIssueReady(id string, placed map[string]bool, graph batchIssueGraph, includeDone bool) bool {
	if !batchIssueOtherwiseReady(id, placed, graph, includeDone) {
		return false
	}
	for _, partnerID := range graph.conflictsWith[id] {
		partner := graph.issues[partnerID]
		if (partner.Status == "in_progress" || partner.Status == "in_review") &&
			!batchIssueDone(partnerID, placed, graph, includeDone) {
			return false
		}
		if batchIssueOtherwiseReady(partnerID, placed, graph, includeDone) &&
			batchIssueBeats(partnerID, id, graph) {
			return false
		}
	}
	return true
}

func batchIssueOtherwiseReady(id string, placed map[string]bool, graph batchIssueGraph, includeDone bool) bool {
	if batchIssueDone(id, placed, graph, includeDone) {
		return false
	}
	for _, blockerID := range graph.blockedBy[id] {
		if !batchIssueDone(blockerID, placed, graph, includeDone) {
			return false
		}
	}
	return true
}

func batchIssueDone(id string, placed map[string]bool, graph batchIssueGraph, includeDone bool) bool {
	issue := graph.issues[id]
	if includeDone {
		// Replaying history: a member counts as done once an earlier Wave placed
		// it, whatever its status says today; an outsider still counts by status.
		if issue.InBatch {
			return placed[id]
		}
		return issue.Status == "done"
	}
	return issue.Status == "done" || placed[id]
}

func batchIssueBeats(leftID, rightID string, graph batchIssueGraph) bool {
	leftRank := batchPriorityRank(graph.issues[leftID].Priority)
	rightRank := batchPriorityRank(graph.issues[rightID].Priority)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	return issueIDLess(leftID, rightID)
}

// batchPriorityRank mirrors orderCase's ranking so a client-side tie-break and
// the SQL ORDER BY agree, unknown priorities included.
func batchPriorityRank(priority string) int {
	if i := slices.Index(Priorities, priority); i >= 0 {
		return i + 1
	}
	return unknownOrderRank
}

func findBatchBlockedByCycle(remaining map[string]bool, graph batchIssueGraph) []string {
	cycleGraph := make(map[string][]string)
	nodes := make(map[string]struct{})
	for sourceID := range remaining {
		for _, targetID := range graph.blockedBy[sourceID] {
			if !remaining[targetID] {
				continue
			}
			cycleGraph[sourceID] = append(cycleGraph[sourceID], targetID)
			nodes[sourceID] = struct{}{}
			nodes[targetID] = struct{}{}
		}
		sort.Strings(cycleGraph[sourceID])
	}

	visited := map[string]bool{}
	inStack := map[string]int{}
	stack := []string{}
	var visit func(string) []string
	visit = func(id string) []string {
		visited[id] = true
		inStack[id] = len(stack)
		stack = append(stack, id)
		for _, next := range cycleGraph[id] {
			if index, ok := inStack[next]; ok {
				cycle := append([]string{}, stack[index:]...)
				return sortIssueIDs(cycle)
			}
			if !visited[next] {
				if cycle := visit(next); len(cycle) > 0 {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		delete(inStack, id)
		return nil
	}

	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sortIssueIDs(ids)
	for _, id := range ids {
		if !visited[id] {
			if cycle := visit(id); len(cycle) > 0 {
				return cycle
			}
		}
	}
	return nil
}

func updateProjectRoot(db *sql.DB, p Project) (Project, error) {
	result, err := db.Exec(`UPDATE projects SET root_path = ? WHERE id = ?`, p.RootPath, p.ID)
	if err != nil {
		return Project{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Project{}, err
	}
	if affected != 1 {
		return Project{}, sql.ErrNoRows
	}
	return p, nil
}

func updateProjectName(db *sql.DB, p Project, name string) (Project, error) {
	tx, err := db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, p.ID)
	if err != nil {
		return Project{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Project{}, err
	}
	if affected != 1 {
		return Project{}, sql.ErrNoRows
	}
	if err := logProjectChangeTx(tx, name, p.Prefix); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	p.Name = name
	return p, nil
}

// insertIssue reserves the number before opening the transaction: a refused
// reservation leaves no row and no Change behind. The local counter is
// advisory once a numberer is set — it only tracks the highest number seen.
func insertIssue(db *sql.DB, numberer IssueNumberer, p Project, title, status, priority, category, triageState string, labels []string, body string, batch string) (Issue, error) {
	// Everything that can refuse the creation runs before the reservation, so
	// a refusal never burns a number.
	var batchID sql.NullInt64
	if batch != "" {
		id, found, err := findBatchID(db, p.ID, batch)
		if err != nil {
			return Issue{}, err
		}
		if !found {
			return Issue{}, ErrBatchNotFound
		}
		batchID = sql.NullInt64{Int64: id, Valid: true}
	}
	var reserved int64
	if numberer != nil {
		var floor int64
		if err := db.QueryRow(`SELECT last_id FROM projects WHERE id = ?`, p.ID).Scan(&floor); err != nil {
			return Issue{}, err
		}
		number, err := numberer.ReserveIssueNumber(p.Prefix, floor)
		if err != nil {
			return Issue{}, &ReservationError{Err: err}
		}
		if number <= 0 {
			return Issue{}, &ReservationError{Err: fmt.Errorf("the Ledger handed out %d", number)}
		}
		reserved = number
	}
	tx, err := db.Begin()
	if err != nil {
		return Issue{}, err
	}
	defer tx.Rollback()

	// A reserved number only lifts the advisory counter; a local one advances
	// it and is the number.
	nextID := reserved
	counter := `UPDATE projects SET last_id = max(last_id, ?) WHERE id = ? RETURNING last_id`
	args := []any{reserved, p.ID}
	if numberer == nil {
		counter = `UPDATE projects SET last_id = last_id + 1 WHERE id = ? RETURNING last_id`
		args = []any{p.ID}
	}
	if err := tx.QueryRow(counter, args...).Scan(&nextID); err != nil {
		return Issue{}, err
	}
	now := nowStamp()
	created := Issue{
		ID:            fmt.Sprintf("%s-%d", p.Prefix, nextID),
		Project:       p.Name,
		Title:         title,
		Status:        status,
		Priority:      priority,
		Category:      category,
		TriageState:   triageState,
		Labels:        dedupe(labels),
		BlockedBy:     []string{},
		RelatesTo:     []string{},
		ConflictsWith: []string{},
		Batch:         nullableString(batch),
		Body:          body,
		Created:       now,
		Updated:       now,
	}

	result, err := tx.Exec(`
INSERT INTO issues(project_id, id, title, status, priority, category, triage_state, branch, body, created, updated, batch_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, created.ID, created.Title, created.Status, created.Priority, created.Category, created.TriageState, created.Branch, created.Body, created.Created, created.Updated, batchID)
	if err != nil {
		return Issue{}, err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return Issue{}, err
	}
	if err := insertIssueFTSTx(tx, rowID, created.Title, created.Body); err != nil {
		return Issue{}, err
	}
	if err := logIssueChangesTx(tx, p, []string{created.ID}); err != nil {
		return Issue{}, err
	}
	for _, label := range created.Labels {
		if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label, updated) VALUES (?, ?, ?, ?)`, p.ID, created.ID, label, now); err != nil {
			return Issue{}, err
		}
		if err := logLabelChangeTx(tx, p, created.ID, label, false, now); err != nil {
			return Issue{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, err
	}
	return created, nil
}

// moveIssueStatus moves an Issue and reads the resulting object inside the same
// transaction, so the returned Issue reflects exactly this write — never a
// concurrent writer's state between commit and a separate re-read.
func moveIssueStatus(db *sql.DB, p Project, id string, targetStatus string) (Issue, string, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return Issue{}, "", false, err
	}
	defer tx.Rollback()

	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM issues WHERE project_id = ? AND id = ?`, p.ID, id).Scan(&currentStatus); err != nil {
		return Issue{}, "", false, err
	}
	changed := currentStatus != targetStatus
	if changed {
		now := nowStamp()
		result, err := tx.Exec(`UPDATE issues SET status = ?, updated = ? WHERE project_id = ? AND id = ?`, targetStatus, now, p.ID, id)
		if err != nil {
			return Issue{}, "", false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return Issue{}, "", false, err
		}
		if affected != 1 {
			return Issue{}, "", false, sql.ErrNoRows
		}
		if err := logIssueChangesTx(tx, p, []string{id}); err != nil {
			return Issue{}, "", false, err
		}
	}
	moved, found, err := findIssueByID(tx, p, id)
	if err != nil {
		return Issue{}, "", false, err
	}
	if !found {
		return Issue{}, "", false, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, "", false, err
	}
	return moved, currentStatus, changed, nil
}

func moveBatchStatus(db *sql.DB, p Project, name string, targetStatus string) (BatchMoveResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return BatchMoveResult{}, err
	}
	defer tx.Rollback()

	batchID, found, err := findBatchID(tx, p.ID, name)
	if err != nil {
		return BatchMoveResult{}, err
	}
	if !found {
		return BatchMoveResult{}, ErrBatchNotFound
	}

	rows, err := tx.Query(`
SELECT id, status
FROM issues
WHERE project_id = ? AND batch_id = ?
ORDER BY row_id ASC`, p.ID, batchID)
	if err != nil {
		return BatchMoveResult{}, err
	}

	changed := []string{}
	skipped := []string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return BatchMoveResult{}, err
		}
		if status == targetStatus {
			skipped = append(skipped, id)
			continue
		}
		changed = append(changed, id)
	}
	if err := rows.Close(); err != nil {
		return BatchMoveResult{}, err
	}
	if err := rows.Err(); err != nil {
		return BatchMoveResult{}, err
	}

	// One bulk write moves exactly the members the scan classified as changed —
	// the WHERE re-selects the same rows inside this transaction's snapshot, so
	// RowsAffected must match. The skipped members already hold targetStatus and
	// keep their updated stamp.
	now := nowStamp()
	result, err := tx.Exec(`UPDATE issues SET status = ?, updated = ? WHERE project_id = ? AND batch_id = ? AND status != ?`,
		targetStatus, now, p.ID, batchID, targetStatus)
	if err != nil {
		return BatchMoveResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return BatchMoveResult{}, err
	}
	if int(affected) != len(changed) {
		return BatchMoveResult{}, sql.ErrNoRows
	}
	if err := logIssueChangesTx(tx, p, changed); err != nil {
		return BatchMoveResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return BatchMoveResult{}, err
	}
	return BatchMoveResult{
		Batch:        name,
		Project:      p.Name,
		TargetStatus: targetStatus,
		Changed:      changed,
		Skipped:      skipped,
	}, nil
}

// editIssue applies the edit and reads the resulting object inside the same
// transaction — see moveIssueStatus for why.
func editIssue(db *sql.DB, p Project, id string, options EditIssueOptions) (Issue, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return Issue{}, false, err
	}
	defer tx.Rollback()

	var rowID int64
	var currentTitle, currentPriority, currentCategory, currentTriageState, currentBranch, currentBody string
	var currentBatchID sql.NullInt64
	if err := tx.QueryRow(`
SELECT row_id, title, priority, category, triage_state, branch, body, batch_id
FROM issues
WHERE project_id = ? AND id = ?`, p.ID, id).Scan(&rowID, &currentTitle, &currentPriority, &currentCategory, &currentTriageState, &currentBranch, &currentBody, &currentBatchID); err != nil {
		return Issue{}, false, err
	}

	currentLabels, err := stringColumn(tx, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, p.ID, id)
	if err != nil {
		return Issue{}, false, err
	}
	nextLabels := make(map[string]struct{}, len(currentLabels))
	for _, label := range currentLabels {
		nextLabels[label] = struct{}{}
	}
	for _, op := range options.LabelOps {
		switch op.Kind {
		case "add":
			nextLabels[op.Label] = struct{}{}
		case "remove":
			delete(nextLabels, op.Label)
		}
	}
	currentLinks, err := linkSetTx(tx, p.ID, id)
	if err != nil {
		return Issue{}, false, err
	}
	nextLinks := make(map[string]struct{}, len(currentLinks))
	for linkKey := range currentLinks {
		nextLinks[linkKey] = struct{}{}
	}
	for _, op := range options.LinkOps {
		targetProject, found, err := findProjectByIssueIDTx(tx, op.Target)
		if err != nil {
			return Issue{}, false, err
		}
		if !found {
			return Issue{}, false, &LinkTargetNotFoundError{TargetID: op.Target}
		}
		if targetProject.ID != p.ID {
			return Issue{}, false, &CrossProjectError{IssueID: op.Target, SourceProject: p.Name, TargetProject: targetProject.Name}
		}
		if exists, err := issueExistsTx(tx, p.ID, op.Target); err != nil {
			return Issue{}, false, err
		} else if !exists {
			return Issue{}, false, &LinkTargetNotFoundError{TargetID: op.Target}
		}
		sourceID, targetID := normalizedLinkIDs(id, op.Target, op.Kind)
		linkKey := issueLinkKey(sourceID, targetID, op.Kind)
		switch op.Action {
		case "add":
			nextLinks[linkKey] = struct{}{}
		case "remove":
			delete(nextLinks, linkKey)
		}
	}

	nextTitle := currentTitle
	if options.TitleSet {
		nextTitle = options.Title
	}
	nextPriority := currentPriority
	if options.PrioritySet {
		nextPriority = options.Priority
	}
	nextCategory := currentCategory
	if options.CategorySet {
		nextCategory = options.Category
	}
	nextTriageState := currentTriageState
	if options.TriageStateSet {
		nextTriageState = options.TriageState
	}
	nextBranch := currentBranch
	if options.BranchSet {
		nextBranch = options.Branch
	}
	nextBody := currentBody
	if options.BodySet {
		nextBody = options.Body
	}
	nextBatchID := currentBatchID
	if options.BatchSet {
		if options.Batch == "" {
			nextBatchID = sql.NullInt64{}
		} else {
			id, found, err := findBatchID(tx, p.ID, options.Batch)
			if err != nil {
				return Issue{}, false, err
			}
			if !found {
				return Issue{}, false, ErrBatchNotFound
			}
			nextBatchID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	scalarChanged := nextTitle != currentTitle || nextPriority != currentPriority || nextCategory != currentCategory || nextTriageState != currentTriageState || nextBranch != currentBranch || nextBody != currentBody
	batchChanged := nextBatchID.Valid != currentBatchID.Valid || (nextBatchID.Valid && nextBatchID.Int64 != currentBatchID.Int64)
	labelsChanged := !labelSetMatches(currentLabels, nextLabels)
	linksChanged := !stringSetMatches(currentLinks, nextLinks)
	changed := scalarChanged || batchChanged || labelsChanged || linksChanged

	if changed {
		now := nowStamp()
		result, err := tx.Exec(`
UPDATE issues
SET title = ?, priority = ?, category = ?, triage_state = ?, branch = ?, body = ?, batch_id = ?, updated = ?
WHERE project_id = ? AND id = ?`, nextTitle, nextPriority, nextCategory, nextTriageState, nextBranch, nextBody, nextBatchID, now, p.ID, id)
		if err != nil {
			return Issue{}, false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return Issue{}, false, err
		}
		if affected != 1 {
			return Issue{}, false, sql.ErrNoRows
		}
		if err := reindexIssueFTSTx(tx, rowID, currentTitle, currentBody, nextTitle, nextBody); err != nil {
			return Issue{}, false, err
		}
		if err := logIssueChangesTx(tx, p, []string{id}); err != nil {
			return Issue{}, false, err
		}
		// Every row of a rewritten set is re-stamped and re-logged, removals as
		// tombstones: an edit that merely re-affirms a row still outranks an
		// older removal made elsewhere.
		if labelsChanged {
			if _, err := tx.Exec(`DELETE FROM issue_labels WHERE project_id = ? AND issue_id = ?`, p.ID, id); err != nil {
				return Issue{}, false, err
			}
			for label := range nextLabels {
				if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label, updated) VALUES (?, ?, ?, ?)`, p.ID, id, label, now); err != nil {
					return Issue{}, false, err
				}
				if err := logLabelChangeTx(tx, p, id, label, false, now); err != nil {
					return Issue{}, false, err
				}
			}
			for _, label := range currentLabels {
				if _, kept := nextLabels[label]; !kept {
					if err := tombstoneSetRowTx(tx, ledger.KindLabel, p.ID, labelChangeKey(id, label), now); err != nil {
						return Issue{}, false, err
					}
					if err := logLabelChangeTx(tx, p, id, label, true, now); err != nil {
						return Issue{}, false, err
					}
				}
			}
		}
		if linksChanged {
			if _, err := tx.Exec(`DELETE FROM issue_links WHERE project_id = ? AND (source_id = ? OR target_id = ?)`, p.ID, id, id); err != nil {
				return Issue{}, false, err
			}
			for linkKey := range nextLinks {
				sourceID, targetID, kind := parseIssueLinkKey(linkKey)
				if _, err := tx.Exec(`INSERT INTO issue_links(project_id, source_id, target_id, kind, updated) VALUES (?, ?, ?, ?, ?)`, p.ID, sourceID, targetID, kind, now); err != nil {
					return Issue{}, false, err
				}
				if err := logLinkChangeTx(tx, p, linkKey, false, now); err != nil {
					return Issue{}, false, err
				}
			}
			for linkKey := range currentLinks {
				if _, kept := nextLinks[linkKey]; !kept {
					if err := tombstoneSetRowTx(tx, ledger.KindLink, p.ID, linkKey, now); err != nil {
						return Issue{}, false, err
					}
					if err := logLinkChangeTx(tx, p, linkKey, true, now); err != nil {
						return Issue{}, false, err
					}
				}
			}
		}
	}

	edited, found, err := findIssueByID(tx, p, id)
	if err != nil {
		return Issue{}, false, err
	}
	if !found {
		return Issue{}, false, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, false, err
	}
	return edited, changed, nil
}

func deleteIssue(db *sql.DB, p Project, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var row issueDeletionRow
	if err := tx.QueryRow(`
SELECT row_id, id, title, body
FROM issues
WHERE project_id = ? AND id = ?`, p.ID, id).Scan(&row.rowID, &row.id, &row.title, &row.body); err != nil {
		return err
	}
	if _, err := deleteIssueRowsTx(tx, p, []issueDeletionRow{row}); err != nil {
		return err
	}
	if err := logIssueTombstonesTx(tx, p, []string{row.id}); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteIssuesByStatus(db *sql.DB, p Project, status string) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
SELECT row_id, id, title, body
FROM issues
WHERE project_id = ? AND status = ?
ORDER BY row_id`, p.ID, status)
	if err != nil {
		return 0, err
	}
	matches := []issueDeletionRow{}
	for rows.Next() {
		var row issueDeletionRow
		if err := rows.Scan(&row.rowID, &row.id, &row.title, &row.body); err != nil {
			rows.Close()
			return 0, err
		}
		matches = append(matches, row)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	deleted, err := deleteIssueRowsTx(tx, p, matches)
	if err != nil {
		return 0, err
	}
	if err := logIssueTombstonesTx(tx, p, issueDeletionIDs(matches)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// dropIssueFTSTx removes an Issue's row from the external-content FTS index;
// the index has no triggers, so every write path maintains it explicitly.
func dropIssueFTSTx(tx *sql.Tx, rowID int64, title, body string) error {
	_, err := tx.Exec(`INSERT INTO issues_fts(issues_fts, rowid, title, body) VALUES ('delete', ?, ?, ?)`, rowID, title, body)
	return err
}

func insertIssueFTSTx(tx *sql.Tx, rowID int64, title, body string) error {
	_, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, title, body)
	return err
}

// reindexIssueFTSTx re-points the index at the new text, skipping the work
// when neither indexed column moved.
func reindexIssueFTSTx(tx *sql.Tx, rowID int64, oldTitle, oldBody, newTitle, newBody string) error {
	if newTitle == oldTitle && newBody == oldBody {
		return nil
	}
	if err := dropIssueFTSTx(tx, rowID, oldTitle, oldBody); err != nil {
		return err
	}
	return insertIssueFTSTx(tx, rowID, newTitle, newBody)
}

func issueDeletionIDs(matches []issueDeletionRow) []string {
	ids := make([]string, len(matches))
	for i, match := range matches {
		ids[i] = match.id
	}
	return ids
}

func deleteIssueRowsTx(tx *sql.Tx, p Project, matches []issueDeletionRow) (int, error) {
	for _, match := range matches {
		if err := dropIssueFTSTx(tx, match.rowID, match.title, match.body); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM issue_labels WHERE project_id = ? AND issue_id = ?`, p.ID, match.id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM issue_links WHERE project_id = ? AND (source_id = ? OR target_id = ?)`, p.ID, match.id, match.id); err != nil {
			return 0, err
		}
		result, err := tx.Exec(`DELETE FROM issues WHERE project_id = ? AND id = ?`, p.ID, match.id)
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if affected != 1 {
			return 0, sql.ErrNoRows
		}
	}
	return len(matches), nil
}

func findIssueByID(q rowQuerier, p Project, id string) (Issue, bool, error) {
	row := q.QueryRow(`
SELECT issues.id, projects.name, issues.title, issues.status, issues.priority, issues.category, issues.triage_state, issues.branch, batches.name, issues.body, issues.created, issues.updated
FROM issues
JOIN projects ON projects.id = issues.project_id
LEFT JOIN batches ON batches.id = issues.batch_id
WHERE issues.project_id = ? AND issues.id = ?`, p.ID, id)
	found, err := scanIssue(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, false, nil
		}
		return Issue{}, false, err
	}

	if err := loadIssueRelations(q, p.ID, &found); err != nil {
		return Issue{}, false, err
	}
	return found, true, nil
}

// scanIssue reads an issue column list — id, project name, title, status,
// priority, category, triage state, branch, batch name, body, created, updated — from a
// row or rows cursor, leaving relations for loadIssueRelations. Queries that
// project the same columns in a different order scan them themselves.
func scanIssue(s interface{ Scan(dest ...any) error }) (Issue, error) {
	found := Issue{
		Labels:        []string{},
		BlockedBy:     []string{},
		RelatesTo:     []string{},
		ConflictsWith: []string{},
	}
	var batch sql.NullString
	if err := s.Scan(&found.ID, &found.Project, &found.Title, &found.Status, &found.Priority, &found.Category, &found.TriageState, &found.Branch, &batch, &found.Body, &found.Created, &found.Updated); err != nil {
		return Issue{}, err
	}
	if batch.Valid {
		found.Batch = &batch.String
	}
	return found, nil
}

// loadIssueRelations fills an Issue's Labels and Links from the label and link
// tables, with the link IDs in canonical order.
func loadIssueRelations(q rowQuerier, projectID int64, issue *Issue) error {
	labels, err := stringColumn(q, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, projectID, issue.ID)
	if err != nil {
		return err
	}
	blockedBy, err := stringColumn(q, `
SELECT target_id
FROM issue_links
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND source_id = ? AND kind = 'blocked_by'
ORDER BY target_id`, projectID, issue.ID)
	if err != nil {
		return err
	}
	relatesTo, err := stringColumn(q, `
SELECT CASE WHEN source_id = ? THEN target_id ELSE source_id END
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND kind = 'relates_to' AND (source_id = ? OR target_id = ?)
ORDER BY 1`, issue.ID, projectID, issue.ID, issue.ID)
	if err != nil {
		return err
	}
	conflictsWith, err := stringColumn(q, `
SELECT CASE WHEN source_id = ? THEN target_id ELSE source_id END
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND kind = 'conflicts_with' AND (source_id = ? OR target_id = ?)
ORDER BY 1`, issue.ID, projectID, issue.ID, issue.ID)
	if err != nil {
		return err
	}
	issue.Labels = labels
	issue.BlockedBy = sortIssueIDs(blockedBy)
	issue.RelatesTo = sortIssueIDs(relatesTo)
	issue.ConflictsWith = sortIssueIDs(conflictsWith)
	return nil
}

type listedIssue struct {
	Issue
	ProjectID int64
}

type issueRelationKey struct {
	ProjectID int64
	IssueID   string
}

// loadListedIssueRelations hydrates every listed Issue with one labels query
// and one links query. Link rows are distributed by the composite project/Issue
// key so all-project listings cannot mix relations between Projects.
func loadListedIssueRelations(q rowQuerier, issues []listedIssue) error {
	if len(issues) == 0 {
		return nil
	}

	issueIndexes := make(map[issueRelationKey]int, len(issues))
	args := make([]any, 0, len(issues))
	for i := range issues {
		issues[i].Labels = []string{}
		issues[i].BlockedBy = []string{}
		issues[i].RelatesTo = []string{}
		issues[i].ConflictsWith = []string{}

		issueIndexes[issueRelationKey{ProjectID: issues[i].ProjectID, IssueID: issues[i].ID}] = i
		args = append(args, issues[i].ProjectID)
	}
	args = dedupe(args)
	placeholders := sqlPlaceholders(len(args))

	if err := forEachRow(q, `
SELECT project_id, issue_id, label
FROM issue_labels
WHERE project_id IN (`+placeholders+`)
ORDER BY project_id, issue_id, label`, args, func(rows *sql.Rows) error {
		var projectID int64
		var issueID, label string
		if err := rows.Scan(&projectID, &issueID, &label); err != nil {
			return err
		}
		if i, ok := issueIndexes[issueRelationKey{ProjectID: projectID, IssueID: issueID}]; ok {
			issues[i].Labels = append(issues[i].Labels, label)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := forEachRow(q, `
SELECT issue_links.project_id, source_id, target_id, kind
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id IN (`+placeholders+`)
  AND kind IN ('blocked_by', 'relates_to', 'conflicts_with')`, args, func(rows *sql.Rows) error {
		var projectID int64
		var sourceID, targetID, kind string
		if err := rows.Scan(&projectID, &sourceID, &targetID, &kind); err != nil {
			return err
		}
		distributeIssueLink(issues, issueIndexes, projectID, sourceID, targetID, kind)
		return nil
	}); err != nil {
		return err
	}

	sortListedIssueRelations(issues)
	return nil
}

// distributeIssueLink hands one link row to whichever listed Issues it touches.
// relates_to and conflicts_with are symmetric, so each endpoint carries the
// other; blocked_by only ever hangs off its source. Every loader distributes
// rows through here, so the Batch, list and detail surfaces cannot disagree
// about what an Issue is linked to.
func distributeIssueLink(issues []listedIssue, indexes map[issueRelationKey]int, projectID int64, sourceID, targetID, kind string) {
	sourceIndex, sourceListed := indexes[issueRelationKey{ProjectID: projectID, IssueID: sourceID}]
	targetIndex, targetListed := indexes[issueRelationKey{ProjectID: projectID, IssueID: targetID}]
	switch kind {
	case "blocked_by":
		if sourceListed {
			issues[sourceIndex].BlockedBy = append(issues[sourceIndex].BlockedBy, targetID)
		}
	case "relates_to":
		if sourceListed {
			issues[sourceIndex].RelatesTo = append(issues[sourceIndex].RelatesTo, targetID)
		}
		if targetListed {
			issues[targetIndex].RelatesTo = append(issues[targetIndex].RelatesTo, sourceID)
		}
	case "conflicts_with":
		if sourceListed {
			issues[sourceIndex].ConflictsWith = append(issues[sourceIndex].ConflictsWith, targetID)
		}
		if targetListed {
			issues[targetIndex].ConflictsWith = append(issues[targetIndex].ConflictsWith, sourceID)
		}
	}
}

// sortListedIssueRelations puts every hydrated relation in the canonical Issue
// ID order the surfaces render.
func sortListedIssueRelations(issues []listedIssue) {
	for i := range issues {
		issues[i].BlockedBy = sortIssueIDs(issues[i].BlockedBy)
		issues[i].RelatesTo = sortIssueIDs(issues[i].RelatesTo)
		issues[i].ConflictsWith = sortIssueIDs(issues[i].ConflictsWith)
	}
}

func listIssues(db *sql.DB, options ListOptions) ([]Issue, error) {
	where := []string{}
	args := []any{}
	ftsQuery := searchQuery(options.Search)
	searching := strings.TrimSpace(options.Search) != ""
	if searching {
		if ftsQuery == "" {
			return []Issue{}, nil
		}
		where = append(where, "issues_fts MATCH ?")
		args = append(args, ftsQuery)
	}
	if !options.AllProjects {
		where = append(where, "issues.project_id = ?")
		args = append(args, options.ProjectID)
	}
	if options.Status != "" {
		where = append(where, "issues.status = ?")
		args = append(args, options.Status)
	} else if !options.IncludeDone {
		where = append(where, "issues.status != 'done'")
	}
	if options.Priority != "" {
		where = append(where, "issues.priority = ?")
		args = append(args, options.Priority)
	}
	if options.Category != "" {
		where = append(where, "issues.category = ?")
		args = append(args, options.Category)
	}
	if options.TriageState != "" {
		where = append(where, "issues.triage_state = ?")
		args = append(args, options.TriageState)
	}
	if options.Batch != "" {
		if options.AllProjects {
			return nil, ErrBatchNotFound
		}
		batchID, found, err := findBatchID(db, options.ProjectID, options.Batch)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrBatchNotFound
		}
		where = append(where, "issues.batch_id = ?")
		args = append(args, batchID)
	}
	for _, label := range options.Labels {
		where = append(where, `EXISTS (
SELECT 1 FROM issue_labels
WHERE issue_labels.project_id = issues.project_id
  AND issue_labels.issue_id = issues.id
  AND issue_labels.label = ?
)`)
		args = append(args, label)
	}
	if options.Ready {
		where = append(where, readyFrontierWhereSQL("issues"))
	}

	query := `
SELECT issues.id, projects.name, issues.title, issues.status, issues.priority, issues.category, issues.triage_state, issues.branch, batches.name, issues.body, issues.created, issues.updated, issues.project_id
FROM issues
JOIN projects ON projects.id = issues.project_id
LEFT JOIN batches ON batches.id = issues.batch_id`
	if searching {
		query += `
JOIN issues_fts ON issues_fts.rowid = issues.row_id`
	}
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += `
ORDER BY `
	if options.AllProjects {
		query += "projects.name ASC, "
	}
	if searching {
		query += "bm25(issues_fts), "
	}
	query += statusOrderSQL + ",\n" + priorityOrderSQL + `,
issues.updated DESC,
issues.id ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rowIssues := []listedIssue{}
	for rows.Next() {
		found := listedIssue{
			Issue: Issue{
				Labels:        []string{},
				BlockedBy:     []string{},
				RelatesTo:     []string{},
				ConflictsWith: []string{},
			},
		}
		var batch sql.NullString
		if err := rows.Scan(&found.ID, &found.Project, &found.Title, &found.Status, &found.Priority, &found.Category, &found.TriageState, &found.Branch, &batch, &found.Body, &found.Created, &found.Updated, &found.ProjectID); err != nil {
			return nil, err
		}
		if batch.Valid {
			found.Batch = &batch.String
		}
		rowIssues = append(rowIssues, found)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := loadListedIssueRelations(db, rowIssues); err != nil {
		return nil, err
	}

	issues := make([]Issue, 0, len(rowIssues))
	for _, rowIssue := range rowIssues {
		issues = append(issues, rowIssue.Issue)
	}
	return issues, nil
}

func readyFrontierWhereSQL(issueAlias string) string {
	return otherwiseReadyWhereSQL(issueAlias) + `
AND NOT EXISTS (
SELECT 1 FROM issue_links AS in_flight_conflict
JOIN issues AS in_flight_partner ON in_flight_partner.project_id = in_flight_conflict.project_id
  AND in_flight_partner.id = CASE
    WHEN in_flight_conflict.source_id = ` + issueAlias + `.id THEN in_flight_conflict.target_id
    ELSE in_flight_conflict.source_id
  END
WHERE in_flight_conflict.project_id = ` + issueAlias + `.project_id
  AND in_flight_conflict.kind = 'conflicts_with'
  AND (in_flight_conflict.source_id = ` + issueAlias + `.id OR in_flight_conflict.target_id = ` + issueAlias + `.id)
  AND in_flight_partner.status IN ('in_progress', 'in_review')
  AND in_flight_partner.status != 'done'
)
AND NOT EXISTS (
SELECT 1 FROM issue_links AS ready_conflict
JOIN issues AS ready_partner ON ready_partner.project_id = ready_conflict.project_id
  AND ready_partner.id = CASE
    WHEN ready_conflict.source_id = ` + issueAlias + `.id THEN ready_conflict.target_id
    ELSE ready_conflict.source_id
  END
WHERE ready_conflict.project_id = ` + issueAlias + `.project_id
  AND ready_conflict.kind = 'conflicts_with'
  AND (ready_conflict.source_id = ` + issueAlias + `.id OR ready_conflict.target_id = ` + issueAlias + `.id)
  AND ` + otherwiseReadyWhereSQL("ready_partner") + `
  AND ` + readyConflictPartnerBeatsSQL("ready_partner", issueAlias) + `
)`
}

func otherwiseReadyWhereSQL(issueAlias string) string {
	blockerAlias := issueAlias + "_blocker"
	return issueAlias + `.status IN ('backlog', 'todo')
AND NOT EXISTS (
SELECT 1 FROM issue_links AS ` + blockerAlias + `_link
JOIN issues AS ` + blockerAlias + ` ON ` + blockerAlias + `.project_id = ` + blockerAlias + `_link.project_id AND ` + blockerAlias + `.id = ` + blockerAlias + `_link.target_id
WHERE ` + blockerAlias + `_link.project_id = ` + issueAlias + `.project_id
  AND ` + blockerAlias + `_link.source_id = ` + issueAlias + `.id
  AND ` + blockerAlias + `_link.kind = 'blocked_by'
  AND ` + blockerAlias + `.status != 'done'
)`
}

func readyConflictPartnerBeatsSQL(partnerAlias, issueAlias string) string {
	return `(` + priorityRankSQL(partnerAlias) + ` < ` + priorityRankSQL(issueAlias) + `
  OR (` + priorityRankSQL(partnerAlias) + ` = ` + priorityRankSQL(issueAlias) + ` AND ` + issueNumberSQL(partnerAlias) + ` < ` + issueNumberSQL(issueAlias) + `)
)`
}

func priorityRankSQL(issueAlias string) string {
	return orderCase(issueAlias+".priority", Priorities)
}

func issueNumberSQL(issueAlias string) string {
	return `CAST(substr(` + issueAlias + `.id, instr(` + issueAlias + `.id, '-') + 1) AS INTEGER)`
}

// rowQuerier is the read surface shared by *sql.DB and *sql.Tx, so helpers can
// run against either a connection or an open transaction without duplication.
type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// forEachRow runs a query and hands every row to scan, owning the cursor's
// lifetime so callers are left with the scan body and nothing else.
func forEachRow(q rowQuerier, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func stringColumn(q rowQuerier, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func findBatchByIDTx(tx *sql.Tx, id int64) (Batch, bool, error) {
	row := tx.QueryRow(batchProgressSQL+`WHERE batches.id = ?`+batchProgressGroupSQL, id)
	batch, _, err := scanBatchProgress(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Batch{}, false, nil
		}
		return Batch{}, false, err
	}
	return batch, true, nil
}

func findBatchID(q rowQuerier, projectID int64, name string) (int64, bool, error) {
	var id int64
	if err := q.QueryRow(`SELECT id FROM batches WHERE project_id = ? AND name = ?`, projectID, name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return id, true, nil
}

func linkSetTx(tx *sql.Tx, projectID int64, issueID string) (map[string]struct{}, error) {
	rows, err := tx.Query(`
SELECT source_id, target_id, kind
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND (source_id = ? OR target_id = ?)`, projectID, issueID, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	links := map[string]struct{}{}
	for rows.Next() {
		var sourceID, targetID, kind string
		if err := rows.Scan(&sourceID, &targetID, &kind); err != nil {
			return nil, err
		}
		links[issueLinkKey(sourceID, targetID, kind)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

func findProjectByIssueIDTx(tx *sql.Tx, issueID string) (Project, bool, error) {
	matches := IssueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return Project{}, false, nil
	}
	return findProjectWhere(tx, `SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, matches[1])
}

func issueExistsTx(tx *sql.Tx, projectID int64, issueID string) (bool, error) {
	return valueExists(tx, `SELECT 1 FROM issues WHERE project_id = ? AND id = ?`, projectID, issueID)
}

func normalizedLinkIDs(sourceID, targetID, kind string) (string, string) {
	if (kind == "relates_to" || kind == "conflicts_with") && issueIDLess(targetID, sourceID) {
		return targetID, sourceID
	}
	return sourceID, targetID
}

func sortIssueIDs(ids []string) []string {
	sort.SliceStable(ids, func(i, j int) bool {
		return issueIDLess(ids[i], ids[j])
	})
	return ids
}

func issueIDLess(left, right string) bool {
	leftMatches := IssueIDPattern.FindStringSubmatch(left)
	rightMatches := IssueIDPattern.FindStringSubmatch(right)
	if leftMatches == nil || rightMatches == nil || leftMatches[1] != rightMatches[1] {
		return left < right
	}
	leftNumber, _ := strconv.Atoi(leftMatches[2])
	rightNumber, _ := strconv.Atoi(rightMatches[2])
	return leftNumber < rightNumber
}

// A Link's key is the same string in the store and in its Change: Issue IDs
// and Link kinds are drawn from patterns that never contain the separator.
func issueLinkKey(sourceID, targetID, kind string) string {
	return sourceID + "|" + targetID + "|" + kind
}

func parseIssueLinkKey(key string) (string, string, string) {
	parts := strings.SplitN(key, "|", 3)
	return parts[0], parts[1], parts[2]
}

func labelSetMatches(labels []string, set map[string]struct{}) bool {
	if len(labels) != len(set) {
		return false
	}
	for _, label := range labels {
		if _, ok := set[label]; !ok {
			return false
		}
	}
	return true
}

func stringSetMatches(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}

func projectNameExistsForAnotherID(db rowQuerier, name string, id int64) (bool, error) {
	return valueExists(db, `SELECT 1 FROM projects WHERE name = ? AND id != ?`, name, id)
}

func valueExists(q rowQuerier, query string, args ...any) (bool, error) {
	var value int
	err := q.QueryRow(query, args...).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// dedupe returns a new slice with duplicates removed, preserving the first
// occurrence's order. Always non-nil.
func dedupe[T comparable](values []T) []T {
	seen := make(map[T]struct{}, len(values))
	deduped := make([]T, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		deduped = append(deduped, value)
	}
	return deduped
}

// sqlPlaceholders renders the "?, ?, ?" list an IN clause binds n values with.
func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
