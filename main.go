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
	"slices"
	"strconv"
	"strings"
	"time"

	itostore "github.com/c3h/ito/internal/store"
	"github.com/c3h/ito/internal/tui"
	"github.com/mattn/go-isatty"
	_ "modernc.org/sqlite"
)

const (
	exitGeneric       = 1
	exitBadUsage      = 2
	exitNotFound      = 3
	exitNotRegistered = 4
)

var (
	// The validation sets are derived from the store's canonical vocabularies so
	// the domain values live in exactly one place (internal/store); the
	// identifier formats come from the store's exported patterns for the same
	// reason.
	validStatuses   = valueSet(itostore.Statuses)
	validPriorities = valueSet(itostore.Priorities)
	validLabels     = valueSet(itostore.Labels)
	isTerminal      = isatty.IsTerminal
	runTUI          = tui.Run
)

// The prose enumerations for hints and help derive from the same slices, so a
// vocabulary change never leaves stale text behind. Priorities are spelled
// ascending (low first) in prose, while the slice orders by precedence.
var (
	statusList   = humanList(itostore.Statuses)
	priorityList = humanList(ascendingPriorities())
	labelList    = humanList(itostore.Labels)
)

// humanList joins a vocabulary for prose: "a, b, c or d".
func humanList(values []string) string {
	if len(values) <= 1 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

func ascendingPriorities() []string {
	values := slices.Clone(itostore.Priorities)
	slices.Reverse(values)
	return values
}

const (
	ansiReset   = "\x1b[0m"
	ansiBright  = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiCyan    = "\x1b[36m"
	ansiBlue    = "\x1b[34m"
	ansiRed     = "\x1b[31m"
	ansiOrange  = "\x1b[38;5;208m"
	ansiMagenta = "\x1b[35m"
)

type listStyle struct {
	enabled bool
}

func newListStyle() listStyle {
	return listStyle{
		enabled: os.Getenv("NO_COLOR") == "" && isatty.IsTerminal(os.Stdout.Fd()),
	}
}

func (s listStyle) apply(code string, value string) string {
	if !s.enabled {
		return value
	}
	return code + value + ansiReset
}

func (s listStyle) issueID(value string) string {
	return s.apply(ansiCyan+ansiBright, value)
}

func (s listStyle) status(value string) string {
	return s.apply(ansiCyan, value)
}

func (s listStyle) priority(value string) string {
	switch value {
	case "urgent":
		return s.apply(ansiRed+ansiBright, value)
	case "high":
		return s.apply(ansiOrange+ansiBright, value)
	case "medium":
		return s.apply(ansiBlue, value)
	case "low":
		return s.apply(ansiDim, value)
	default:
		return value
	}
}

func (s listStyle) project(value string) string {
	return s.apply(ansiMagenta+ansiBright, value)
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringSliceFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type labelEditOp = itostore.LabelEditOp

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

type linkEditOp = itostore.LinkEditOp

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

type project = itostore.Project

type cliError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Hint  string `json:"hint"`
}

type issue = itostore.Issue

type batch = itostore.Batch

type batchPlan = itostore.BatchPlan

type createdBatch struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Created string `json:"created"`
}

type batchListItem struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Created string `json:"created"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Waves   *int   `json:"waves"`
}

type batchListRow struct {
	Batch batch
	Waves *int
}

type deletedBatch struct {
	Deleted        string `json:"deleted"`
	MembersCleared int    `json:"members_cleared"`
}

type batchShowItem struct {
	Name    string          `json:"name"`
	Project string          `json:"project"`
	Created string          `json:"created"`
	Total   int             `json:"total"`
	Done    int             `json:"done"`
	Waves   []batchWaveItem `json:"waves"`
}

type batchWaveItem struct {
	Wave   int             `json:"wave"`
	Ready  bool            `json:"ready"`
	Issues []issueListItem `json:"issues"`
}

type issueListItem struct {
	ID            string   `json:"id"`
	Project       string   `json:"project"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	Priority      string   `json:"priority"`
	Labels        []string `json:"labels"`
	BlockedBy     []string `json:"blocked_by"`
	RelatesTo     []string `json:"relates_to"`
	ConflictsWith []string `json:"conflicts_with"`
	Batch         *string  `json:"batch"`
	Created       string   `json:"created"`
	Updated       string   `json:"updated"`
}

type listOptions = itostore.ListOptions

type editIssueOptions = itostore.EditIssueOptions

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
func openMigratedStore() (*sql.DB, *itostore.Store, *commandFailure) {
	db, err := itostore.OpenDefault()
	if err != nil {
		return nil, nil, &commandFailure{exitGeneric, fmt.Sprintf("could not open the central store: %v", err), "check ITO_HOME and the directory permissions."}
	}
	if err := itostore.Migrate(db); err != nil {
		db.Close()
		return nil, nil, &commandFailure{exitGeneric, fmt.Sprintf("could not migrate the central store: %v", err), "check that the ito.db file is a valid SQLite database."}
	}
	return db, itostore.New(db), nil
}

