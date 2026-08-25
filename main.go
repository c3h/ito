package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	itoconfig "github.com/c3h/ito/internal/config"
	"github.com/c3h/ito/internal/ledger"
	itostore "github.com/c3h/ito/internal/store"
	"github.com/c3h/ito/internal/tui"
	"github.com/charmbracelet/x/term"
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
	validStatuses             = valueSet(itostore.Statuses)
	validPriorities           = valueSet(itostore.Priorities)
	validCategories           = valueSet(itostore.Categories)
	validTriage               = valueSet(itostore.TriageStates)
	validLabels               = valueSet(itostore.Labels)
	isTerminal                = isatty.IsTerminal
	stdin           io.Reader = os.Stdin
	runTUI                    = tui.Run
	// openLedger dials the configured Ledger; tests replace it with an
	// in-memory one.
	openLedger = func(cfg itoconfig.Ledger) (ledger.Ledger, error) {
		return ledger.OpenTurso(cfg.URL, cfg.Token)
	}
)

// The prose enumerations for hints and help derive from the same slices, so a
// vocabulary change never leaves stale text behind. Priorities are spelled
// ascending (low first) in prose, while the slice orders by precedence.
var (
	statusList     = humanList(itostore.Statuses)
	listStatusList = humanList(append(slices.Clone(itostore.Statuses), "all"))
	priorityList   = humanList(ascendingPriorities())
	categoryList   = humanList(itostore.Categories)
	triageList     = humanList(itostore.TriageStates)
	labelList      = humanList(itostore.Labels)
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

// issueRow is the one-line Issue shape every human listing prints — the Issue
// list, and the Wave and waiting groups of "ito batch show".
func (s listStyle) issueRow(i issue) string {
	return fmt.Sprintf("%s [%s %s] %s", s.issueID(i.ID), s.status(i.Status), s.priority(i.Priority), i.Title)
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
	batch
	Waves *int `json:"waves"`
}

type batchListRow struct {
	Batch batch
	Waves *int
	// Waiting counts the members no Wave could place: gated by work outside the
	// Batch. Derived, not stored — see the Wave derivation in the store.
	Waiting int
}

type deletedBatch struct {
	Deleted        string `json:"deleted"`
	MembersCleared int    `json:"members_cleared"`
}

type movedBatch struct {
	Batch        string   `json:"batch"`
	Project      string   `json:"project"`
	TargetStatus string   `json:"target_status"`
	Changed      []string `json:"changed"`
	Skipped      []string `json:"skipped"`
}

type batchShowItem struct {
	batch
	Waves   []batchWaveItem `json:"waves"`
	Waiting []issueListItem `json:"waiting"`
}

type batchWaveItem struct {
	Wave   int             `json:"wave"`
	Ready  bool            `json:"ready"`
	Done   bool            `json:"done"`
	Issues []issueListItem `json:"issues"`
}

type issueListItem struct {
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

type configDisplay struct {
	LocalDBPath string         `json:"local_db_path"`
	Ledger      *ledgerDisplay `json:"ledger"`
}

// ledgerDisplay is the connection as shown to the user: never the token.
type ledgerDisplay struct {
	URL    string `json:"url"`
	Device string `json:"device"`
}

type connectResult struct {
	ledgerDisplay
	Pushed int `json:"pushed"`
	Pulled int `json:"pulled"`
}

type disconnectResult struct {
	URL string `json:"url"`
}

type commandFailure struct {
	code    int
	message string
	hint    string
}

func (e commandFailure) Error() string {
	return e.message
}

// openStore opens the central store, returning a ready *sql.DB (the caller
// defers Close) or a typed *commandFailure carrying the exact code/message/hint.
// OpenDefault already migrates the store it opens.
func openStore() (*sql.DB, *itostore.Store, *commandFailure) {
	db, err := itostore.OpenDefault()
	if err != nil {
		hint := "check ITO_HOME and the directory permissions."
		if errors.Is(err, itoconfig.ErrRetiredCloudBackend) {
			hint = ""
		}
		return nil, nil, &commandFailure{exitGeneric, fmt.Sprintf("could not open the central store: %v", err), hint}
	}
	st := itostore.New(db)
	// With a Ledger configured, numbers come from it (decision 0005); the
	// dial waits for the first creation, so read-only commands never dial.
	if cfg, err := itoconfig.Load(); err == nil && cfg.Ledger != nil {
		st.SetIssueNumberer(ledgerNumberer{})
	}
	return db, st, nil
}

// reserveTimeout bounds how long a creation waits for its number; unlike a
// push, a reservation that does not answer fails the command.
const reserveTimeout = 10 * time.Second

// ledgerNumberer reserves Issue numbers from the configured Ledger.
type ledgerNumberer struct{}

func (ledgerNumberer) ReserveIssueNumber(prefix string, floor int64) (int64, error) {
	return askLedger(reserveTimeout, func(l ledger.Ledger) (int64, error) {
		return l.ReserveIssueNumber(prefix, floor)
	})
}

// askLedger dials the configured Ledger and runs ask against it, giving up
// after timeout. The work runs aside so nothing on the way to the Ledger can
// hold the caller past the timeout; a call abandoned mid-flight may still
// land on the Ledger, which every caller tolerates.
func askLedger[T any](timeout time.Duration, ask func(ledger.Ledger) (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		l, _, err := connectedLedger()
		if err != nil {
			done <- outcome{err: err}
			return
		}
		value, err := ask(l)
		done <- outcome{value, err}
	}()
	select {
	case o := <-done:
		return o.value, o.err
	case <-time.After(timeout):
		var zero T
		return zero, fmt.Errorf("the Ledger did not answer within %s", timeout)
	}
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

func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return versionFromBuildInfo(info)
}

func versionFromBuildInfo(info *debug.BuildInfo) string {
	if info == nil {
		return "devel"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	revision := ""
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "devel"
	}
	revision = revision[:min(12, len(revision))]
	if dirty {
		revision += ".dirty"
	}
	return revision
}

func runCLI(args []string) int {
	if len(args) == 0 {
		tty := isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd())
		configured, err := itoconfig.Exists()
		if err != nil {
			return fail(false, exitGeneric, fmt.Sprintf("could not inspect the config: %v", err), "check ITO_HOME and the directory permissions.")
		}
		if !configured {
			if !tty {
				return fail(false, exitGeneric, "this machine has no ito config yet and only a terminal can be asked how to start.", firstRunHint)
			}
			if code := firstRun(); code != 0 {
				return code
			}
		} else if !tty {
			printRootHelp(os.Stdout)
			return 0
		}
		db, st, openFail := openStore()
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
		if err := runTUI(st, p, tui.Options{Sync: tuiSync(st)}); err != nil {
			return fail(false, exitGeneric, fmt.Sprintf("could not run the TUI: %v", err), "try again from an interactive terminal.")
		}
		return 0
	}
	if isHelpArg(args[0]) {
		printRootHelp(os.Stdout)
		return 0
	}
	if args[0] == "--version" || args[0] == "-version" {
		fmt.Printf("ito %s\n", buildVersion())
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
	case "config":
		return runConfig(args[1:])
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
	case "sync":
		return runSync(args[1:])
	case "ledger":
		return runLedger(args[1:])
	default:
		return fail(wantsJSON(args, nil), exitBadUsage, "unknown command: "+args[0]+".", "Run 'ito --help' to see available commands.")
	}
}

func runConfig(args []string) int {
	if wantsHelp(args, nil) {
		printCommandHelp("config")
		return 0
	}
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	fs.BoolVar(&jsonMode, "json", false, "")
	flagArgs, positionals := splitFlagsAndPositionals(args, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, nil), exitBadUsage, err.Error(), "run 'ito config --help' to see the accepted flags.")
	}
	if len(positionals) != 0 {
		return fail(jsonMode, exitBadUsage, "ito config takes no positional arguments.", "use only --json.")
	}

	home, err := itoconfig.HomeDir()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not resolve the ito home: %v.", err), "")
	}
	cfg, err := itoconfig.Load()
	if err != nil {
		message := fmt.Sprintf("could not read the config: %v", err)
		if !errors.Is(err, itoconfig.ErrRetiredCloudBackend) {
			message += "; fix or remove it."
		}
		return fail(jsonMode, exitGeneric, message, "")
	}
	display := configDisplay{LocalDBPath: itoconfig.LocalDBPath(home)}
	if cfg.Ledger != nil {
		// The Device identity lives in the store; a store that will not open
		// must not hide the rest of the config, so it is left blank instead.
		display.Ledger = &ledgerDisplay{URL: cfg.Ledger.URL, Device: localDeviceID()}
	}
	if jsonMode {
		return printJSON(display, "config")
	}
	fmt.Printf("Local DB: %s\n", display.LocalDBPath)
	if display.Ledger != nil {
		fmt.Printf("Ledger: %s\nDevice: %s\n", display.Ledger.URL, display.Ledger.Device)
	}
	return 0
}

