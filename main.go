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

const (
	exitGeneric       = 1
	exitBadUsage      = 2
	exitNotFound      = 3
	exitNotRegistered = 4
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

func transliterateASCII(input string) string {
	folded, _, err := transform.String(asciiFold, input)
	if err != nil {
		return input
	}
	return folded
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringSliceFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type labelEditOp struct {
	Kind  string
	Label string
}

type labelEditFlag struct {
	kind string
	ops  *[]labelEditOp
}

func (f labelEditFlag) String() string {
	if f.ops == nil {
		return ""
	}
	labels := make([]string, 0, len(*f.ops))
	for _, op := range *f.ops {
		if op.Kind == f.kind {
			labels = append(labels, op.Label)
		}
	}
	return strings.Join(labels, ",")
}

func (f labelEditFlag) Set(value string) error {
	*f.ops = append(*f.ops, labelEditOp{Kind: f.kind, Label: value})
	return nil
}

type linkEditOp struct {
	Kind   string
	Action string
	Target string
}

type linkEditFlag struct {
	kind   string
	action string
	ops    *[]linkEditOp
}

func (f linkEditFlag) String() string {
	if f.ops == nil {
		return ""
	}
	targets := make([]string, 0, len(*f.ops))
	for _, op := range *f.ops {
		if op.Kind == f.kind && op.Action == f.action {
			targets = append(targets, op.Target)
		}
	}
	return strings.Join(targets, ",")
}

func (f linkEditFlag) Set(value string) error {
	*f.ops = append(*f.ops, linkEditOp{Kind: f.kind, Action: f.action, Target: value})
	return nil
}

type project struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Prefix   string  `json:"prefix"`
	RootPath *string `json:"root_path"`
}

type cliError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Hint  string `json:"hint"`
}

type issue struct {
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

type issueListItem struct {
	ID        string   `json:"id"`
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels"`
	BlockedBy []string `json:"blocked_by"`
	RelatesTo []string `json:"relates_to"`
	Created   string   `json:"created"`
	Updated   string   `json:"updated"`
}

type listOptions struct {
	ProjectID   int64
	AllProjects bool
	Status      string
	Priority    string
	Labels      []string
	Search      string
}

type issueDeletionRow struct {
	rowID int64
	id    string
	title string
	body  string
}

type editIssueOptions struct {
	TitleSet    bool
	Title       string
	PrioritySet bool
	Priority    string
	BodySet     bool
	Body        string
	LabelOps    []labelEditOp
	LinkOps     []linkEditOp
}

type deletedIssue struct {
	Deleted int    `json:"deleted"`
	ID      string `json:"id"`
}

type deletedIssues struct {
	Deleted int `json:"deleted"`
}

type commandFailure struct {
	code    int
	message string
	hint    string
}

func (e commandFailure) Error() string {
	return e.message
}

// openMigratedStore opens the central store and runs the migration, returning a
// ready *sql.DB (the caller defers Close) or a typed *commandFailure carrying
// the exact code/message/hint. On migrate failure it closes the DB first.
func openMigratedStore() (*sql.DB, *commandFailure) {
	db, err := openStore()
	if err != nil {
		return nil, &commandFailure{exitGeneric, fmt.Sprintf("could not open the central store: %v", err), "check ITO_HOME and the directory permissions."}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, &commandFailure{exitGeneric, fmt.Sprintf("could not migrate the central store: %v", err), "check that the ito.db file is a valid SQLite database."}
	}
	return db, nil
}

// resolveIssueProject finds the Project that owns the Issue's Prefix and, when an
// explicit project name is given, validates that it names the same Project. It
// returns the owning Project or a typed *commandFailure the handler renders.
func resolveIssueProject(db *sql.DB, prefix string, projectName string, issueID string) (project, *commandFailure) {
	p, found, err := findProjectByPrefix(db, prefix)
	if err != nil {
		return project{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read the Project for prefix %q: %v", prefix, err), "try again or inspect the central store."}
	}
	if !found {
		return project{}, &commandFailure{exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID."}
	}
	if projectName != "" {
		override, found, err := findProjectByName(db, projectName)
		if err != nil {
			return project{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read project %q: %v", projectName, err), "try again or inspect the central store."}
		}
		if !found {
			return project{}, &commandFailure{exitNotRegistered, fmt.Sprintf("project %q not found.", projectName), "check the registered Project name."}
		}
		if override.ID != p.ID {
			return project{}, &commandFailure{exitBadUsage, fmt.Sprintf("Issue %q belongs to Project %q, not to %q.", issueID, p.Name, override.Name), "remove --project or specify the Project that owns the Prefix."}
		}
	}
	return p, nil
}

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		printRootHelp(os.Stderr)
		return exitBadUsage
	}
	if isHelpArg(args[0]) {
		printRootHelp(os.Stdout)
		return 0
	}

	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "new":
		return runNew(args[1:])
	case "rename":
		return runRename(args[1:])
	case "show":
		return runShow(args[1:])
	case "list":
		return runList(args[1:])
	case "move":
		return runMove(args[1:])
	case "edit":
		return runEdit(args[1:])
	case "rm":
		return runRm(args[1:])
	case "prune":
		return runPrune(args[1:])
	default:
		return fail(wantsJSON(args, nil), exitBadUsage, "unknown command: "+args[0], "run 'ito --help' to see available commands.")
	}
}

func runInit(args []string) int {
	if wantsHelp(args, commandValueFlags("init")) {
		printCommandHelp("init")
		return 0
	}
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var manualName string
	var manualPrefix string
	var reattachName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&manualName, "name", "", "")
	fs.StringVar(&manualPrefix, "prefix", "", "")
	fs.StringVar(&reattachName, "reattach", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args, commandValueFlags("init")), exitBadUsage, err.Error(), "run 'ito init --help' to see the accepted flags.")
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

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	if reattachName != "" {
		return runInitReattach(db, rootPath, reattachName, jsonMode)
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

	// Only the derived-name path surfaces the moved-repo reattach hint. An
	// explicit --name colliding with a live Project is a deliberate name
	// collision (handled below as exit 2), not a detachment.
	if manualName == "" {
		detached, found, err := findDetachedProjectByName(db, name, rootPath)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not search for detached projects: %v", err), "try again or inspect the central store.")
		}
		if found {
			// This is the init write path, so it is the correct place to clear a
			// stale pointer: if the detached Project still records an old root,
			// persist root_path = NULL so the Project serializes as detached.
			if detached.RootPath != nil {
				detached.RootPath = nil
				if _, err := updateProjectRoot(db, detached); err != nil {
					return fail(jsonMode, exitGeneric, fmt.Sprintf("could not clear the stale root for project %q: %v", detached.Name, err), "try again or inspect the central store.")
				}
			}
			return fail(jsonMode, exitNotRegistered, fmt.Sprintf("project %q exists but does not point to this directory.", detached.Name), fmt.Sprintf("run 'ito init --reattach %s' to re-point this Project.", detached.Name))
		}
	}

	if exists, err := valueExists(db, `SELECT 1 FROM projects WHERE name = ?`, name); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project name: %v", err), "try again or inspect the central store.")
	} else if exists {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", name), "choose another one with --name.")
	}

	var created project
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
		created, err = insertProject(db, name, prefix, rootPath)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not register the project: %v", err), "check for name, prefix or root_path collisions.")
		}
	} else {
		created, err = insertProjectWithGeneratedPrefix(db, name, filepath.Base(rootPath), rootPath)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not register the project: %v", err), "check for name, prefix or root_path collisions.")
		}
	}
	return printProject(created, jsonMode)
}

