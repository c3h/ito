package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

var (
	projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	prefixPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,7}$`)
	issueIDPattern     = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,7})-([1-9][0-9]*)$`)
	searchTermPattern  = regexp.MustCompile(`[\pL\pN]+`)
	validStatuses      = map[string]struct{}{"backlog": {}, "todo": {}, "in_progress": {}, "in_review": {}, "done": {}}
	validPriorities    = map[string]struct{}{"low": {}, "medium": {}, "high": {}, "urgent": {}}
	validLabels        = map[string]struct{}{"feature": {}, "bug": {}, "docs": {}, "tests": {}, "refactor": {}, "chore": {}, "research": {}, "infra": {}}
	// asciiFold decomposes (NFD) and strips combining marks, transliterating
	// Latin accents to ASCII (café → cafe). Scripts with no decomposition to
	// ASCII (Cyrillic, CJK) pass through intact and are discarded downstream.
	asciiFold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
)

var (
	ErrNotRegistered = errors.New("project not registered")
	ErrDetached      = errors.New("project detached")
	ErrNotFound      = errors.New("not found")
	ErrCrossProject  = errors.New("cross-project link")
)

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

type Project struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Prefix   string  `json:"prefix"`
	RootPath *string `json:"root_path"`
}

type Issue struct {
	ID        string   `json:"id"`
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels"`
	BlockedBy []string `json:"blocked_by"`
	RelatesTo []string `json:"relates_to"`
	Body      string   `json:"body"`
	Created   string   `json:"created"`
	Updated   string   `json:"updated"`
}

type ListOptions struct {
	ProjectID   int64
	AllProjects bool
	Status      string
	Priority    string
	Labels      []string
	Search      string
	Ready       bool
}

type EditIssueOptions struct {
	TitleSet    bool
	Title       string
	PrioritySet bool
	Priority    string
	BodySet     bool
	Body        string
	LabelOps    []LabelEditOp
	LinkOps     []LinkEditOp
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

type EditResult struct {
	Issue   Issue
	Changed bool
}

type Store struct {
	db *sql.DB
}

func Open(home string) (*sql.DB, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		filepath.Join(home, "ito.db"),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func transliterateASCII(input string) string {
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
	return insertIssue(s.db, p, title, status, priority, labels, body)
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
	beforeStatus, changed, err := moveIssueStatus(s.db, p, id, targetStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MoveResult{}, ErrNotFound
		}
		return MoveResult{}, err
	}
	moved, err := s.FindIssue(p, id)
	if err != nil {
		return MoveResult{}, err
	}
	return MoveResult{Issue: moved, BeforeStatus: beforeStatus, Changed: changed}, nil
}

func (s *Store) Edit(p Project, id string, options EditIssueOptions) (EditResult, error) {
	changed, err := editIssue(s.db, p, id, options)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EditResult{}, ErrNotFound
		}
		return EditResult{}, err
	}
	edited, err := s.FindIssue(p, id)
	if err != nil {
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
	name := normalizeProjectName(filepath.Base(rootPath))
	detached, found, err := findDetachedProjectByName(s.db, name, rootPath)
	if err != nil {
		return Project{}, err
	}
	if found {
		return Project{}, &DetachedError{ProjectName: detached.Name}
	}
	return Project{}, ErrNotRegistered
}

func OpenDefault() (*sql.DB, error) {
	home := os.Getenv("ITO_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".ito")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	// _txlock=immediate makes db.Begin() emit BEGIN IMMEDIATE so write
	// transactions take the write lock up front, avoiding lock-upgrade
	// deadlocks (read-then-write would otherwise fail with SQLITE_BUSY).
	// WAL improves read/write concurrency. The _pragma settings apply to
	// every pooled connection, unlike a single post-open db.Exec.
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		filepath.Join(home, "ito.db"),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);

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
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO schema_version(version)
SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_version)`)
	return err
}

func canonicalPath(path string) (string, error) {
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

func normalizeProjectName(input string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(transliterateASCII(input)) {
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
		exists, err := valueExistsTx(tx, `SELECT 1 FROM projects WHERE prefix = ?`, candidate)
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
	for _, r := range strings.ToUpper(transliterateASCII(input)) {
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

func findProjectByRoot(db *sql.DB, rootPath string) (Project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE root_path = ?`, rootPath)
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
		return Project{}, false, err
	}
	return p, true, nil
}