func localDeviceID() string {
	db, st, openFail := openStore()
	if openFail != nil {
		return ""
	}
	defer db.Close()
	device, err := st.DeviceID()
	if err != nil {
		return ""
	}
	return device
}

func runLedger(args []string) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		printCommandHelp("ledger")
		return 0
	}
	switch args[0] {
	case "connect":
		return runLedgerConnect(args[1:])
	case "disconnect":
		return runLedgerDisconnect(args[1:])
	default:
		return fail(wantsJSON(args[1:], nil), exitBadUsage, "unknown ledger command: "+args[0]+".", "Run 'ito ledger --help' to see available commands.")
	}
}

// runLedgerConnect decides by what each side holds: an empty local store
// pulls the whole Ledger; two populated sides merge by last-writer-wins only
// under --force; a populated store against an empty Ledger snapshots the
// store into it and records the connection only once the Ledger holds every
// row. Once the policy passes the connection is recorded, then the first Sync
// runs — so an interrupted one resumes with "ito sync" rather than facing the
// policy again against a half-pulled store.
func runLedgerConnect(args []string) int {
	valueFlags := commandValueFlags("ledger connect")
	if wantsHelp(args, valueFlags) {
		printCommandHelp("ledger connect")
		return 0
	}
	fs := flag.NewFlagSet("ledger connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var url, token string
	var force, jsonMode bool
	fs.StringVar(&url, "url", "", "")
	fs.StringVar(&token, "token", "", "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&jsonMode, "json", false, "")
	flagArgs, positionals := splitFlagsAndPositionals(args, valueFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, valueFlags), exitBadUsage, err.Error(), "run 'ito ledger connect --help' to see the accepted flags.")
	}
	if len(positionals) != 0 {
		return fail(jsonMode, exitBadUsage, "ito ledger connect takes no positional arguments.", "pass the Ledger with --url and --token.")
	}
	if url == "" || token == "" {
		return fail(jsonMode, exitBadUsage, "ito ledger connect needs both --url and --token.", "run 'ito ledger connect --help' to see the accepted flags.")
	}

	result, connectFail := connectLedger(url, token, force)
	if connectFail != nil {
		return fail(jsonMode, connectFail.code, connectFail.message, connectFail.hint)
	}
	if jsonMode {
		return printJSON(result, "connect result")
	}
	fmt.Println(result.summary())
	return 0
}