func runRename(args []string) int {
	if wantsHelp(args, commandValueFlags("rename")) {
		printCommandHelp("rename")
		return 0
	}
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("rename")), exitBadUsage, err.Error(), "run 'ito rename --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito rename takes exactly one new name.", "use: ito rename <name>.")
	}
	newName := positionals[0]
	if !projectNamePattern.MatchString(newName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", newName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	rootPath, inGit, err := resolveCurrentRoot()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory.")
	}
	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(db, rootPath, inGit, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	if exists, err := projectNameExistsForAnotherID(db, newName, p.ID); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project name: %v", err), "try again or inspect the central store.")
	} else if exists {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", newName), "choose another name.")
	}
	renamed, err := updateProjectName(db, p, newName)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not rename the project: %v", err), "try again or inspect the central store.")
	}
	return printProject(renamed, jsonMode)
}

func runNew(args []string) int {
	if wantsHelp(args, commandValueFlags("new")) {
		printCommandHelp("new")
		return 0
	}
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	var title string
	var status string
	var priority string
	var labels stringSliceFlag
	var body string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&status, "status", "backlog", "")
	fs.StringVar(&priority, "priority", "low", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&body, "body", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args, commandValueFlags("new")), exitBadUsage, err.Error(), "run 'ito new --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 {
		return fail(jsonMode, exitBadUsage, "ito new takes no positional arguments.", "use --title <title>.")
	}
	if strings.TrimSpace(title) == "" {
		return fail(jsonMode, exitBadUsage, "title is required.", "use --title <title> with non-empty text.")
	}
	if !isValidValue(status, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use backlog, todo, in_progress, in_review or done.")
	}
	if !isValidValue(priority, validPriorities) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use low, medium, high or urgent.")
	}
	for _, label := range labels {
		if !isValidValue(label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", label), "use feature, bug, docs, tests, refactor, chore, research or infra.")
		}
	}
	if body == "-" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the Issue body from stdin: %v", err), "try again by passing --body <text>.")
		}
		body = string(input)
	}

	rootPath, inGit, err := resolveCurrentRoot()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory.")
	}
	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(db, rootPath, inGit, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	created, err := insertIssue(db, p, title, status, priority, labels, body)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not create the Issue: %v", err), "try again or inspect the central store.")
	}
	return printIssue(created, jsonMode)
}

func runShow(args []string) int {
	if wantsHelp(args, commandValueFlags("show")) {
		printCommandHelp("show")
		return 0
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("show")), exitBadUsage, err.Error(), "run 'ito show --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito show takes exactly one full ID.", "use: ito show <PREFIX>-<n>.")
	}
	issueID := positionals[0]
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !projectNamePattern.MatchString(projectName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", projectName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(db, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	foundIssue, found, err := findIssueByID(db, p, issueID)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if !found {
		return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
	}
	return printIssueDetail(foundIssue, jsonMode)
}