func findProjectByName(db *sql.DB, name string) (Project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
		return Project{}, false, err
	}
	return p, true, nil
}

func findProjectByPrefix(db *sql.DB, prefix string) (Project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, prefix)
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
		return Project{}, false, err
	}
	return p, true, nil
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
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
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
	storedRoot, err := canonicalPath(*p.RootPath)
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

func insertProject(db *sql.DB, name, prefix, rootPath string) (Project, error) {
	result, err := db.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, ?)`, name, prefix, rootPath)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
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
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Prefix: prefix, RootPath: &rootPath}, nil
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
	result, err := db.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, p.ID)
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
	p.Name = name
	return p, nil
}

func insertIssue(db *sql.DB, p Project, title, status, priority string, labels []string, body string) (Issue, error) {
	tx, err := db.Begin()
	if err != nil {
		return Issue{}, err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`UPDATE projects SET last_id = last_id + 1 WHERE id = ?`, p.ID)
	if err != nil {
		return Issue{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Issue{}, err
	}
	if affected != 1 {
		return Issue{}, sql.ErrNoRows
	}

	var nextID int64
	if err := tx.QueryRow(`SELECT last_id FROM projects WHERE id = ?`, p.ID).Scan(&nextID); err != nil {
		return Issue{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	created := Issue{
		ID:        fmt.Sprintf("%s-%d", p.Prefix, nextID),
		Project:   p.Name,
		Title:     title,
		Status:    status,
		Priority:  priority,
		Labels:    append([]string{}, labels...),
		BlockedBy: []string{},
		RelatesTo: []string{},
		Body:      body,
		Created:   now,
		Updated:   now,
	}
	if created.Labels == nil {
		created.Labels = []string{}
	}

	result, err = tx.Exec(`