func (r connectResult) summary() string {
	return fmt.Sprintf("Connected to %s as Device %s; pushed %d, pulled %d.", r.URL, r.Device, r.Pushed, r.Pulled)
}

// connectLedger is the shared path of ito ledger connect and the first-run
// choice: it checks the Ledger, applies connectPolicy, records the connection
// and runs the first sync. The config is written only once the policy passes,
// so a refused connect leaves the machine unconfigured.
func connectLedger(url, token string, force bool) (connectResult, *commandFailure) {
	cfg, err := itoconfig.Load()
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read the config: %v", err), ""}
	}
	if cfg.Ledger != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("this Device is already connected to %s.", cfg.Ledger.URL), "run 'ito ledger disconnect' first to point it elsewhere."}
	}
	connection := itoconfig.Ledger{URL: url, Token: token}
	l, err := openLedger(connection)
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not reach the Ledger: %v", err), "check the URL, the token and the network."}
	}
	if err := l.EnsureSchema(); err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not prepare the Ledger: %v", err), "check the URL, the token and the network."}
	}

	db, st, openFail := openStore()
	if openFail != nil {
		return connectResult{}, openFail
	}
	defer db.Close()
	localEmpty, err := st.Empty()
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not inspect the local store: %v", err), "try again or inspect the central store."}
	}
	device, err := st.DeviceID()
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read the Device identity: %v", err), "try again or inspect the central store."}
	}
	inventory, err := ledger.TakeInventory(l)
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not read the Ledger: %v", err), "check the URL, the token and the network."}
	}
	// A Ledger only this Device ever wrote to is one it started — a snapshot
	// that failed before the config was written — so the retry resumes it
	// instead of facing the two-populated-sides rule.
	ledgerEmpty := inventory.Empty() || inventory.WrittenOnlyBy(device)
	if policyFail := connectPolicy(localEmpty, ledgerEmpty, force); policyFail != nil {
		return connectResult{}, policyFail
	}

	if err := st.ResetSync(); err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not reset the sync state: %v", err), "try again or inspect the central store."}
	}
	snapshotted := 0
	if !localEmpty {
		if _, err := st.Snapshot(); err != nil {
			return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not snapshot the local store: %v", err), "try again or inspect the central store."}
		}
		if ledgerEmpty {
			pushed, snapshotFail := pushSnapshot(st, l)
			if snapshotFail != nil {
				return connectResult{}, snapshotFail
			}
			snapshotted = pushed
		}
	}
	if err := itoconfig.Write(itoconfig.Config{Ledger: &connection}); err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("could not write the config: %v", err), "check the ito home permissions and run 'ito ledger connect' again."}
	}
	result, err := st.Sync(l)
	pushed := snapshotted + result.Pushed
	if err != nil {
		return connectResult{}, &commandFailure{exitGeneric, fmt.Sprintf("connected, but the first sync stopped after pushing %d and pulling %d Changes: %v", pushed, result.Pulled, err), "run 'ito sync' to resume."}
	}
	return connectResult{ledgerDisplay: ledgerDisplay{URL: url, Device: device}, Pushed: pushed, Pulled: result.Pulled}, nil
}

// pushSnapshot writes the completed change log into an empty Ledger — in the
// Ledger's batches — then checks that the Ledger folds back to the rows the
// store holds. Nothing here touches the config, so a failure leaves the
// Device disconnected and the next connect resumes: the log keeps its
// Changes and the Ledger deduplicates what it already received.
func pushSnapshot(st *itostore.Store, l ledger.Ledger) (int, *commandFailure) {
	pushed, err := st.Push(l)
	if err != nil {
		return 0, &commandFailure{exitGeneric, fmt.Sprintf("could not push the snapshot to the Ledger: %v", err), "check the URL, the token and the network, then run 'ito ledger connect' again; the Ledger keeps what it received and the next attempt sends the rest."}
	}
	local, err := st.RowCounts()
	if err != nil {
		return 0, &commandFailure{exitGeneric, fmt.Sprintf("could not count the local rows: %v", err), "try again or inspect the central store."}
	}
	inventory, err := ledger.TakeInventory(l)
	if err != nil {
		return 0, &commandFailure{exitGeneric, fmt.Sprintf("could not read the snapshot back from the Ledger: %v", err), "check the URL, the token and the network, then run 'ito ledger connect' again."}
	}
	if !maps.Equal(local, inventory.Rows) {
		return 0, &commandFailure{exitGeneric, fmt.Sprintf("the snapshot did not validate: the local store holds %s, the Ledger reports %s.", describeRowCounts(local), describeRowCounts(inventory.Rows)), "the connection was not recorded; run 'ito ledger connect' again or inspect the Ledger."}
	}
	return pushed, nil
}

func describeRowCounts(counts map[string]int) string {
	parts := make([]string, 0, len(ledger.Kinds))
	for _, kind := range ledger.Kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	return strings.Join(parts, ", ")
}

// firstRunHint names the explicit commands behind the two first-run choices,
// for every path that cannot or will not ask.
const firstRunHint = "connect to an existing Ledger with 'ito ledger connect --url <url> --token <token>', or start empty from a terminal with 'ito'; 'ito --help' lists the commands."