func runMove(args []string) int {
	if wantsHelp(args, commandValueFlags("move")) {
		printCommandHelp("move")
		return 0
	}
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("move")), exitBadUsage, err.Error(), "run 'ito move --help' to see the accepted flags.")
	}
	if len(positionals) != 2 {
		return fail(jsonMode, exitBadUsage, "ito move takes exactly one full ID and a target status.", "use: ito move <PREFIX>-<n> <status>.")
	}
	issueID := positionals[0]
	targetStatus := positionals[1]
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	if !isValidValue(targetStatus, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", targetStatus), "use backlog, todo, in_progress, in_review or done.")
	}
	prefix := matches[1]
	if projectName != "" && !projectNamePattern.MatchString(projectName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", projectName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(db, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	beforeStatus, changed, err := moveIssueStatus(db, p, issueID, targetStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not move Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	movedIssue, found, err := findIssueByID(db, p, issueID)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if !found {
		return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
	}
	if jsonMode {
		return printIssueDetail(movedIssue, true)
	}
	if !changed {
		fmt.Printf("%s is already in %s; nothing changed.\n", movedIssue.ID, targetStatus)
		return 0
	}
	fmt.Printf("%s moved from %s to %s.\n", movedIssue.ID, beforeStatus, targetStatus)
	return 0
}

func runEdit(args []string) int {
	if wantsHelp(args, commandValueFlags("edit")) {
		printCommandHelp("edit")
		return 0
	}
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	var title string
	var priority string
	var body string
	var labelOps []labelEditOp
	var linkOps []linkEditOp
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&body, "body", "", "")
	fs.Var(labelEditFlag{kind: "add", ops: &labelOps}, "add-label", "")
	fs.Var(labelEditFlag{kind: "remove", ops: &labelOps}, "remove-label", "")
	fs.Var(linkEditFlag{kind: "blocked_by", action: "add", ops: &linkOps}, "block", "")
	fs.Var(linkEditFlag{kind: "blocked_by", action: "remove", ops: &linkOps}, "unblock", "")
	fs.Var(linkEditFlag{kind: "relates_to", action: "add", ops: &linkOps}, "relate", "")
	fs.Var(linkEditFlag{kind: "relates_to", action: "remove", ops: &linkOps}, "unrelate", "")
	parseArgs, issueID, positionalCount := splitEditArgs(args)
	if err := fs.Parse(parseArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("edit")), exitBadUsage, err.Error(), "run 'ito edit --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 || positionalCount != 1 {
		return fail(jsonMode, exitBadUsage, "ito edit takes exactly one full ID.", "use: ito edit <PREFIX>-<n> [--title <title>] [--priority <priority>] [--body <text>|-] [--block <ID>].")
	}
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !projectNamePattern.MatchString(projectName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", projectName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	options := editIssueOptions{LabelOps: labelOps, LinkOps: linkOps}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "title":
			options.TitleSet = true
		case "priority":
			options.PrioritySet = true
		case "body":
			options.BodySet = true
		}
	})
	if !options.TitleSet && !options.PrioritySet && !options.BodySet && len(options.LabelOps) == 0 && len(options.LinkOps) == 0 {
		return fail(jsonMode, exitBadUsage, "no changes requested.", "use at least one flag like --title, --priority, --body, --add-label, --block or --relate.")
	}
	if options.TitleSet {
		if strings.TrimSpace(title) == "" {
			return fail(jsonMode, exitBadUsage, "title is required.", "use --title <title> with non-empty text.")
		}
		options.Title = title
	}
	if options.PrioritySet {
		if !isValidValue(priority, validPriorities) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use low, medium, high or urgent.")
		}
		options.Priority = priority
	}
	if options.BodySet {
		if body == "-" {
			input, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the Issue body from stdin: %v", err), "try again by passing --body <text>.")
			}
			body = string(input)
		}
		options.Body = body
	}
	for _, op := range options.LabelOps {
		if !isValidValue(op.Label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", op.Label), "use feature, bug, docs, tests, refactor, chore, research or infra.")
		}
	}
	for _, op := range options.LinkOps {
		if !issueIDPattern.MatchString(op.Target) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", op.Target), "use the full format <PREFIX>-<n>, for example AUTH-12.")
		}
		if op.Target == issueID {
			return fail(jsonMode, exitBadUsage, "Links to the Issue itself are not allowed.", "specify an Issue different from the source.")
		}
	}

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(db, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	changed, err := editIssue(db, p, issueID, options)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		var commandErr commandFailure
		if errors.As(err, &commandErr) {
			return fail(jsonMode, commandErr.code, commandErr.message, commandErr.hint)
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not edit Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	editedIssue, found, err := findIssueByID(db, p, issueID)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if !found {
		return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
	}
	if jsonMode {
		return printIssueDetail(editedIssue, true)
	}
	if !changed {
		fmt.Printf("%s did not change; the final state already matched the request.\n", editedIssue.ID)
		return 0
	}
	fmt.Printf("%s edited.\n", editedIssue.ID)
	return 0
}

func runRm(args []string) int {
	if wantsHelp(args, commandValueFlags("rm")) {
		printCommandHelp("rm")
		return 0
	}
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("rm")), exitBadUsage, err.Error(), "run 'ito rm --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito rm takes exactly one full ID.", "use: ito rm <PREFIX>-<n>.")
	}
	issueID := positionals[0]
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !projectNamePattern.MatchString(projectName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", projectName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(db, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	if err := deleteIssue(db, p, issueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not delete Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	deleted := deletedIssue{Deleted: 1, ID: issueID}
	if jsonMode {
		encoded, err := json.Marshal(deleted)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the removal: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	fmt.Printf("%s deleted.\n", issueID)
	return 0
}

func runPrune(args []string) int {
	if wantsHelp(args, commandValueFlags("prune")) {
		printCommandHelp("prune")
		return 0
	}
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	var status string
	var yes bool
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&status, "status", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args, commandValueFlags("prune")), exitBadUsage, err.Error(), "run 'ito prune --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 {
		return fail(jsonMode, exitBadUsage, "ito prune takes no positional arguments.", "use explicit filters like --status <status> and confirm with --yes.")
	}
	if status == "" {
		return fail(jsonMode, exitBadUsage, "explicit filter is required.", "use --status <status> to choose which Issues to delete.")
	}
	if !yes {
		return fail(jsonMode, exitBadUsage, "explicit confirmation is required.", "add --yes to confirm the destructive deletion.")
	}
	if !isValidValue(status, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use backlog, todo, in_progress, in_review or done.")
	}
	if projectName != "" && !projectNamePattern.MatchString(projectName) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", projectName), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}

	rootPath, inGit, err := resolveCurrentRoot()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory.")
	}
	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()
	p, code, message, hint := resolveProject(db, rootPath, inGit, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}

	deleted, err := deleteIssuesByStatus(db, p, status)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not delete Issues with status %q: %v", status, err), "try again or inspect the central store.")
	}
	result := deletedIssues{Deleted: deleted}
	if jsonMode {
		encoded, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the removal: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	fmt.Printf("%d\n", deleted)
	return 0
}