INSERT INTO issues(project_id, id, title, status, priority, body, created, updated)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, created.ID, created.Title, created.Status, created.Priority, created.Body, created.Created, created.Updated)
	if err != nil {
		return Issue{}, err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return Issue{}, err
	}
	if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, created.Title, created.Body); err != nil {
		return Issue{}, err
	}
	for _, label := range created.Labels {
		if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label) VALUES (?, ?, ?)`, p.ID, created.ID, label); err != nil {
			return Issue{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Issue{}, err
	}
	return created, nil
}

func moveIssueStatus(db *sql.DB, p Project, id string, targetStatus string) (string, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM issues WHERE project_id = ? AND id = ?`, p.ID, id).Scan(&currentStatus); err != nil {
		return "", false, err
	}
	if currentStatus == targetStatus {
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return currentStatus, false, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.Exec(`UPDATE issues SET status = ?, updated = ? WHERE project_id = ? AND id = ?`, targetStatus, now, p.ID, id)
	if err != nil {
		return "", false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if affected != 1 {
		return "", false, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return currentStatus, true, nil
}

func editIssue(db *sql.DB, p Project, id string, options EditIssueOptions) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var rowID int64
	var currentTitle, currentPriority, currentBody string
	if err := tx.QueryRow(`
SELECT row_id, title, priority, body
FROM issues
WHERE project_id = ? AND id = ?`, p.ID, id).Scan(&rowID, &currentTitle, &currentPriority, &currentBody); err != nil {
		return false, err
	}

	currentLabels, err := stringColumnTx(tx, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, p.ID, id)
	if err != nil {
		return false, err
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
		return false, err
	}
	nextLinks := make(map[string]struct{}, len(currentLinks))
	for linkKey := range currentLinks {
		nextLinks[linkKey] = struct{}{}
	}
	for _, op := range options.LinkOps {
		targetProject, found, err := findProjectByIssueIDTx(tx, op.Target)
		if err != nil {
			return false, err
		}
		if !found {
			return false, ErrNotFound
		}
		if targetProject.ID != p.ID {
			return false, &CrossProjectError{IssueID: op.Target, SourceProject: p.Name, TargetProject: targetProject.Name}
		}
		if exists, err := issueExistsTx(tx, p.ID, op.Target); err != nil {
			return false, err
		} else if !exists {
			return false, ErrNotFound
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
	nextBody := currentBody
	if options.BodySet {
		nextBody = options.Body
	}

	scalarChanged := nextTitle != currentTitle || nextPriority != currentPriority || nextBody != currentBody
	labelsChanged := !labelSetMatches(currentLabels, nextLabels)
	linksChanged := !stringSetMatches(currentLinks, nextLinks)
	changed := scalarChanged || labelsChanged || linksChanged
	if !changed {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.Exec(`
UPDATE issues
SET title = ?, priority = ?, body = ?, updated = ?
WHERE project_id = ? AND id = ?`, nextTitle, nextPriority, nextBody, now, p.ID, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, sql.ErrNoRows
	}
	if nextTitle != currentTitle || nextBody != currentBody {
		if _, err := tx.Exec(`INSERT INTO issues_fts(issues_fts, rowid, title, body) VALUES ('delete', ?, ?, ?)`, rowID, currentTitle, currentBody); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, nextTitle, nextBody); err != nil {
			return false, err
		}
	}
	if labelsChanged {
		if _, err := tx.Exec(`DELETE FROM issue_labels WHERE project_id = ? AND issue_id = ?`, p.ID, id); err != nil {
			return false, err
		}
		for label := range nextLabels {
			if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label) VALUES (?, ?, ?)`, p.ID, id, label); err != nil {
				return false, err
			}
		}
	}
	if linksChanged {
		if _, err := tx.Exec(`DELETE FROM issue_links WHERE project_id = ? AND (source_id = ? OR target_id = ?)`, p.ID, id, id); err != nil {
			return false, err
		}
		for linkKey := range nextLinks {
			sourceID, targetID, kind := parseIssueLinkKey(linkKey)
			if _, err := tx.Exec(`INSERT INTO issue_links(project_id, source_id, target_id, kind) VALUES (?, ?, ?, ?)`, p.ID, sourceID, targetID, kind); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func deleteIssueRowsTx(tx *sql.Tx, p Project, matches []issueDeletionRow) (int, error) {
	for _, match := range matches {
		if _, err := tx.Exec(`INSERT INTO issues_fts(issues_fts, rowid, title, body) VALUES ('delete', ?, ?, ?)`, match.rowID, match.title, match.body); err != nil {
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

func findIssueByID(db *sql.DB, p Project, id string) (Issue, bool, error) {
	row := db.QueryRow(`
SELECT issues.id, projects.name, issues.title, issues.status, issues.priority, issues.body, issues.created, issues.updated
FROM issues
JOIN projects ON projects.id = issues.project_id
WHERE issues.project_id = ? AND issues.id = ?`, p.ID, id)
	found := Issue{
		Labels:    []string{},
		BlockedBy: []string{},
		RelatesTo: []string{},
	}
	if err := row.Scan(&found.ID, &found.Project, &found.Title, &found.Status, &found.Priority, &found.Body, &found.Created, &found.Updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, false, nil
		}
		return Issue{}, false, err
	}

	labels, err := stringColumn(db, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, p.ID, id)
	if err != nil {
		return Issue{}, false, err
	}
	blockedBy, err := stringColumn(db, `
SELECT target_id
FROM issue_links
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND source_id = ? AND kind = 'blocked_by'
ORDER BY target_id`, p.ID, id)
	if err != nil {
		return Issue{}, false, err
	}
	relatesTo, err := stringColumn(db, `
SELECT CASE WHEN source_id = ? THEN target_id ELSE source_id END
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND kind = 'relates_to' AND (source_id = ? OR target_id = ?)
ORDER BY 1`, id, p.ID, id, id)
	if err != nil {
		return Issue{}, false, err
	}
	found.Labels = labels
	found.BlockedBy = sortIssueIDs(blockedBy)
	found.RelatesTo = sortIssueIDs(relatesTo)
	return found, true, nil
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
	} else {
		where = append(where, "issues.status != 'done'")
	}
	if options.Priority != "" {
		where = append(where, "issues.priority = ?")
		args = append(args, options.Priority)
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
		where = append(where, `issues.status IN ('backlog', 'todo')`)
		where = append(where, `NOT EXISTS (
SELECT 1 FROM issue_links
JOIN issues AS blocker ON blocker.project_id = issue_links.project_id AND blocker.id = issue_links.target_id
WHERE issue_links.project_id = issues.project_id
  AND issue_links.source_id = issues.id
  AND issue_links.kind = 'blocked_by'
  AND blocker.status != 'done'
)`)
	}

	query := `
SELECT issues.id, projects.name, issues.title, issues.status, issues.priority, issues.body, issues.created, issues.updated, issues.project_id
FROM issues
JOIN projects ON projects.id = issues.project_id`
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
	query += `CASE issues.status
  WHEN 'backlog' THEN 1
  WHEN 'todo' THEN 2
  WHEN 'in_progress' THEN 3
  WHEN 'in_review' THEN 4
  WHEN 'done' THEN 5
  ELSE 99
END,
CASE issues.priority
  WHEN 'urgent' THEN 1
  WHEN 'high' THEN 2
  WHEN 'medium' THEN 3
  WHEN 'low' THEN 4
  ELSE 99
END,
issues.updated DESC,
issues.id ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rowIssue struct {
		Issue
		ProjectID int64
	}
	rowIssues := []rowIssue{}
	for rows.Next() {
		found := rowIssue{
			Issue: Issue{
				Labels:    []string{},
				BlockedBy: []string{},
				RelatesTo: []string{},
			},
		}
		if err := rows.Scan(&found.ID, &found.Project, &found.Title, &found.Status, &found.Priority, &found.Body, &found.Created, &found.Updated, &found.ProjectID); err != nil {
			return nil, err
		}
		rowIssues = append(rowIssues, found)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	issues := make([]Issue, 0, len(rowIssues))
	for _, rowIssue := range rowIssues {
		labels, err := stringColumn(db, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, rowIssue.ProjectID, rowIssue.ID)
		if err != nil {
			return nil, err
		}
		blockedBy, err := stringColumn(db, `
SELECT target_id
FROM issue_links
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND source_id = ? AND kind = 'blocked_by'
ORDER BY target_id`, rowIssue.ProjectID, rowIssue.ID)
		if err != nil {
			return nil, err
		}
		relatesTo, err := stringColumn(db, `
SELECT CASE WHEN source_id = ? THEN target_id ELSE source_id END
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND kind = 'relates_to' AND (source_id = ? OR target_id = ?)
ORDER BY 1`, rowIssue.ID, rowIssue.ProjectID, rowIssue.ID, rowIssue.ID)
		if err != nil {
			return nil, err
		}
		rowIssue.Labels = labels
		rowIssue.BlockedBy = sortIssueIDs(blockedBy)
		rowIssue.RelatesTo = sortIssueIDs(relatesTo)
		issues = append(issues, rowIssue.Issue)
	}
	return issues, nil
}

func stringColumn(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
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

func stringColumnTx(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
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
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return Project{}, false, nil
	}
	row := tx.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, matches[1])
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, false, nil
		}
		return Project{}, false, err
	}
	return p, true, nil
}

func issueExistsTx(tx *sql.Tx, projectID int64, issueID string) (bool, error) {
	var value int
	err := tx.QueryRow(`SELECT 1 FROM issues WHERE project_id = ? AND id = ?`, projectID, issueID).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func normalizedLinkIDs(sourceID, targetID, kind string) (string, string) {
	if kind == "relates_to" && issueIDLess(targetID, sourceID) {
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
	leftMatches := issueIDPattern.FindStringSubmatch(left)
	rightMatches := issueIDPattern.FindStringSubmatch(right)
	if leftMatches == nil || rightMatches == nil || leftMatches[1] != rightMatches[1] {
		return left < right
	}
	leftNumber, _ := strconv.Atoi(leftMatches[2])
	rightNumber, _ := strconv.Atoi(rightMatches[2])
	return leftNumber < rightNumber
}

func issueLinkKey(sourceID, targetID, kind string) string {
	return sourceID + "\x00" + targetID + "\x00" + kind
}

func parseIssueLinkKey(key string) (string, string, string) {
	parts := strings.SplitN(key, "\x00", 3)
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

func projectNameExistsForAnotherID(db *sql.DB, name string, id int64) (bool, error) {
	var value int
	err := db.QueryRow(`SELECT 1 FROM projects WHERE name = ? AND id != ?`, name, id).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func valueExists(db *sql.DB, query string, arg string) (bool, error) {
	var value int
	err := db.QueryRow(query, arg).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func valueExistsTx(tx *sql.Tx, query string, arg string) (bool, error) {
	var value int
	err := tx.QueryRow(query, arg).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