// firstRun asks, on a terminal with no config, whether this machine starts
// empty or joins an existing Ledger; either answer leaves a config behind so
// the question is asked once. This is the only prompt in ito (decision 0005).
func firstRun() int {
	fmt.Println("This machine has no ito config yet.")
	fmt.Println("  1) Start empty — a local store on this machine")
	fmt.Println("  2) Connect to an existing Ledger — pull the tracker from Turso")
	choice, err := promptLine("Choose [1/2]: ")
	if err != nil {
		return fail(false, exitBadUsage, "no choice was made.", firstRunHint)
	}
	switch choice {
	case "1":
		if err := itoconfig.Write(itoconfig.Config{}); err != nil {
			return fail(false, exitGeneric, fmt.Sprintf("could not write the config: %v", err), "check the ito home permissions.")
		}
		return 0
	case "2":
		url, err := promptLine("Ledger URL: ")
		if err != nil || url == "" {
			return fail(false, exitBadUsage, "no Ledger URL was given.", firstRunHint)
		}
		token, err := promptSecret("Ledger token: ")
		if err != nil || token == "" {
			return fail(false, exitBadUsage, "no Ledger token was given.", firstRunHint)
		}
		result, connectFail := connectLedger(url, token, false)
		if connectFail != nil {
			return fail(false, connectFail.code, connectFail.message, connectFail.hint)
		}
		fmt.Println(result.summary())
		return 0
	default:
		return fail(false, exitBadUsage, fmt.Sprintf("%q is not one of the choices.", choice), firstRunHint)
	}
}

// promptLine reads one line byte by byte so nothing past it is buffered away
// from stdin: promptSecret then reads the same terminal through its fd.
func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := stdin.Read(buf)
		if n == 1 && buf[0] == '\n' {
			break
		}
		line = append(line, buf[:n]...)
		if err != nil {
			if len(line) == 0 {
				return "", err
			}
			break
		}
	}
	return strings.TrimSpace(string(line)), nil
}

// promptSecret reads without echo when stdin is a real terminal, so the token
// never lands in the scrollback; any other reader is read as a plain line.
func promptSecret(prompt string) (string, error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f.Fd()) {
		fmt.Print(prompt)
		secret, err := term.ReadPassword(f.Fd())
		fmt.Println()
		return strings.TrimSpace(string(secret)), err
	}
	return promptLine(prompt)
}

// connectPolicy is the state table of ito ledger connect; nil means the
// connection may proceed.
func connectPolicy(localEmpty, ledgerEmpty, force bool) *commandFailure {
	switch {
	case localEmpty:
		return nil
	case ledgerEmpty:
		return nil
	case !force:
		return &commandFailure{exitGeneric, "both the local store and the Ledger already hold Issues.", "pass --force to merge them, keeping the later version of every row that both changed."}
	default:
		return nil
	}
}

func runLedgerDisconnect(args []string) int {
	if wantsHelp(args, nil) {
		printCommandHelp("ledger disconnect")
		return 0
	}
	fs := flag.NewFlagSet("ledger disconnect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	fs.BoolVar(&jsonMode, "json", false, "")
	flagArgs, positionals := splitFlagsAndPositionals(args, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, nil), exitBadUsage, err.Error(), "run 'ito ledger disconnect --help' to see the accepted flags.")
	}
	if len(positionals) != 0 {
		return fail(jsonMode, exitBadUsage, "ito ledger disconnect takes no positional arguments.", "use only --json.")
	}

	cfg, err := itoconfig.Load()
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not read the config: %v", err), "")
	}
	if cfg.Ledger == nil {
		return fail(jsonMode, exitGeneric, "no Ledger is connected.", "connect one with 'ito ledger connect --url <url> --token <token>'.")
	}
	url := cfg.Ledger.URL
	if err := itoconfig.Write(itoconfig.Config{}); err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not write the config: %v", err), "check the ito home permissions.")
	}
	if jsonMode {
		return printJSON(disconnectResult{URL: url}, "disconnect result")
	}
	fmt.Printf("Disconnected from %s; the local store keeps every row.\n", url)
	return 0
}

// syncTimeout bounds how long the TUI's background sync may run before the
// indicator gives up on it; a Ledger that answers later is only abandoned.
var syncTimeout = 30 * time.Second

// tuiSync is the background sync the TUI runs on launch and on refresh, or nil
// when no Ledger is connected so the TUI never dials anything. Reading the
// config here keeps the decision cheap; the dial itself waits for the sync.
// Like pushAfterWrite, the sync runs aside and is abandoned on timeout — the
// Ledger deduplicates resends, so the next sync resumes cleanly. A sync still
// running when the TUI quits meets a closed store and fails the same way.
func tuiSync(st *itostore.Store) tui.SyncFunc {
	cfg, err := itoconfig.Load()
	if err != nil || cfg.Ledger == nil {
		return nil
	}
	return func() (itostore.SyncResult, error) {
		return askLedger(syncTimeout, st.Sync)
	}
}

// pushTimeout bounds how long a writing command waits for its push; a Ledger
// that is slow or unreachable costs the command at most this much.
var pushTimeout = 5 * time.Second

// connectedLedger dials the Ledger the config names; connected is false when
// none is configured.
func connectedLedger() (l ledger.Ledger, connected bool, err error) {
	cfg, err := itoconfig.Load()
	if err != nil {
		return nil, false, fmt.Errorf("could not read the config: %w", err)
	}
	if cfg.Ledger == nil {
		return nil, false, nil
	}
	l, err = openLedger(*cfg.Ledger)
	if err != nil {
		return nil, true, fmt.Errorf("could not reach the Ledger: %w", err)
	}
	return l, true, nil
}

// pushAfterWrite sends the Changes a writing command just committed to the
// connected Ledger, so the other Devices see them on their next Sync without
// this one running "ito sync". It never alters the command's outcome: any
// failure is one warning on stderr and the Changes stay pending. Without a
// connected Ledger it does nothing.
func pushAfterWrite(st *itostore.Store) {
	// Dial and push run aside so nothing on the way to the Ledger can hold
	// the command past the timeout. A push abandoned mid-flight is harmless:
	// the Ledger deduplicates resends, and a Change only stops being pending
	// once the local marking after a confirmed append succeeds.
	done := make(chan error, 1)
	go func() {
		l, connected, err := connectedLedger()
		if err == nil && connected {
			_, err = st.Push(l)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			warnPush(err.Error())
		}
	case <-time.After(pushTimeout):
		warnPush(fmt.Sprintf("the push to the Ledger timed out after %s", pushTimeout))
	}
}