func runList(args []string) int {
	if wantsHelp(args, commandValueFlags("list")) {
		printCommandHelp("list")
		return 0
	}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	var allProjects bool
	var status string
	var priority string
	var search string
	var labels stringSliceFlag
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.BoolVar(&allProjects, "all-projects", false, "")
	fs.StringVar(&status, "status", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&search, "search", "", "")
	fs.Var(&labels, "label", "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args, commandValueFlags("list")), exitBadUsage, err.Error(), "run 'ito list --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 {
		return fail(jsonMode, exitBadUsage, "ito list takes no positional arguments.", "use flags like --status, --label, --priority, --project or --all-projects.")
	}
	if projectName != "" && allProjects {
		return fail(jsonMode, exitBadUsage, "--project and --all-projects cannot be used together.", "choose a scope: an explicit Project or all Projects.")
	}
	if status != "" && !isValidValue(status, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use backlog, todo, in_progress, in_review or done.")
	}
	if priority != "" && !isValidValue(priority, validPriorities) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use low, medium, high or urgent.")
	}
	for _, label := range labels {
		if !isValidValue(label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", label), "use feature, bug, docs, tests, refactor, chore, research or infra.")
		}
	}

	db, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	options := listOptions{
		AllProjects: allProjects,
		Status:      status,
		Priority:    priority,
		Labels:      append([]string{}, labels...),
		Search:      search,
	}
	if !allProjects {
		rootPath, inGit, err := resolveCurrentRoot()
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory.")
		}
		p, code, message, hint := resolveProject(db, rootPath, inGit, projectName)
		if code != 0 {
			return fail(jsonMode, code, message, hint)
		}
		options.ProjectID = p.ID
	}

	issues, err := listIssues(db, options)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not list Issues: %v", err), "try again or inspect the central store.")
	}
	return printIssueList(issues, jsonMode, allProjects)
}

func runInitReattach(db *sql.DB, rootPath string, name string, jsonMode bool) int {
	if !projectNamePattern.MatchString(name) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", name), "use the format [a-z0-9][a-z0-9-]{1,62}.")
	}
	existingAtRoot, found, err := findProjectByRoot(db, rootPath)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the current root: %v", err), "try again or inspect the central store.")
	}
	if found && existingAtRoot.Name != name {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("the current root already belongs to project %q.", existingAtRoot.Name), "choose a directory with no registered Project to reattach.")
	}
	p, found, err := findProjectByName(db, name)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read project %q: %v", name, err), "try again or inspect the central store.")
	}
	if !found {
		return fail(jsonMode, exitNotRegistered, fmt.Sprintf("project %q not found.", name), "check the registered Project name.")
	}
	p.RootPath = &rootPath
	reattached, err := updateProjectRoot(db, p)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not re-point project %q: %v", name, err), "check for root_path collisions in the central store.")
	}
	return printProject(reattached, jsonMode)
}

func resolveProject(db *sql.DB, rootPath string, inGit bool, explicitName string) (project, int, string, string) {
	if explicitName != "" {
		if !projectNamePattern.MatchString(explicitName) {
			return project{}, exitBadUsage, fmt.Sprintf("invalid project name %q.", explicitName), "use the format [a-z0-9][a-z0-9-]{1,62}."
		}
		p, found, err := findProjectByName(db, explicitName)
		if err != nil {
			return project{}, exitGeneric, fmt.Sprintf("could not read project %q: %v", explicitName, err), "try again or inspect the central store."
		}
		if !found {
			return project{}, exitNotRegistered, fmt.Sprintf("project %q not found.", explicitName), "check the registered Project name."
		}
		return p, 0, "", ""
	}

	p, found, err := findProjectByRoot(db, rootPath)
	if err != nil {
		return project{}, exitGeneric, fmt.Sprintf("could not read the current project: %v", err), "try again or inspect the central store."
	}
	if found {
		return p, 0, "", ""
	}
	if !inGit {
		p, found, err := findClosestProjectAncestor(db, rootPath)
		if err != nil {
			return project{}, exitGeneric, fmt.Sprintf("could not resolve the ancestor project: %v", err), "try again or inspect the central store."
		}
		if found {
			return p, 0, "", ""
		}
	}
	name := normalizeProjectName(filepath.Base(rootPath))
	detached, found, err := findDetachedProjectByName(db, name, rootPath)
	if err != nil {
		return project{}, exitGeneric, fmt.Sprintf("could not search for detached projects: %v", err), "try again or inspect the central store."
	}
	if found {
		return project{}, exitNotRegistered, fmt.Sprintf("project %q exists but does not point to this directory.", detached.Name), fmt.Sprintf("run 'ito init --reattach %s' to re-point this Project.", detached.Name)
	}
	return project{}, exitNotRegistered, "no Project registered for the current directory.", "run 'ito init' in this Project or use --project <name>."
}