// resolveIssueProject finds the Project that owns the Issue's Prefix and, when an
// explicit project name is given, validates that it names the same Project. It
// returns the owning Project or a typed *commandFailure the handler renders.
func resolveIssueProject(st *itostore.Store, prefix string, projectName string, issueID string) (project, *commandFailure) {
	p, found, err := st.FindProjectByPrefix(prefix)
	if err != nil {
		return project{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read the Project for prefix %q: %v", prefix, err), "try again or inspect the central store."}
	}
	if !found {
		return project{}, &commandFailure{exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID."}
	}
	if projectName != "" {
		override, found, err := st.FindProjectByName(projectName)
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
		if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
			printRootHelp(os.Stdout)
			return 0
		}
		db, st, openFail := openMigratedStore()
		if openFail != nil {
			return fail(false, openFail.code, openFail.message, openFail.hint)
		}
		defer db.Close()
		p, code, message, hint := resolveProject(st, "")
		if code != 0 {
			if code != exitNotRegistered {
				return fail(false, code, message, hint)
			}
			p = project{}
		}
		if err := runTUI(st, p); err != nil {
			return fail(false, exitGeneric, fmt.Sprintf("could not run the TUI: %v", err), "try again from an interactive terminal.")
		}
		return 0
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
	case "batch":
		return runBatch(args[1:])
	case "move":
		return runMove(args[1:])
	case "edit":
		return runEdit(args[1:])
	case "rm":
		return runRm(args[1:])
	case "prune":
		return runPrune(args[1:])
	default:
		return fail(wantsJSON(args, nil), exitBadUsage, "unknown command: "+args[0]+".", "Run 'ito --help' to see available commands.")
	}
}

func runBatch(args []string) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		printCommandHelp("batch")
		return 0
	}
	switch args[0] {
	case "new":
		return runBatchNew(args[1:])
	case "list":
		return runBatchList(args[1:])
	case "rename":
		return runBatchRename(args[1:])
	case "rm":
		return runBatchRM(args[1:])
	case "show":
		return runBatchShow(args[1:])
	default:
		return fail(wantsJSON(args[1:], nil), exitBadUsage, "unknown batch command: "+args[0]+".", "Run 'ito batch --help' to see available commands.")
	}
}

func runBatchNew(args []string) int {
	if wantsHelp(args, commandValueFlags("batch new")) {
		printCommandHelp("batch new")
		return 0
	}
	fs := flag.NewFlagSet("batch new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch new")), exitBadUsage, err.Error(), "run 'ito batch new --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito batch new takes exactly one name.", "use: ito batch new <name>.")
	}
	name := positionals[0]
	if !itostore.ProjectNamePattern.MatchString(name) {
		return failInvalidBatchName(jsonMode, name)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	created, err := st.CreateBatch(p, name)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchExists) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("a batch named %q already exists in Project %q.", name, p.Name), "choose another Batch name.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not create the Batch: %v", err), "try again or inspect the central store.")
	}
	return printCreatedBatch(created, jsonMode)
}

func runBatchList(args []string) int {
	if wantsHelp(args, commandValueFlags("batch list")) {
		printCommandHelp("batch list")
		return 0
	}
	fs := flag.NewFlagSet("batch list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch list")), exitBadUsage, err.Error(), "run 'ito batch list --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 {
		return fail(jsonMode, exitBadUsage, "ito batch list takes no positional arguments.", "use flags like --project or --json.")
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	batches, err := st.ListBatches(p)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not list Batches: %v", err), "try again or inspect the central store.")
	}
	rows, err := batchListRows(st, p, batches)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not derive Batch Waves: %v", err), "try again or inspect the central store.")
	}
	return printBatchList(rows, jsonMode)
}

func runBatchRename(args []string) int {
	if wantsHelp(args, commandValueFlags("batch rename")) {
		printCommandHelp("batch rename")
		return 0
	}
	fs := flag.NewFlagSet("batch rename", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch rename")), exitBadUsage, err.Error(), "run 'ito batch rename --help' to see the accepted flags.")
	}
	if len(positionals) != 2 {
		return fail(jsonMode, exitBadUsage, "ito batch rename takes exactly two names.", "use: ito batch rename <old> <new>.")
	}
	oldName, newName := positionals[0], positionals[1]
	if !itostore.ProjectNamePattern.MatchString(newName) {
		return failInvalidBatchName(jsonMode, newName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	renamed, err := st.RenameBatch(p, oldName, newName)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchExists) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("a batch named %q already exists in Project %q.", newName, p.Name), "choose another Batch name.")
		}
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", oldName, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not rename the Batch: %v", err), "try again or inspect the central store.")
	}
	return printRenamedBatch(oldName, renamed, jsonMode)
}

func runBatchRM(args []string) int {
	if wantsHelp(args, commandValueFlags("batch rm")) {
		printCommandHelp("batch rm")
		return 0
	}
	fs := flag.NewFlagSet("batch rm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch rm")), exitBadUsage, err.Error(), "run 'ito batch rm --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito batch rm takes exactly one name.", "use: ito batch rm <name>.")
	}
	name := positionals[0]

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	deleted, err := st.DeleteBatch(p, name)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", name, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not delete the Batch: %v", err), "try again or inspect the central store.")
	}
	return printDeletedBatch(deleted.Name, deleted.MembersCleared, jsonMode)
}