func warnPush(cause string) {
	fmt.Fprintf(os.Stderr, "warning: %s; the Changes stay pending until the next 'ito sync'.\n", cause)
}

func runSync(args []string) int {
	if wantsHelp(args, nil) {
		printCommandHelp("sync")
		return 0
	}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	fs.BoolVar(&jsonMode, "json", false, "")
	flagArgs, positionals := splitFlagsAndPositionals(args, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, nil), exitBadUsage, err.Error(), "run 'ito sync --help' to see the accepted flags.")
	}
	if len(positionals) != 0 {
		return fail(jsonMode, exitBadUsage, "ito sync takes no positional arguments.", "use only --json.")
	}

	l, connected, err := connectedLedger()
	if err != nil {
		return fail(jsonMode, exitGeneric, err.Error(), "check the network and 'ito config'.")
	}
	if !connected {
		return fail(jsonMode, exitGeneric, "no Ledger is connected, so there is nothing to sync with.", "connect one with 'ito ledger connect --url <url> --token <token>'.")
	}

	db, st, openFail := openStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	result, err := st.Sync(l)
	if err != nil {
		return fail(jsonMode, exitGeneric, fmt.Sprintf("sync stopped after pushing %d and pulling %d Changes: %v", result.Pushed, result.Pulled, err), "run 'ito sync' again to resume.")
	}
	if jsonMode {
		return printJSON(result, "sync result")
	}
	fmt.Printf("Pushed %d, pulled %d.\n", result.Pushed, result.Pulled)
	return 0
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
	case "move":
		return runBatchMove(args[1:])
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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

	db, st, openFail := openStore()
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

