package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	exitGeneric       = 1
	exitBadUsage      = 2
	exitNotRegistered = 4
)

var (
	projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	prefixPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,7}$`)
)

type project struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	RootPath string `json:"root_path"`
}

type cliError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Hint  string `json:"hint"`
}

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ito <command> [flags]")
		return exitBadUsage
	}

	switch args[0] {
	case "init":
		return runInit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		return exitBadUsage
	}
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var manualName string
	var manualPrefix string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&manualName, "name", "", "")
	fs.StringVar(&manualPrefix, "prefix", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args), exitBadUsage, err.Error(), "run 'ito init --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 {
		return fail(jsonMode, exitBadUsage, "ito init takes no positional arguments.", "remove the positional arguments and use flags like --name, --prefix or --json.")
	}

	rootPath, inGit, err := resolveCurrentRoot()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory.")
	}

	name := manualName
	if name == "" {
		name = normalizeProjectName(filepath.Base(rootPath))
	}
	if !projectNamePattern.MatchString(name) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", name), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	db, err := openStore()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not open the central store: %v", err), "check ITO_HOME and the directory permissions.")
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not migrate the central store: %v", err), "check that the ito.db file is a valid SQLite database.")
	}

	existing, found, err := findProjectByRoot(db, rootPath)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the current project: %v", err), "try again or inspect the central store.")
	}
	if found {
		return printProject(existing, jsonMode)
	}
	if !inGit {
		existing, found, err := findClosestProjectAncestor(db, rootPath)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the ancestor project: %v", err), "try again or inspect the central store.")
		}
		if found {
			return printProject(existing, jsonMode)
		}
	}

	if exists, err := valueExists(db, `SELECT 1 FROM projects WHERE name = ?`, name); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project name: %v", err), "try again or inspect the central store.")
	} else if exists {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", name), "choose another one with --name.")
	}

	prefix := manualPrefix
	if prefix != "" {
		if !prefixPattern.MatchString(prefix) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid prefix %q.", prefix), "use the format [A-Z][A-Z0-9]{1,7}.")
		}
		if exists, err := valueExists(db, `SELECT 1 FROM projects WHERE prefix = ?`, prefix); err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project prefix: %v", err), "try again or inspect the central store.")
		} else if exists {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project with prefix %q already exists.", prefix), "choose another one with --prefix.")
		}
	} else {
		prefix, err = nextGeneratedPrefix(db, filepath.Base(rootPath))
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not generate a prefix for the project: %v", err), "choose a manual prefix with --prefix.")
		}
	}

	created, err := insertProject(db, name, prefix, rootPath)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not register the project: %v", err), "check for name, prefix or root_path collisions.")
	}
	return printProject(created, jsonMode)
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			return true
		}
	}
	return false
}

func fail(jsonMode bool, code int, message string, hint string) int {
	if jsonMode {
		encoded, err := json.Marshal(cliError{Error: message, Code: code, Hint: hint})
		if err == nil {
			fmt.Fprintln(os.Stderr, string(encoded))
			return code
		}
	}
	if hint == "" {
		fmt.Fprintln(os.Stderr, message)
	} else {
		fmt.Fprintf(os.Stderr, "%s %s\n", message, hint)
	}
	return code
}

func printProject(p project, jsonMode bool) int {
	if jsonMode {
		encoded, err := json.Marshal(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the project: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	fmt.Printf("%s %s %s\n", p.Name, p.Prefix, p.RootPath)
	return 0
}

func openStore() (*sql.DB, error) {
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
	db, err := sql.Open("sqlite", filepath.Join(home, "ito.db"))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
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

func resolveCurrentRoot() (string, bool, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := cmd.Output()
	if err == nil {
		commonDir := strings.TrimSpace(string(output))
		if filepath.Base(commonDir) == ".git" {
			root, err := canonicalPath(filepath.Dir(commonDir))
			return root, true, err
		}
		root, err := canonicalPath(commonDir)
		return root, true, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	root, err := canonicalPath(cwd)
	return root, false, err
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
	for _, r := range strings.ToLower(input) {
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

func nextGeneratedPrefix(db *sql.DB, input string) (string, error) {
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
		exists, err := valueExists(db, `SELECT 1 FROM projects WHERE prefix = ?`, candidate)
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
	for _, r := range strings.ToUpper(input) {
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

func findProjectByRoot(db *sql.DB, rootPath string) (project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE root_path = ?`, rootPath)
	var p project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project{}, false, nil
		}
		return project{}, false, err
	}
	return p, true, nil
}

func findClosestProjectAncestor(db *sql.DB, cwd string) (project, bool, error) {
	rows, err := db.Query(`SELECT id, name, prefix, root_path FROM projects WHERE root_path IS NOT NULL`)
	if err != nil {
		return project{}, false, err
	}
	defer rows.Close()

	var best project
	bestLen := -1
	for rows.Next() {
		var candidate project
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.Prefix, &candidate.RootPath); err != nil {
			return project{}, false, err
		}
		if isPathAncestor(candidate.RootPath, cwd) && len(candidate.RootPath) > bestLen {
			best = candidate
			bestLen = len(candidate.RootPath)
		}
	}
	if err := rows.Err(); err != nil {
		return project{}, false, err
	}
	if bestLen == -1 {
		return project{}, false, nil
	}
	return best, true, nil
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

func insertProject(db *sql.DB, name, prefix, rootPath string) (project, error) {
	result, err := db.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, ?)`, name, prefix, rootPath)
	if err != nil {
		return project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return project{}, err
	}
	return project{ID: id, Name: name, Prefix: prefix, RootPath: rootPath}, nil
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