func runBatchShow(args []string) int {
	if wantsHelp(args, commandValueFlags("batch show")) {
		printCommandHelp("batch show")
		return 0
	}
	fs := flag.NewFlagSet("batch show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch show")), exitBadUsage, err.Error(), "run 'ito batch show --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito batch show takes exactly one name.", "use: ito batch show <name>.")
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	plan, err := st.ShowBatch(p, positionals[0])
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", positionals[0], p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		var cycle *itostore.BatchCycleError
		if errors.As(err, &cycle) {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("Batch %q has a blocked_by cycle among: %s.", positionals[0], strings.Join(cycle.Issues, ", ")), "remove or change one blocked_by link before deriving Waves.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not show Batch: %v", err), "try again or inspect the central store.")
	}
	return printBatchShow(plan, jsonMode)
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
		name = itostore.NormalizeProjectName(filepath.Base(rootPath))
	}
	if !itostore.ProjectNamePattern.MatchString(name) {
		return failInvalidProjectName(jsonMode, name)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	if reattachName != "" {
		return runInitReattach(st, rootPath, reattachName, jsonMode)
	}

	existing, found, err := st.FindProjectByRoot(rootPath)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the current project: %v", err), "try again or inspect the central store.")
	}
	if found {
		return printProject(existing, jsonMode)
	}
	// Explicit --name/--prefix means the user is asking for a new Project at the
	// cwd — returning a covering ancestor would silently drop the flags.
	if !inGit && manualName == "" && manualPrefix == "" {
		existing, found, err := st.FindClosestProjectAncestor(rootPath)
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
		detached, found, err := st.FindDetachedProjectByName(name, rootPath)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not search for detached projects: %v", err), "try again or inspect the central store.")
		}
		if found {
			// This is the init write path, so it is the correct place to clear a
			// stale pointer: if the detached Project still records an old root,
			// persist root_path = NULL so the Project serializes as detached.
			if detached.RootPath != nil {
				detached.RootPath = nil
				if _, err := st.UpdateProjectRoot(detached); err != nil {
					return fail(jsonMode, exitGeneric, fmt.Sprintf("could not clear the stale root for project %q: %v", detached.Name, err), "try again or inspect the central store.")
				}
			}
			return fail(jsonMode, exitNotRegistered, fmt.Sprintf("project %q exists but does not point to this directory.", detached.Name), fmt.Sprintf("run 'ito init --reattach %s' to re-point this Project.", detached.Name))
		}
	}

	if exists, err := st.ProjectNameExists(name); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project name: %v", err), "try again or inspect the central store.")
	} else if exists {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", name), "choose another one with --name.")
	}

	var created project
	prefix := manualPrefix
	if prefix != "" {
		if !itostore.PrefixPattern.MatchString(prefix) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid prefix %q.", prefix), "use the format [A-Z][A-Z0-9]{1,7}.")
		}
		if exists, err := st.ProjectPrefixExists(prefix); err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project prefix: %v", err), "try again or inspect the central store.")
		} else if exists {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project with prefix %q already exists.", prefix), "choose another one with --prefix.")
		}
		created, err = st.CreateProject(name, prefix, rootPath)
		if err != nil {
			return failCreateProject(jsonMode, name, prefix, err)
		}
	} else {
		created, err = st.CreateProjectWithGeneratedPrefix(name, filepath.Base(rootPath), rootPath)
		if err != nil {
			return failCreateProject(jsonMode, name, prefix, err)
		}
	}
	return printProject(created, jsonMode)
}