func runBatchMove(args []string) int {
	if wantsHelp(args, commandValueFlags("batch move")) {
		printCommandHelp("batch move")
		return 0
	}
	fs := flag.NewFlagSet("batch move", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var jsonMode bool
	var projectName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch move")), exitBadUsage, err.Error(), "run 'ito batch move --help' to see the accepted flags.")
	}
	if len(positionals) != 2 {
		return fail(jsonMode, exitBadUsage, "ito batch move takes exactly one Batch name and a target status.", "use: ito batch move <name> <status>.")
	}
	name, targetStatus := positionals[0], positionals[1]
	if !isValidValue(targetStatus, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", targetStatus), "use "+statusList+".")
	}
	if projectName != "" && !itostore.ProjectNamePattern.MatchString(projectName) {
		return failInvalidProjectName(jsonMode, projectName)
	}

	db, st, openFail := openStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	moved, err := st.MoveBatch(p, name, targetStatus)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", name, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not move Batch %q: %v", name, err), "try again or inspect the central store.")
	}
	pushAfterWrite(st)
	return printMovedBatch(moved, jsonMode)
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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
	var includeDone bool
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.BoolVar(&includeDone, "include-done", false, "")
	flagArgs, positionals := splitFlagsAndPositionals(args, map[string]struct{}{"project": {}})
	if err := fs.Parse(flagArgs); err != nil {
		return fail(wantsJSON(args, commandValueFlags("batch show")), exitBadUsage, err.Error(), "run 'ito batch show --help' to see the accepted flags.")
	}
	if len(positionals) != 1 {
		return fail(jsonMode, exitBadUsage, "ito batch show takes exactly one name.", "use: ito batch show <name>.")
	}

	db, st, openFail := openStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	plan, err := st.ShowBatchWithOptions(p, positionals[0], itostore.ShowBatchOptions{IncludeDone: includeDone})
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

	db, st, openFail := openStore()
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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
	var category string
	var triageState string
	var labels stringSliceFlag
	var body string
	var batchName string
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&status, "status", "backlog", "")
	fs.StringVar(&priority, "priority", "low", "")
	fs.StringVar(&category, "category", "uncategorized", "")
	fs.StringVar(&triageState, "triage-state", "needs-triage", "")
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
	if !isValidValue(category, validCategories) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid category %q.", category), "use "+categoryList+".")
	}
	if !isValidValue(triageState, validTriage) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid triage state %q.", triageState), "use "+triageList+".")
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

	db, st, openFail := openStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	p, code, message, hint := resolveProject(st, projectName)
	if code != 0 {
		return fail(jsonMode, code, message, hint)
	}
	created, err := st.CreateIssueInBatchWithMetadata(p, title, status, priority, category, triageState, labels, body, batchName)
	if err != nil {
		if errors.Is(err, itostore.ErrBatchNotFound) {
			return fail(jsonMode, exitNotFound, fmt.Sprintf("Batch %q not found in Project %q.", batchName, p.Name), "run 'ito batch list' to see the Project's Batches.")
		}
		var reservation *itostore.ReservationError
		if errors.As(err, &reservation) {
			return fail(jsonMode, exitGeneric, fmt.Sprintf("could not reserve an Issue number from the Ledger: %v", reservation.Err), "check the network and try again; nothing was created.")
		}
		return fail(jsonMode, exitGeneric, fmt.Sprintf("could not create the Issue: %v", err), "try again or inspect the central store.")
	}
	pushAfterWrite(st)
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

	db, st, openFail := openStore()
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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
	var category string
	var triageState string
	var branch string
	var body string
	var batchName string
	var labelOps []labelEditOp
	var linkOps []linkEditOp
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&category, "category", "", "")
	fs.StringVar(&triageState, "triage-state", "", "")
	fs.StringVar(&branch, "branch", "", "")
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
		case "category":
			options.CategorySet = true
		case "triage-state":
			options.TriageStateSet = true
		case "branch":
			options.BranchSet = true
		case "body":
			options.BodySet = true
		case "batch":
			options.BatchSet = true
		}
	})
	if !options.TitleSet && !options.PrioritySet && !options.CategorySet && !options.TriageStateSet && !options.BranchSet && !options.BodySet && !options.BatchSet && len(options.LabelOps) == 0 && len(options.LinkOps) == 0 {
		return fail(jsonMode, exitBadUsage, "no changes requested.", "use at least one flag like --title, --priority, --category, --triage-state, --branch, --body, --batch, --add-label, --block, --relate or --conflict.")
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
	if options.CategorySet {
		if !isValidValue(category, validCategories) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid category %q.", category), "use "+categoryList+".")
		}
		options.Category = category
	}
	if options.TriageStateSet {
		if !isValidValue(triageState, validTriage) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid triage state %q.", triageState), "use "+triageList+".")
		}
		options.TriageState = triageState
	}
	if options.BranchSet {
		options.Branch = branch
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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

	db, st, openFail := openStore()
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
	pushAfterWrite(st)
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
	var category string
	var triageState string
	var search string
	var batchName string
	var ready bool
	var labels stringSliceFlag
	fs.BoolVar(&jsonMode, "json", false, "")
	fs.StringVar(&projectName, "project", "", "")
	fs.BoolVar(&allProjects, "all-projects", false, "")
	fs.StringVar(&status, "status", "", "")
	fs.StringVar(&priority, "priority", "", "")
	fs.StringVar(&category, "category", "", "")
	fs.StringVar(&triageState, "triage-state", "", "")
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
	// "all" is a pseudo-status: it stands for no status filter plus the done
	// Issues the default hides, so nothing past this point knows the word.
	includeDone := status == "all"
	if includeDone {
		status = ""
	}
	if status != "" && !isValidValue(status, validStatuses) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid status %q.", status), "use "+listStatusList+".")
	}
	if priority != "" && !isValidValue(priority, validPriorities) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid priority %q.", priority), "use "+priorityList+".")
	}
	if category != "" && !isValidValue(category, validCategories) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid category %q.", category), "use "+categoryList+".")
	}
	if triageState != "" && !isValidValue(triageState, validTriage) {
		return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid triage state %q.", triageState), "use "+triageList+".")
	}
	for _, label := range labels {
		if !isValidValue(label, validLabels) {
			return fail(jsonMode, exitBadUsage, fmt.Sprintf("invalid label %q.", label), "use "+labelList+".")
		}
	}

	db, st, openFail := openStore()
	if openFail != nil {
		return fail(jsonMode, openFail.code, openFail.message, openFail.hint)
	}
	defer db.Close()

	options := listOptions{
		AllProjects: allProjects,
		Status:      status,
		IncludeDone: includeDone,
		Priority:    priority,
		Category:    category,
		TriageState: triageState,
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
		return map[string]struct{}{"project": {}, "title": {}, "status": {}, "priority": {}, "category": {}, "triage-state": {}, "label": {}, "body": {}, "batch": {}}
	case "show":
		return map[string]struct{}{"project": {}}
	case "list":
		return map[string]struct{}{"project": {}, "status": {}, "priority": {}, "category": {}, "triage-state": {}, "search": {}, "label": {}, "batch": {}}
	case "batch new", "batch list", "batch move", "batch rename", "batch rm", "batch show":
		return map[string]struct{}{"project": {}}
	case "move":
		return map[string]struct{}{"project": {}}
	case "edit":
		return map[string]struct{}{
			"project": {}, "title": {}, "priority": {}, "category": {}, "triage-state": {}, "branch": {}, "body": {},
			"batch":     {},
			"add-label": {}, "remove-label": {},
			"block": {}, "unblock": {}, "relate": {}, "unrelate": {}, "conflict": {}, "unconflict": {},
		}
	case "rm":
		return map[string]struct{}{"project": {}}
	case "prune":
		return map[string]struct{}{"project": {}, "status": {}}
	case "ledger connect":
		return map[string]struct{}{"url": {}, "token": {}}
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

Local, agent-driven issue tracker. Issues are rows in a central SQLite store at ~/.ito/ito.db (override with ITO_HOME) — never inside your repo.

A Project is resolved from the git root, so every worktree shares it: run "ito init" once per repo, then address another Project from any cwd with --project <name> (or "ito list --all-projects" to span all). An Issue's full ID is <PREFIX>-<n> such as AUTH-12; the Prefix is chosen per Project at init. Issues carry execution status plus priority, category, triage state and Labels for agent-readable filtering. A Batch is a named set of Issues planned together, and its Waves ("ito batch show") are the link-graph generations safe to run in parallel.

Commands:
  init     Registers or re-points the Project for the current directory.
  new      Creates an Issue in the current Project.
  list     Lists Issues.
  config   Shows where the store lives and which Ledger is connected.
  batch    Manages Batches in the current Project.
  show     Shows an Issue by full ID.
  move     Moves an Issue to another status.
  edit     Edits an Issue's fields, Labels and Links.
  rm       Deletes an Issue.
  prune    Deletes Issues in bulk with an explicit filter.
  rename   Renames the current Project.
  sync     Exchanges Changes with the connected Ledger.
  ledger   Connects this Device to a Ledger, or disconnects it.

Every command takes --json and never prompts; failures exit non-zero (2 usage, 3 not found, 4 no Project here) with an actionable sentence on stderr. Bare "ito" in a TTY opens the TUI, and is the one place that asks: on a machine with no config it first offers, once, to start empty or connect to a Ledger; outside a TTY that question becomes an error naming "ito ledger connect". "ito --version" prints the build.

Use "ito <command> --help" to see the command's flags.`)
}