// commandValueFlags returns the value-consuming flags for a command, mirroring
// each handler's fs.Var/fs.StringVar set. It is the single source of truth that
// keeps help/json detection (which must skip flag values) in sync with parsing.
func commandValueFlags(command string) map[string]struct{} {
	switch command {
	case "init":
		return map[string]struct{}{"name": {}, "prefix": {}, "reattach": {}}
	case "rename":
		return map[string]struct{}{"project": {}}
	case "new":
		return map[string]struct{}{"project": {}, "title": {}, "status": {}, "priority": {}, "label": {}, "body": {}}
	case "show":
		return map[string]struct{}{"project": {}}
	case "list":
		return map[string]struct{}{"project": {}, "status": {}, "priority": {}, "search": {}, "label": {}}
	case "move":
		return map[string]struct{}{"project": {}}
	case "edit":
		return map[string]struct{}{
			"project": {}, "title": {}, "priority": {}, "body": {},
			"add-label": {}, "remove-label": {},
			"block": {}, "unblock": {}, "relate": {}, "unrelate": {},
		}
	case "rm":
		return map[string]struct{}{"project": {}}
	case "prune":
		return map[string]struct{}{"project": {}, "status": {}}
	default:
		return nil
	}
}

// flagName extracts a flag's name from a token, stripping leading dashes and any
// inline "=value". Non-flag tokens yield an empty name.
func flagName(arg string) string {
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return ""
	}
	name := strings.TrimLeft(arg, "-")
	if idx := strings.IndexByte(name, '='); idx >= 0 {
		name = name[:idx]
	}
	return name
}

// wantsJSON reports whether --json (or -json / --json=true) is set, ignoring any
// token consumed as a value-flag's value (so a flag value of "-json" is not
// mistaken for a request). valueFlags names the command's value-consuming flags.
func wantsJSON(args []string, valueFlags map[string]struct{}) bool {
	flagArgs, _ := splitFlagsAndPositionals(args, valueFlags)
	jsonMode := false
	for i := 0; i < len(flagArgs); i++ {
		arg := flagArgs[i]
		name := flagName(arg)
		if _, ok := valueFlags[name]; ok && !strings.Contains(arg, "=") {
			// Skip the next token: it is this flag's value, not a request.
			i++
			continue
		}
		if name != "json" {
			continue
		}
		if idx := strings.IndexByte(arg, '='); idx >= 0 {
			parsed, err := strconv.ParseBool(arg[idx+1:])
			jsonMode = err == nil && parsed
			continue
		}
		jsonMode = true
	}
	return jsonMode
}

// wantsHelp reports whether a help flag is set, ignoring any token consumed as a
// value-flag's value (so a flag value of "-h"/"--help" is not mistaken for a
// help request). valueFlags names the command's value-consuming flags.
func wantsHelp(args []string, valueFlags map[string]struct{}) bool {
	flagArgs, _ := splitFlagsAndPositionals(args, valueFlags)
	for i := 0; i < len(flagArgs); i++ {
		arg := flagArgs[i]
		name := flagName(arg)
		if _, ok := valueFlags[name]; ok && !strings.Contains(arg, "=") {
			i++
			continue
		}
		if isHelpArg(arg) {
			return true
		}
	}
	return false
}

func isHelpArg(arg string) bool {
	return arg == "--help" || arg == "-help" || arg == "-h"
}

func printRootHelp(w io.Writer) {
	fmt.Fprintln(w, `usage: ito <command> [flags]

Local issue tracker with a central SQLite store at ~/.ito/ito.db.

Commands:
  init     Registers or re-points the Project for the current directory.
  new      Creates an Issue in the current Project.
  list     Lists Issues.
  show     Shows an Issue by full ID.
  move     Moves an Issue to another status.
  edit     Edits an Issue's fields, Labels and Links.
  rm       Deletes an Issue.
  prune    Deletes Issues in bulk with an explicit filter.
  rename   Renames the current Project.

Use "ito <command> --help" to see the command's flags.`)
}