// failCreateProject renders a project-creation failure, mapping the store's
// typed uniqueness errors (which close the check-then-insert race window) to
// the same actionable exit-2 messages the pre-checks produce.
func failCreateProject(jsonMode bool, name, prefix string, err error) int {
	if errors.Is(err, itostore.ErrNameExists) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", name), "choose another one with --name.")
	}
	if errors.Is(err, itostore.ErrPrefixExists) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project with prefix %q already exists.", prefix), "choose another one with --prefix.")
	}
	return fail(jsonMode, exitGeneric, fmt.Sprintf("could not register the project: %v", err), "check for name, prefix or root_path collisions.")
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
	if !itostore.ProjectNamePattern.MatchString(newName) {
		return failInvalidProjectName(jsonMode, newName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	if exists, err := st.ProjectNameExistsForAnotherID(newName, p.ID); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the project name: %v", err), "try again or inspect the central store.")
	} else if exists {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("a project named %q already exists.", newName), "choose another name.")
	}
	renamed, err := st.RenameProject(p, newName)
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
	var batchName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&status, "status", "backlog", "")
	fs.StringVar(&priority, "priority", "low", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&body, "body", "", "")
	fs.StringVar(&batchName, "batch", "", "")
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
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use "+statusList+".")
	}
	if !isValidValue(priority, validPriorities) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use "+priorityList+".")
	}
	for _, label := range labels {
		if !isValidValue(label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", label), "use "+labelList+".")
		}
	}
	if body == "-" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the Issue body from stdin: %v", err), "try again by passing --body <text>.")
		}
		body = string(input)
	}
	batchSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "batch" {
			batchSet = true
		}
	})
	if batchSet && batchName == "" {
		return fail(jsonMode, exitBadUsage, "--batch requires a non-empty Batch name on new.", "omit --batch to create the Issue outside any Batch.")
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	created, err := st.CreateIssueInBatch(p, title, status, priority, labels, body, batchName)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", batchName, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
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
	matches := itostore.IssueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(st, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	foundIssue, err := st.FindIssue(p, issueID)
	if err != nil && !errors.Is(err, itostore.ErrNotFound) {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if errors.Is(err, itostore.ErrNotFound) {
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
	matches := itostore.IssueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	if !isValidValue(targetStatus, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", targetStatus), "use "+statusList+".")
	}
	prefix := matches[1]
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(st, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	moved, err := st.Move(p, issueID, targetStatus)
	if err != nil {
		if errors.Is(err, itostore.ErrNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not move Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if jsonMode {
		return printIssueDetail(moved.Issue, true)
	}
	if !moved.Changed {
		fmt.Printf("%s is already in %s; nothing changed.\n", moved.Issue.ID, targetStatus)
		return 0
	}
	fmt.Printf("%s moved from %s to %s.\n", moved.Issue.ID, moved.BeforeStatus, targetStatus)
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
	var batchName string
	var labelOps []labelEditOp
	var linkOps []linkEditOp
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&body, "body", "", "")
	fs.StringVar(&batchName, "batch", "", "")
	fs.Var(labelEditFlag{kind: "add", ops: &labelOps}, "add-label", "")
	fs.Var(labelEditFlag{kind: "remove", ops: &labelOps}, "remove-label", "")
	fs.Var(linkEditFlag{kind: "blocked_by", action: "add", ops: &linkOps}, "block", "")
	fs.Var(linkEditFlag{kind: "blocked_by", action: "remove", ops: &linkOps}, "unblock", "")
	fs.Var(linkEditFlag{kind: "relates_to", action: "add", ops: &linkOps}, "relate", "")
	fs.Var(linkEditFlag{kind: "relates_to", action: "remove", ops: &linkOps}, "unrelate", "")
	fs.Var(linkEditFlag{kind: "conflicts_with", action: "add", ops: &linkOps}, "conflict", "")
	fs.Var(linkEditFlag{kind: "conflicts_with", action: "remove", ops: &linkOps}, "unconflict", "")
	parseArgs, issueID, positionalCount := splitEditArgs(args)
	if err := fs.Parse(parseArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("edit")), exitBadUsage, err.Error(), "run 'ito edit --help' to see the accepted flags.")
	}
	if fs.NArg() != 0 || positionalCount != 1 {
		return fail(jsonMode, exitBadUsage, "ito edit takes exactly one full ID.", "use: ito edit <PREFIX>-<n> [--title <title>] [--priority <priority>] [--body <text>|-] [--block <ID>].")
	}
	matches := itostore.IssueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
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
		case "batch":
			options.BatchSet = true
		}
	})
	if !options.TitleSet && !options.PrioritySet && !options.BodySet && !options.BatchSet && len(options.LabelOps) == 0 && len(options.LinkOps) == 0 {
		return fail(jsonMode, exitBadUsage, "no changes requested.", "use at least one flag like --title, --priority, --body, --batch, --add-label, --block, --relate or --conflict.")
	}
	if options.TitleSet {
		if strings.TrimSpace(title) == "" {
			return fail(jsonMode, exitBadUsage, "title is required.", "use --title <title> with non-empty text.")
		}
		options.Title = title
	}
	if options.PrioritySet {
		if !isValidValue(priority, validPriorities) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use "+priorityList+".")
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
	if options.BatchSet {
		options.Batch = batchName
	}
	for _, op := range options.LabelOps {
		if !isValidValue(op.Label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", op.Label), "use "+labelList+".")
		}
	}
	for _, op := range options.LinkOps {
		if !itostore.IssueIDPattern.MatchString(op.Target) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", op.Target), "use the full format <PREFIX>-<n>, for example AUTH-12.")
		}
		if op.Target == issueID {
			return fail(jsonMode, exitBadUsage, "Links to the Issue itself are not allowed.", "specify an Issue different from the source.")
		}
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(st, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	edited, err := st.Edit(p, issueID, options)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", batchName, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		var linkTarget *itostore.LinkTargetNotFoundError
		if errors.As(err, &linkTarget) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", linkTarget.TargetID), "check the linked Issue ID.")
		}
		if errors.Is(err, itostore.ErrNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		var crossProject *itostore.CrossProjectError
		if errors.As(err, &crossProject) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("Issue %q belongs to Project %q, not to %q.", crossProject.IssueID, crossProject.TargetProject, crossProject.SourceProject), "In v1, links can only point to Issues in the same Project.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not edit Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if jsonMode {
		return printIssueDetail(edited.Issue, true)
	}
	if !edited.Changed {
		fmt.Printf("%s unchanged; the final state already matched the request.\n", edited.Issue.ID)
		return 0
	}
	fmt.Printf("%s edited.\n", edited.Issue.ID)
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
	matches := itostore.IssueIDPattern.FindStringSubmatch(issueID)
	if matches == nil {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid Issue ID %q.", issueID), "use the full format <PREFIX>-<n>, for example AUTH-12.")
	}
	prefix := matches[1]
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, resolveFail := resolveIssueProject(st, prefix, projectName, issueID)
	if resolveFail != nil {
		return fail(jsonMode, resolveFail.code, resolveFail.message, resolveFail.hint)
	}

	if err := st.DeleteIssue(p, issueID); err != nil {
		if errors.Is(err, itostore.ErrNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Issue %q not found.", issueID), "check the full Issue ID.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not delete Issue %q: %v", issueID, err), "try again or inspect the central store.")
	}
	if jsonMode {
		return printJSON(deletedIssue{Deleted: 1, ID: issueID}, "removal")
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
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use "+statusList+".")
	}
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
	}

	db, st, openFail := openMigratedStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()
	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}

	deleted, err := st.DeleteIssuesByStatus(p, status)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not delete Issues with status %q: %v", status, err), "try again or inspect the central store.")
	}
	if jsonMode {
		return printJSON(deletedIssues{Deleted: deleted}, "removal")
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
	var batchName string
	var ready bool
	var labels stringSliceFlag
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.BoolVar(&allProjects, "all-projects", false, "")
	fs.StringVar(&status, "status", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&search, "search", "", "")
	fs.StringVar(&batchName, "batch", "", "")
	fs.BoolVar(&ready, "ready", false, "")
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
	if batchName != "" && allProjects {
		return fail(jsonMode, exitBadUsage, "--batch and --all-projects cannot be used together.", "choose a single Project when filtering by Batch.")
	}
	if status != "" && !isValidValue(status, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use "+statusList+".")
	}
	if priority != "" && !isValidValue(priority, validPriorities) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use "+priorityList+".")
	}
	for _, label := range labels {
		if !isValidValue(label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", label), "use "+labelList+".")
		}
	}

	db, st, openFail := openMigratedStore()
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
		Ready:       ready,
		Batch:       batchName,
	}
	if !allProjects {
		p, code, message, hint := resolveProject(st, projectName)
		if code != 0 {
			return fail(jsonMode, code, message, hint)
		}
		options.ProjectID = p.ID
	}

	issues, err := st.ListIssues(options)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in this Project.", batchName), "run 'ito batch list' to see the Project's Batches.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not list Issues: %v", err), "try again or inspect the central store.")
	}
	return printIssueList(issues, jsonMode, allProjects)
}