func printCommandHelp(command string) {
	switch command {
	case "config":
		fmt.Println(`usage: ito config [--json]

Shows the local database path and, when a Ledger is connected, its URL and this Device's identity. The token is never shown.

Flags:
  --json               Prints {"local_db_path": ..., "ledger": {"url": ..., "device": ...} | null}.`)
	case "ledger":
		fmt.Println(`usage: ito ledger <connect|disconnect> [flags]

Manages this Device's connection to a Ledger, the shared change log that machines sync through.

Commands:
  connect      Connects to a Turso Ledger and pulls the tracker it holds.
  disconnect   Forgets the connection; the local store keeps every row.

Use "ito ledger <command> --help" to see the command's flags.`)
	case "ledger connect":
		fmt.Println(`usage: ito ledger connect --url <url> --token <token> [--force] [--json]

Connects this Device to the Turso-hosted Ledger at <url>, creating its tables when they do not exist yet. An empty local store pulls the whole tracker. A populated local store against an empty Ledger snapshots every Project, Issue, Link, Label and Batch into it — pushed in batches and validated by per-kind row counts before the connection is recorded — so any other Device can then bootstrap from the Ledger. When both the local store and the Ledger already hold Issues the command refuses unless --force is passed, which merges them keeping the later version of every row. The connection is recorded once those checks pass, then the first Sync runs ("ito sync" resumes it if interrupted); the token is stored in the config file with owner-only permissions. Bare "ito" on a machine with no config offers the same connection interactively.

Flags:
  --url <url>          The Ledger's libsql:// URL.
  --token <token>      The auth token for that Ledger.
  --force              Merges a populated local store with a populated Ledger.
  --json               Prints {"url": ..., "device": ..., "pushed": n, "pulled": n}.`)
	case "ledger disconnect":
		fmt.Println(`usage: ito ledger disconnect [--json]

Drops the Ledger connection from the config. Local rows stay; "ito sync" fails until a Ledger is connected again.

Flags:
  --json               Prints {"url": ...}.`)
	case "sync":
		fmt.Println(`usage: ito sync [--json]

Pushes this Device's pending Changes to the connected Ledger, then pulls and applies the ones it has not seen. Rows changed on two Devices converge to the later "updated". Prints how many Changes went each way; fails when no Ledger is connected. Writing commands already push right after they commit, so a manual sync is mostly for pulling and for resending what a failed push left pending.

Flags:
  --json               Prints {"pushed": n, "pulled": n}.`)
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
		fmt.Printf(`usage: ito new --title <title> [--status <status>] [--priority <priority>] [--category <category>] [--triage-state <state>] [--label <label>] [--body <text>|-] [--batch <name>] [--project <name>] [--json]

Creates an Issue and prints the ID in human mode.
With a Ledger connected, the number is reserved from the Ledger so it stays sequential across Devices; an unreachable Ledger fails the command and creates nothing. Without one, numbering is local.
Status tracks execution; triage state tracks whether the Issue needs triage, is ready for an agent, needs human decision, or is wontfix. Category describes the kind of work and is separate from Labels.

Flags:
  --title <title>          Title is required.
  --status <status>        %s. Default: backlog.
  --priority <priority>    %s. Default: low.
  --category <category>    %s. Default: uncategorized.
  --triage-state <state>   %s. Default: needs-triage.
  --label <label>          Repeatable initial Label: %s.
  --body <text>|-          Markdown body. Use "-" to read stdin.
  --batch <name>           Assign the Issue to an existing Batch.
  --project <name>         Explicit Project.
  --json                   Prints JSON.
`, statusList, priorityList, categoryList, triageList, labelList)
	case "show":
		fmt.Println(`usage: ito show [--project <name>] [--json] <PREFIX>-<n>

Shows an Issue by full ID.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "list":
		fmt.Printf(`usage: ito list [--ready] [--status <status>] [--priority <priority>] [--category <category>] [--triage-state <state>] [--label <label>] [--search <text>] [--batch <name>] [--project <name>|--all-projects] [--json]

Lists Issues in the current Project. Issues in done are hidden by default; --status all shows every status, including done.
Use --ready to list the backlog/todo frontier whose blockers are all done and conflicts_with partners are not unsafe to start in parallel; an agent can fan out one git worktree per ready Issue.
Status filters execution; triage state filters triage/readiness metadata.

Flags:
  --ready                  Filter to backlog/todo Issues whose blockers are done and conflicts allow parallel work.
  --status <status>        Filter by %s; all shows every status, including done.
  --priority <priority>    Filter by %s.
  --category <category>    Filter by %s.
  --triage-state <state>   Filter by %s.
  --label <label>          Filter by Label. Repeatable.
  --search <text>          Full-text search in title and body.
  --batch <name>           Filter to Issues assigned to an existing Batch.
  --project <name>         Explicit Project.
  --all-projects           Lists all Projects.
  --json                   Prints JSON.
`, listStatusList, priorityList, categoryList, triageList)
	case "batch":
		fmt.Println(`usage: ito batch <command> [flags]

Manages Batches in the current Project.

Commands:
  new      Creates a Batch.
  list     Lists Batches newest-first.
  move     Moves every member Issue to a status.
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
	case "batch move":
		fmt.Printf(`usage: ito batch move [--project <name>] [--json] <name> <status>

Moves every Issue in a Batch to a valid status and reports changed and skipped IDs.

Flags:
  <status>             %s.
  --project <name>     Explicit Project.
  --json               Prints JSON.
`, statusList)
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
		fmt.Println(`usage: ito batch show [--project <name>] [--include-done] [--json] <name>

Shows the Batch's open members grouped by derived Waves and external waiting, plus done/total progress.
Use --include-done to include historical completed Waves.

Flags:
  --project <name>     Explicit Project.
  --include-done       Includes completed members in historical Waves.
  --json               Prints JSON.`)
	case "move":
		fmt.Println(`usage: ito move [--project <name>] [--json] <PREFIX>-<n> <status>

Moves an Issue to any valid status.

Flags:
  --project <name>     Validates that the Issue belongs to the given Project.
  --json               Prints JSON.`)
	case "edit":
		fmt.Printf(`usage: ito edit <PREFIX>-<n> [--title <title>] [--priority <priority>] [--category <category>] [--triage-state <state>] [--branch <name>|--branch ""] [--body <text>|-] [--batch <name>|--batch ""] [--add-label <label>] [--remove-label <label>] [--block <ID>] [--unblock <ID>] [--relate <ID>] [--unrelate <ID>] [--conflict <ID>] [--unconflict <ID>] [--project <name>] [--json]

Edits an Issue. Requires at least one change.
Status is changed with ito move; triage state is changed here because it describes review/readiness metadata, not execution progress.

Flags:
  --title <title>          New title.
  --priority <priority>    %s.
  --category <category>    %s.
  --triage-state <state>   %s.
  --branch <name>|""       Set the working branch, or clear it with "".
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
`, priorityList, categoryList, triageList)
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

// errorSentence ends a message the way every ito error reads, so the ones that
// come from elsewhere (the flag package's, above all) match the hand-written
// ones: an actionable sentence, then the hint.
func errorSentence(message string) string {
	message = strings.TrimSpace(message)
	if message == "" || strings.ContainsAny(message[len(message)-1:], ".!?") {
		return message
	}
	return message + "."
}

func fail(jsonMode bool, code int, message string, hint string) int {
	message = errorSentence(message)
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
			Batch:   plan.Batch,
			Waves:   &waves,
			Waiting: len(plan.Waiting),
		})
	}
	return rows, nil
}