func printCommandHelp(command string) {
	switch command {
	case "init":
		fmt.Println(`usage: ito init [--name <name>] [--prefix <PREFIX>] [--reattach <name>] [--json]

Registers the current git root, or the cwd outside git, in the central store. Does not write to the repo.

Flags:
  --name <name>        Initial Project name. Format: [a-z0-9][a-z0-9-]{1,62}.
  --prefix <PREFIX>    Manual Prefix. Format: [A-Z][A-Z0-9]{1,7}.
  --reattach <name>    Re-points an existing Project to the current root.
  --json               Prints JSON.`)
	case "rename":
		fmt.Println(`usage: ito rename [--project <name>] [--json] <name>

Renames the current Project or an explicit Project.

Flags:
  --project <name>     Target Project when the cwd should not resolve implicitly.
  --json               Prints JSON.`)
	case "new":
		fmt.Println(`usage: ito new --title <title> [--status <status>] [--priority <priority>] [--label <label>] [--body <text>|-] [--project <name>] [--json]

Creates an Issue and prints the ID in human mode.

Flags:
  --title <title>          Title is required.
  --status <status>        backlog, todo, in_progress, in_review or done. Default: backlog.
  --priority <priority>    low, medium, high or urgent. Default: low.
  --label <label>          Repeatable initial Label: feature, bug, docs, tests, refactor, chore, research or infra.
  --body <text>|-          Markdown body. Use "-" to read stdin.
  --project <name>         Explicit Project.
  --json                   Prints JSON.`)
	case "show":
		fmt.Println(`usage: ito show [--project <name>] [--json] <PREFIX>-<n>

Shows an Issue by full ID.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "list":
		fmt.Println(`usage: ito list [--status <status>] [--priority <priority>] [--label <label>] [--search <text>] [--project <name>|--all-projects] [--json]

Lists Issues in the current Project. Issues in done are hidden by default, except with --status done.

Flags:
  --status <status>        Filter by backlog, todo, in_progress, in_review or done.
  --priority <priority>    Filter by low, medium, high or urgent.
  --label <label>          Filter by Label. Repeatable.
  --search <text>          Full-text search in title and body.
  --project <name>         Explicit Project.
  --all-projects           Lists all Projects.
  --json                   Prints JSON.`)
	case "move":
		fmt.Println(`usage: ito move [--project <name>] [--json] <PREFIX>-<n> <status>

Moves an Issue to any valid status.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "edit":
		fmt.Println(`usage: ito edit <PREFIX>-<n> [--title <title>] [--priority <priority>] [--body <text>|-] [--add-label <label>] [--remove-label <label>] [--block <ID>] [--unblock <ID>] [--relate <ID>] [--unrelate <ID>] [--project <name>] [--json]

Edits an Issue. Requires at least one change.

Flags:
  --title <title>          New title.
  --priority <priority>    low, medium, high or urgent.
  --body <text>|-          New markdown body. Use "-" to read stdin.
  --add-label <label>      Adds a Label. Repeatable.
  --remove-label <label>   Removes a Label. Repeatable.
  --block <ID>             Adds a blocked_by link.
  --unblock <ID>           Removes a blocked_by link.
  --relate <ID>            Adds a relates_to link.
  --unrelate <ID>          Removes a relates_to link.
  --project <name>         Validates that the Issue belongs to the given Project.
  --json                   Prints JSON.`)
	case "rm":
		fmt.Println(`usage: ito rm [--project <name>] [--json] <PREFIX>-<n>

Deletes an Issue and its Links/Labels.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "prune":
		fmt.Println(`usage: ito prune --status <status> --yes [--project <name>] [--json]

Deletes Issues in bulk. Requires an explicit filter and flag confirmation.

Flags:
  --status <status>    Filter by backlog, todo, in_progress, in_review or done.
  --yes                Confirms the destructive deletion.
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	}
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
	rootPath := "(detached)"
	if p.RootPath != nil {
		rootPath = *p.RootPath
	}
	fmt.Printf("%s %s %s\n", p.Name, p.Prefix, rootPath)
	return 0
}

func printIssue(i issue, jsonMode bool) int {
	if jsonMode {
		encoded, err := json.Marshal(i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the Issue: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	fmt.Println(i.ID)
	return 0
}

func printIssueDetail(i issue, jsonMode bool) int {
	if jsonMode {
		encoded, err := json.Marshal(i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the Issue: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	fmt.Printf("ID: %s\n", i.ID)
	fmt.Printf("Project: %s\n", i.Project)
	fmt.Printf("Title: %s\n", i.Title)
	fmt.Printf("Status: %s\n", i.Status)
	fmt.Printf("Priority: %s\n", i.Priority)
	fmt.Printf("Created: %s\n", i.Created)
	fmt.Printf("Updated: %s\n", i.Updated)
	fmt.Printf("Labels: %s\n", formatList(i.Labels))
	fmt.Println("Links:")
	fmt.Printf("  blocked_by: %s\n", formatList(i.BlockedBy))
	fmt.Printf("  relates_to: %s\n", formatList(i.RelatesTo))
	fmt.Println("Body:")
	fmt.Print(i.Body)
	if i.Body == "" || !strings.HasSuffix(i.Body, "\n") {
		fmt.Println()
	}
	return 0
}

func printIssueList(issues []issue, jsonMode bool, allProjects bool) int {
	if jsonMode {
		items := make([]issueListItem, 0, len(issues))
		for _, i := range issues {
			items = append(items, issueListItem{
				ID:        i.ID,
				Project:   i.Project,
				Title:     i.Title,
				Status:    i.Status,
				Priority:  i.Priority,
				Labels:    i.Labels,
				BlockedBy: i.BlockedBy,
				RelatesTo: i.RelatesTo,
				Created:   i.Created,
				Updated:   i.Updated,
			})
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not serialize the Issue list: %v\n", err)
			return exitGeneric
		}
		fmt.Println(string(encoded))
		return 0
	}
	if len(issues) == 0 {
		fmt.Println("no Issues found. adjust the filters or create one with 'ito new --title <title>'.")
		return 0
	}
	currentProject := ""
	for _, i := range issues {
		if allProjects && i.Project != currentProject {
			if currentProject != "" {
				fmt.Println()
			}
			currentProject = i.Project
			fmt.Printf("%s:\n", currentProject)
		}
		prefix := ""
		if allProjects {
			prefix = "  "
		}
		fmt.Printf("%s%s [%s %s] %s\n", prefix, i.ID, i.Status, i.Priority, i.Title)
	}
	return 0
}

func formatList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return strings.Join(values, ", ")
}

func isValidValue(value string, valid map[string]struct{}) bool {
	_, ok := valid[value]
	return ok
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

// splitFlagsAndPositionals separates flag tokens from the positional arguments,
// allowing flags to appear in any position relative to the positionals
// (the stdlib flag.Parse stops at the first non-flag token). valueFlags names
// the flags that consume the next argument as a value, so that value
// is not mistaken for a positional.
func splitFlagsAndPositionals(args []string, valueFlags map[string]struct{}) (flagArgs, positionals []string) {
	flagArgs = make([]string, 0, len(args))
	positionals = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// End-of-flags marker: everything after is positional. Consume the
			// marker itself so it composes with fs.Parse downstream.
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			name = name[:idx]
		}
		if _, ok := valueFlags[name]; ok && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positionals
}

func splitEditArgs(args []string) ([]string, string, int) {
	valueFlags := map[string]struct{}{
		"project":      {},
		"title":        {},
		"priority":     {},
		"body":         {},
		"add-label":    {},
		"remove-label": {},
		"block":        {},
		"unblock":      {},
		"relate":       {},
		"unrelate":     {},
	}
	flagArgs, positionals := splitFlagsAndPositionals(args, valueFlags)
	issueID := ""
	if len(positionals) > 0 {
		issueID = positionals[0]
	}
	return flagArgs, issueID, len(positionals)
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
	// Distinguish git authoritatively reporting no work tree (a clean non-zero
	// exit) from the git invocation itself failing (git missing, exec error,
	// killed by signal). Only the authoritative no-work-tree case is a genuine
	// "not in a repo" and may fall back to the cwd with inGit=false; an
	// invocation failure must surface, so a transient git hiccup inside a real
	// repo never silently degrades to ancestor matching against the cwd.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", false, fmt.Errorf("could not invoke git: %w", err)
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

func findProjectByName(db *sql.DB, name string) (project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	var p project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project{}, false, nil
		}
		return project{}, false, err
	}
	return p, true, nil
}

func findProjectByPrefix(db *sql.DB, prefix string) (project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, prefix)
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
		if candidate.RootPath == nil {
			continue
		}
		if isPathAncestor(*candidate.RootPath, cwd) && len(*candidate.RootPath) > bestLen {
			best = candidate
			bestLen = len(*candidate.RootPath)
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

// findDetachedProjectByName reports whether a same-named Project exists whose
// stored root_path no longer matches the current resolved root. Detachment is a
// path-identity question, not an on-disk-existence one: a Project is detached
// when its root_path is NULL or, once canonicalized the same way callers
// canonicalize the current root, points somewhere other than currentRoot. This
// is a pure read; it never mutates the store (the NULL-clearing on the moved-repo
// path lives on the init write path, in runInit's detached branch).
func findDetachedProjectByName(db *sql.DB, name string, currentRoot string) (project, bool, error) {
	row := db.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE name = ?`, name)
	var p project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project{}, false, nil
		}
		return project{}, false, err
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
	return project{}, false, nil
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
	return project{ID: id, Name: name, Prefix: prefix, RootPath: &rootPath}, nil
}

// insertProjectWithGeneratedPrefix derives an auto-generated prefix and inserts
// the project inside a single IMMEDIATE transaction, so two concurrent inits
// deriving the same base prefix cannot both pass the uniqueness check and race
// on the INSERT. The returned prefix reflects the deterministic auto-suffix.
func insertProjectWithGeneratedPrefix(db *sql.DB, name, baseInput, rootPath string) (project, error) {
	tx, err := db.Begin()
	if err != nil {
		return project{}, err
	}
	defer tx.Rollback()

	prefix, err := nextGeneratedPrefixTx(tx, baseInput)
	if err != nil {
		return project{}, err
	}
	result, err := tx.Exec(`INSERT INTO projects(name, prefix, root_path) VALUES (?, ?, ?)`, name, prefix, rootPath)
	if err != nil {
		return project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return project{}, err
	}
	if err := tx.Commit(); err != nil {
		return project{}, err
	}
	return project{ID: id, Name: name, Prefix: prefix, RootPath: &rootPath}, nil
}

func updateProjectRoot(db *sql.DB, p project) (project, error) {
	result, err := db.Exec(`UPDATE projects SET root_path = ? WHERE id = ?`, p.RootPath, p.ID)
	if err != nil {
		return project{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return project{}, err
	}
	if affected != 1 {
		return project{}, sql.ErrNoRows
	}
	return p, nil
}

func updateProjectName(db *sql.DB, p project, name string) (project, error) {
	result, err := db.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, p.ID)
	if err != nil {
		return project{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return project{}, err
	}
	if affected != 1 {
		return project{}, sql.ErrNoRows
	}
	p.Name = name
	return p, nil
}

func insertIssue(db *sql.DB, p project, title, status, priority string, labels []string, body string) (issue, error) {
	tx, err := db.Begin()
	if err != nil {
		return issue{}, err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`UPDATE projects SET last_id = last_id + 1 WHERE id = ?`, p.ID)
	if err != nil {
		return issue{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return issue{}, err
	}
	if affected != 1 {
		return issue{}, sql.ErrNoRows
	}

	var nextID int64
	if err := tx.QueryRow(`SELECT last_id FROM projects WHERE id = ?`, p.ID).Scan(&nextID); err != nil {
		return issue{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	created := issue{
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
		return issue{}, err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return issue{}, err
	}
	if _, err := tx.Exec(`INSERT INTO issues_fts(rowid, title, body) VALUES (?, ?, ?)`, rowID, created.Title, created.Body); err != nil {
		return issue{}, err
	}
	for _, label := range created.Labels {
		if _, err := tx.Exec(`INSERT INTO issue_labels(project_id, issue_id, label) VALUES (?, ?, ?)`, p.ID, created.ID, label); err != nil {
			return issue{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return issue{}, err
	}
	return created, nil
}

func moveIssueStatus(db *sql.DB, p project, id string, targetStatus string) (string, bool, error) {
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

func editIssue(db *sql.DB, p project, id string, options editIssueOptions) (bool, error) {
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
			return false, commandFailure{
				code:    exitNotFound,
				message: fmt.Sprintf("Issue %q not found.", op.Target),
				hint:    "check the full ID of the linked Issue.",
			}
		}
		if targetProject.ID != p.ID {
			return false, commandFailure{
				code:    exitBadUsage,
				message: fmt.Sprintf("Issue %q belongs to Project %q, not to %q.", op.Target, targetProject.Name, p.Name),
				hint:    "In v1, links can only point to Issues in the same Project.",
			}
		}
		if exists, err := issueExistsTx(tx, p.ID, op.Target); err != nil {
			return false, err
		} else if !exists {
			return false, commandFailure{
				code:    exitNotFound,
				message: fmt.Sprintf("Issue %q not found.", op.Target),
				hint:    "check the full ID of the linked Issue.",
			}
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

func deleteIssue(db *sql.DB, p project, id string) error {
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

func deleteIssuesByStatus(db *sql.DB, p project, status string) (int, error) {
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

func deleteIssueRowsTx(tx *sql.Tx, p project, matches []issueDeletionRow) (int, error) {
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

func findIssueByID(db *sql.DB, p project, id string) (issue, bool, error) {
	row := db.QueryRow(`
SELECT issues.id, projects.name, issues.title, issues.status, issues.priority, issues.body, issues.created, issues.updated
FROM issues
JOIN projects ON projects.id = issues.project_id
WHERE issues.project_id = ? AND issues.id = ?`, p.ID, id)
	found := issue{
		Labels:    []string{},
		BlockedBy: []string{},
		RelatesTo: []string{},
	}
	if err := row.Scan(&found.ID, &found.Project, &found.Title, &found.Status, &found.Priority, &found.Body, &found.Created, &found.Updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return issue{}, false, nil
		}
		return issue{}, false, err
	}

	labels, err := stringColumn(db, `SELECT label FROM issue_labels WHERE project_id = ? AND issue_id = ? ORDER BY label`, p.ID, id)
	if err != nil {
		return issue{}, false, err
	}
	blockedBy, err := stringColumn(db, `
SELECT target_id
FROM issue_links
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND source_id = ? AND kind = 'blocked_by'
ORDER BY target_id`, p.ID, id)
	if err != nil {
		return issue{}, false, err
	}
	relatesTo, err := stringColumn(db, `
SELECT CASE WHEN source_id = ? THEN target_id ELSE source_id END
FROM issue_links
JOIN issues AS source ON source.project_id = issue_links.project_id AND source.id = issue_links.source_id
JOIN issues AS target ON target.project_id = issue_links.project_id AND target.id = issue_links.target_id
WHERE issue_links.project_id = ? AND kind = 'relates_to' AND (source_id = ? OR target_id = ?)
ORDER BY 1`, id, p.ID, id, id)
	if err != nil {
		return issue{}, false, err
	}
	found.Labels = labels
	found.BlockedBy = sortIssueIDs(blockedBy)
	found.RelatesTo = sortIssueIDs(relatesTo)
	return found, true, nil
}

func listIssues(db *sql.DB, options listOptions) ([]issue, error) {
	where := []string{}
	args := []any{}
	ftsQuery := searchQuery(options.Search)
	searching := strings.TrimSpace(options.Search) != ""
	if searching {
		if ftsQuery == "" {
			return []issue{}, nil
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
		issue
		ProjectID int64
	}
	rowIssues := []rowIssue{}
	for rows.Next() {
		found := rowIssue{
			issue: issue{
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

	issues := make([]issue, 0, len(rowIssues))
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
		issues = append(issues, rowIssue.issue)
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

func findProjectByIssueIDTx(tx *sql.Tx, issueID string) (project, bool, error) {
	matches := issueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return project{}, false, nil
	}
	row := tx.QueryRow(`SELECT id, name, prefix, root_path FROM projects WHERE prefix = ?`, matches[1])
	var p project
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.RootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project{}, false, nil
		}
		return project{}, false, err
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