func runInitReattach(st *itostore.Store, rootPath string, name string, jsonMode bool) int {
	if !itostore.ProjectNamePattern.MatchString(name) {
		return failInvalidProjectName(jsonMode, name)
	}
	existingAtRoot, found, err := st.FindProjectByRoot(rootPath)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not validate the current root: %v", err), "try again or inspect the central store.")
	}
	if found && existingAtRoot.Name != name {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("the current root already belongs to project %q.", existingAtRoot.Name), "choose a directory with no registered Project to reattach.")
	}
	p, found, err := st.FindProjectByName(name)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read project %q: %v", name, err), "try again or inspect the central store.")
	}
	if !found {
		return fail(jsonMode, exitNotRegistered, fmt.Sprintf("project %q not found.", name), "check the registered Project name.")
	}
	p.RootPath = &rootPath
	reattached, err := st.UpdateProjectRoot(p)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not re-point project %q: %v", name, err), "check for root_path collisions in the central store.")
	}
	return printProject(reattached, jsonMode)
}

// resolveProject resolves the Project a command acts on. With an explicit name
// it never touches git or the cwd — so --project keeps working on machines
// without git in PATH; only the implicit path resolves the current root.
func resolveProject(st *itostore.Store, explicitName string) (project, int, string, string) {
	if explicitName != "" {
		if !itostore.ProjectNamePattern.MatchString(explicitName) {
			return project{}, exitBadUsage, fmt.Sprintf("invalid project name %q.", explicitName), "use the format [a-z0-9][a-z0-9-]{1,62}."
		}
		p, found, err := st.FindProjectByName(explicitName)
		if err != nil {
			return project{}, exitGeneric, fmt.Sprintf("could not read project %q: %v", explicitName, err), "try again or inspect the central store."
		}
		if !found {
			return project{}, exitNotRegistered, fmt.Sprintf("project %q not found.", explicitName), "check the registered Project name."
		}
		return p, 0, "", ""
	}

	rootPath, inGit, err := resolveCurrentRoot()
	if err != nil {
		return project{}, exitGeneric, fmt.Sprintf("could not resolve the current project: %v", err), "run the command inside an accessible directory."
	}
	p, err := st.ResolveProject(rootPath, inGit, "")
	if err != nil {
		var detached *itostore.DetachedError
		if errors.As(err, &detached) {
			return project{}, exitNotRegistered, fmt.Sprintf("project %q exists but does not point to this directory.", detached.ProjectName), fmt.Sprintf("run 'ito init --reattach %s' to re-point this Project.", detached.ProjectName)
		}
		if errors.Is(err, itostore.ErrNotRegistered) {
			return project{}, exitNotRegistered, "no Project registered for the current directory.", "run 'ito init' in this Project or use --project <name>."
		}
		return project{}, exitGeneric, fmt.Sprintf("could not read the current project: %v", err), "try again or inspect the central store."
	}
	return p, 0, "", ""
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
		return map[string]struct{}{"project": {}, "title": {}, "status": {}, "priority": {}, "label": {}, "body": {}, "batch": {}}
	case "show":
		return map[string]struct{}{"project": {}}
	case "list":
		return map[string]struct{}{"project": {}, "status": {}, "priority": {}, "search": {}, "label": {}, "batch": {}}
	case "batch new", "batch list", "batch rename", "batch rm", "batch show":
		return map[string]struct{}{"project": {}}
	case "move":
		return map[string]struct{}{"project": {}}
	case "edit":
		return map[string]struct{}{
			"project": {}, "title": {}, "priority": {}, "body": {},
			"batch":     {},
			"add-label": {}, "remove-label": {},
			"block": {}, "unblock": {}, "relate": {}, "unrelate": {}, "conflict": {}, "unconflict": {},
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
  batch    Manages Batches in the current Project.
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
		fmt.Printf(`usage: ito new --title <title> [--status <status>] [--priority <priority>] [--label <label>] [--body <text>|-] [--batch <name>] [--project <name>] [--json]

Creates an Issue and prints the ID in human mode.

Flags:
  --title <title>          Title is required.
  --status <status>        %s. Default: backlog.
  --priority <priority>    %s. Default: low.
  --label <label>          Repeatable initial Label: %s.
  --body <text>|-          Markdown body. Use "-" to read stdin.
  --batch <name>           Assign the Issue to an existing Batch.
  --project <name>         Explicit Project.
  --json                   Prints JSON.
`, statusList, priorityList, labelList)
	case "show":
		fmt.Println(`usage: ito show [--project <name>] [--json] <PREFIX>-<n>

Shows an Issue by full ID.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "list":
		fmt.Printf(`usage: ito list [--ready] [--status <status>] [--priority <priority>] [--label <label>] [--search <text>] [--batch <name>] [--project <name>|--all-projects] [--json]

Lists Issues in the current Project. Issues in done are hidden by default, except with --status done.
Use --ready to list the backlog/todo frontier whose blockers are all done and conflicts_with partners are not unsafe to start in parallel; an agent can fan out one git worktree per ready Issue.

Flags:
  --ready                  Filter to backlog/todo Issues whose blockers are done and conflicts allow parallel work.
  --status <status>        Filter by %s.
  --priority <priority>    Filter by %s.
  --label <label>          Filter by Label. Repeatable.
  --search <text>          Full-text search in title and body.
  --batch <name>           Filter to Issues assigned to an existing Batch.
  --project <name>         Explicit Project.
  --all-projects           Lists all Projects.
  --json                   Prints JSON.
`, statusList, priorityList)
	case "batch":
		fmt.Println(`usage: ito batch <command> [flags]

Manages Batches in the current Project.

Commands:
  new      Creates a Batch.
  list     Lists Batches newest-first.
  show     Shows a Batch grouped by derived Waves.
  rename   Renames a Batch.
  rm       Deletes a Batch and releases its members.

Use "ito batch <command> --help" to see the command's flags.`)
	case "batch new":
		fmt.Println(`usage: ito batch new [--project <name>] [--json] <name>

Creates a Batch in the current Project. The name uses the Project-name format.

Flags:
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	case "batch list":
		fmt.Println(`usage: ito batch list [--project <name>] [--json]

Lists Batches newest-first with created date, progress, and derived Wave count.

Flags:
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	case "batch rename":
		fmt.Println(`usage: ito batch rename [--project <name>] [--json] <old> <new>

Renames a Batch in the current Project. The new name uses the Project-name format.

Flags:
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	case "batch rm":
		fmt.Println(`usage: ito batch rm [--project <name>] [--json] <name>

Deletes a Batch and clears membership from its Issues. Issues are never deleted.

Flags:
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	case "batch show":
		fmt.Println(`usage: ito batch show [--project <name>] [--json] <name>

Shows the Batch's non-done members grouped by derived Waves, plus done/total progress.

Flags:
  --project <name>     Explicit Project.
  --json               Prints JSON.`)
	case "move":
		fmt.Println(`usage: ito move [--project <name>] [--json] <PREFIX>-<n> <status>

Moves an Issue to any valid status.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "edit":
		fmt.Printf(`usage: ito edit <PREFIX>-<n> [--title <title>] [--priority <priority>] [--body <text>|-] [--batch <name>|--batch ""] [--add-label <label>] [--remove-label <label>] [--block <ID>] [--unblock <ID>] [--relate <ID>] [--unrelate <ID>] [--conflict <ID>] [--unconflict <ID>] [--project <name>] [--json]

Edits an Issue. Requires at least one change.

Flags:
  --title <title>          New title.
  --priority <priority>    %s.
  --body <text>|-          New markdown body. Use "-" to read stdin.
  --batch <name>|""        Move into a Batch, or clear membership with "".
  --add-label <label>      Adds a Label. Repeatable.
  --remove-label <label>   Removes a Label. Repeatable.
  --block <ID>             Adds a blocked_by link.
  --unblock <ID>           Removes a blocked_by link.
  --relate <ID>            Adds a relates_to link.
  --unrelate <ID>          Removes a relates_to link.
  --conflict <ID>          Adds a conflicts_with link.
  --unconflict <ID>        Removes a conflicts_with link.
  --project <name>         Validates that the Issue belongs to the given Project.
  --json                   Prints JSON.
`, priorityList)
	case "rm":
		fmt.Println(`usage: ito rm [--project <name>] [--json] <PREFIX>-<n>

Deletes an Issue and its Links/Labels.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "prune":
		fmt.Printf(`usage: ito prune --status <status> --yes [--project <name>] [--json]

Deletes Issues in bulk. Requires an explicit filter and flag confirmation.

Flags:
  --status <status>    Filter by %s.
  --yes                Confirms the destructive deletion.
  --project <name>     Explicit Project.
  --json               Prints JSON.
`, statusList)
	}
}

// failInvalidProjectName reports the standard usage error for a malformed
// Project name.
func failInvalidProjectName(jsonMode bool, name string) int {
	return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid project name %q.", name), "use the format [a-z0-9][a-z0-9-]{1,62}.")
}

func failInvalidBatchName(jsonMode bool, name string) int {
	return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid batch name %q.", name), "use the format [a-z0-9][a-z0-9-]{1,62}.")
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

// printJSON marshals value to a single stdout line, reporting a marshal failure
// on stderr with the value described as what.
func printJSON(value any, what string) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not serialize the %s: %v\n", what, err)
		return exitGeneric
	}
	fmt.Println(string(encoded))
	return 0
}

func printProject(p project, jsonMode bool) int {
	if jsonMode {
		return printJSON(p, "project")
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
		return printJSON(i, "Issue")
	}
	fmt.Println(i.ID)
	return 0
}

func printCreatedBatch(b batch, jsonMode bool) int {
	if jsonMode {
		return printJSON(createdBatch{Name: b.Name, Project: b.Project, Created: b.Created}, "Batch")
	}
	fmt.Println(b.Name)
	return 0
}

func batchListRows(st *itostore.Store, p project, batches []batch) ([]batchListRow, error) {
	rows := make([]batchListRow, 0, len(batches))
	for _, b := range batches {
		plan, err := st.ShowBatch(p, b.Name)
		if err != nil {
			var cycle *itostore.BatchCycleError
			if errors.As(err, &cycle) {
				rows = append(rows, batchListRow{Batch: b})
				continue
			}
			return nil, err
		}
		waves := len(plan.Waves)
		rows = append(rows, batchListRow{
			Batch: plan.Batch,
			Waves: &waves,
		})
	}
	return rows, nil
}

func printBatchList(rows []batchListRow, jsonMode bool) int {
	if jsonMode {
		items := make([]batchListItem, 0, len(rows))
		for _, row := range rows {
			b := row.Batch
			items = append(items, batchListItem{
				Name:    b.Name,
				Project: b.Project,
				Created: b.Created,
				Total:   b.Total,
				Done:    b.Done,
				Waves:   row.Waves,
			})
		}
		return printJSON(items, "Batch list")
	}
	if len(rows) == 0 {
		fmt.Println("no batches. create one with 'ito batch new <name>'.")
		return 0
	}
	for _, row := range rows {
		b := row.Batch
		progress := "done"
		if b.Total == 0 || b.Done != b.Total {
			progress = fmt.Sprintf("%d/%d done", b.Done, b.Total)
		}
		if row.Waves == nil {
			fmt.Printf("%s %s %s · cycle\n", b.Name, batchCreatedDate(b.Created), progress)
			continue
		}
		if *row.Waves > 0 {
			fmt.Printf("%s %s %s · wave 1/%d\n", b.Name, batchCreatedDate(b.Created), progress, *row.Waves)
			continue
		}
		fmt.Printf("%s %s %s\n", b.Name, batchCreatedDate(b.Created), progress)
	}
	return 0
}

func printRenamedBatch(oldName string, b batch, jsonMode bool) int {
	if jsonMode {
		// store.Batch's JSON tags are exactly the rename payload.
		return printJSON(b, "Batch")
	}
	fmt.Printf("%s renamed to %s.\n", oldName, b.Name)
	return 0
}

func printDeletedBatch(name string, membersCleared int, jsonMode bool) int {
	if jsonMode {
		return printJSON(deletedBatch{Deleted: name, MembersCleared: membersCleared}, "Batch deletion")
	}
	fmt.Printf("%s deleted. %d members released.\n", name, membersCleared)
	return 0
}

func printBatchShow(plan batchPlan, jsonMode bool) int {
	if jsonMode {
		item := batchShowItem{
			Name:    plan.Name,
			Project: plan.Project,
			Created: plan.Created,
			Total:   plan.Total,
			Done:    plan.Done,
			Waves:   make([]batchWaveItem, 0, len(plan.Waves)),
		}
		for _, wave := range plan.Waves {
			waveItem := batchWaveItem{
				Wave:   wave.Wave,
				Ready:  wave.Ready,
				Issues: make([]issueListItem, 0, len(wave.Issues)),
			}
			for _, issue := range wave.Issues {
				waveItem.Issues = append(waveItem.Issues, issueListJSON(issue))
			}
			item.Waves = append(item.Waves, waveItem)
		}
		return printJSON(item, "Batch")
	}

	fmt.Printf("%s %s %d/%d\n", plan.Name, batchCreatedDate(plan.Created), plan.Done, plan.Total)
	if len(plan.Waves) == 0 {
		return 0
	}
	style := newListStyle()
	for _, wave := range plan.Waves {
		state := "waiting"
		if wave.Ready {
			state = "ready"
		}
		fmt.Printf("\nWave %d · %s\n", wave.Wave, state)
		for _, issue := range wave.Issues {
			fmt.Printf("%s [%s %s] %s\n", style.issueID(issue.ID), style.status(issue.Status), style.priority(issue.Priority), issue.Title)
		}
	}
	return 0
}

func batchCreatedDate(created string) string {
	parsed, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return created
	}
	return parsed.UTC().Format("2006-01-02")
}

func printIssueDetail(i issue, jsonMode bool) int {
	if jsonMode {
		return printJSON(i, "Issue")
	}
	fmt.Printf("ID: %s\n", i.ID)
	fmt.Printf("Project: %s\n", i.Project)
	fmt.Printf("Title: %s\n", i.Title)
	fmt.Printf("Status: %s\n", i.Status)
	fmt.Printf("Priority: %s\n", i.Priority)
	fmt.Printf("Batch: %s\n", formatOptionalString(i.Batch))
	fmt.Printf("Created: %s\n", i.Created)
	fmt.Printf("Updated: %s\n", i.Updated)
	fmt.Printf("Labels: %s\n", formatList(i.Labels))
	fmt.Println("Links:")
	fmt.Printf("  blocked_by: %s\n", formatList(i.BlockedBy))
	fmt.Printf("  relates_to: %s\n", formatList(i.RelatesTo))
	fmt.Printf("  conflicts_with: %s\n", formatList(i.ConflictsWith))
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
			items = append(items, issueListJSON(i))
		}
		return printJSON(items, "Issue list")
	}
	if len(issues) == 0 {
		fmt.Println("no Issues found. adjust the filters or create one with 'ito new --title <title>'.")
		return 0
	}
	style := newListStyle()
	currentProject := ""
	for _, i := range issues {
		if allProjects && i.Project != currentProject {
			if currentProject != "" {
				fmt.Println()
			}
			currentProject = i.Project
			fmt.Printf("%s:\n", style.project(currentProject))
		}
		prefix := ""
		if allProjects {
			prefix = "  "
		}
		fmt.Printf("%s%s [%s %s] %s\n", prefix, style.issueID(i.ID), style.status(i.Status), style.priority(i.Priority), i.Title)
	}
	return 0
}