func printBatchList(rows []batchListRow, jsonMode bool) int {
	if jsonMode {
		items := make([]batchListItem, 0, len(rows))
		for _, row := range rows {
			b := row.Batch
			items = append(items, batchListItem{batch: b, Waves: row.Waves})
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
		if !b.Complete() {
			progress = fmt.Sprintf("%d/%d done", b.Done, b.Total)
		}
		state := ""
		switch {
		case row.Waves == nil:
			state = " · cycle"
		case *row.Waves > 0:
			state = fmt.Sprintf(" · wave 1/%d", *row.Waves)
		case row.Waiting > 0:
			state = " · waiting"
		}
		fmt.Printf("%s %s %s%s\n", b.Name, b.Date(), progress, state)
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

func printMovedBatch(result itostore.BatchMoveResult, jsonMode bool) int {
	if jsonMode {
		return printJSON(movedBatch{
			Batch:        result.Batch,
			Project:      result.Project,
			TargetStatus: result.TargetStatus,
			Changed:      result.Changed,
			Skipped:      result.Skipped,
		}, "Batch move")
	}
	fmt.Printf("%s moved %d Issues to %s. changed: %s. skipped: %s.\n",
		result.Batch, len(result.Changed), result.TargetStatus, formatList(result.Changed), formatList(result.Skipped))
	return 0
}

func printBatchShow(plan batchPlan, jsonMode bool) int {
	if jsonMode {
		item := batchShowItem{
			batch:   plan.Batch,
			Waves:   make([]batchWaveItem, 0, len(plan.Waves)),
			Waiting: make([]issueListItem, 0, len(plan.Waiting)),
		}
		for _, wave := range plan.Waves {
			waveItem := batchWaveItem{
				Wave:   wave.Wave,
				Ready:  wave.Ready,
				Done:   wave.Done,
				Issues: make([]issueListItem, 0, len(wave.Issues)),
			}
			for _, issue := range wave.Issues {
				waveItem.Issues = append(waveItem.Issues, issueListJSON(issue))
			}
			item.Waves = append(item.Waves, waveItem)
		}
		for _, issue := range plan.Waiting {
			item.Waiting = append(item.Waiting, issueListJSON(issue))
		}
		return printJSON(item, "Batch")
	}

	fmt.Printf("%s %s %d/%d\n", plan.Name, plan.Date(), plan.Done, plan.Total)
	if len(plan.Waves) == 0 && len(plan.Waiting) == 0 {
		return 0
	}
	style := newListStyle()
	for _, wave := range plan.Waves {
		state := "waiting"
		if wave.Done {
			state = "done"
		} else if wave.Ready {
			state = "ready"
		}
		fmt.Printf("\nWave %d · %s\n", wave.Wave, state)
		printBatchGroup(style, wave.Issues)
	}
	if len(plan.Waiting) > 0 {
		fmt.Println("\nWaiting · blocked outside the Batch")
		printBatchGroup(style, plan.Waiting)
	}
	return 0
}

// printBatchGroup prints the members under a Wave or waiting heading.
func printBatchGroup(style listStyle, issues []issue) {
	for _, i := range issues {
		fmt.Println(style.issueRow(i))
	}
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
	fmt.Printf("Category: %s\n", i.Category)
	fmt.Printf("Triage state: %s\n", i.TriageState)
	if i.Branch != "" {
		fmt.Printf("Branch: %s\n", i.Branch)
	}
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
		fmt.Println(prefix + style.issueRow(i))
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
		Category:      i.Category,
		TriageState:   i.TriageState,
		Branch:        i.Branch,
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