func issueListJSON(i issue) issueListItem {
	return issueListItem{
		ID:            i.ID,
		Project:       i.Project,
		Title:         i.Title,
		Status:        i.Status,
		Priority:      i.Priority,
		Labels:        i.Labels,
		BlockedBy:     i.BlockedBy,
		RelatesTo:     i.RelatesTo,
		ConflictsWith: i.ConflictsWith,
		Batch:         i.Batch,
		Created:       i.Created,
		Updated:       i.Updated,
	}
}

func formatList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return strings.Join(values, ", ")
}

func formatOptionalString(value *string) string {
	if value == nil {
		return "none"
	}
	return *value
}

func isValidValue(value string, valid map[string]struct{}) bool {
	_, ok := valid[value]
	return ok
}

// valueSet turns an ordered vocabulary slice into a membership set for validation.
func valueSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
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
	flagArgs, positionals := splitFlagsAndPositionals(args, commandValueFlags("edit"))
	issueID := ""
	if len(positionals) > 0 {
		issueID = positionals[0]
	}
	return flagArgs, issueID, len(positionals)
}

func resolveCurrentRoot() (string, bool, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := cmd.Output()
	if err == nil {
		commonDir := strings.TrimSpace(string(output))
		if filepath.Base(commonDir) == ".git" {
			root, err := itostore.CanonicalPath(filepath.Dir(commonDir))
			return root, true, err
		}
		root, err := itostore.CanonicalPath(commonDir)
		return root, true, err
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", false, fmt.Errorf("could not invoke git: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	root, err := itostore.CanonicalPath(cwd)
	return root, false, err
}
