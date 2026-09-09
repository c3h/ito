package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func newSyncTestModel(t *testing.T, sync SyncFunc) (model, *store.Store, store.Project) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st := store.New(db)
	project, err := st.CreateProject("sync-app", "SYN", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.CreateIssue(project, store.NewIssue{Title: "Local before sync", Status: "todo", Priority: "medium"}); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	return newModel(st, project, Options{Sync: sync}), st, project
}

func TestTUIRendersLocalIssuesWhileSyncIsPending(t *testing.T) {
	release := make(chan struct{})
	m, _, _ := newSyncTestModel(t, func() (store.SyncResult, error) {
		<-release
		return store.SyncResult{}, nil
	})

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned no sync command with a Ledger connected")
	}
	// The command blocks on the Ledger while the model keeps rendering.
	landed := make(chan tea.Msg, 1)
	go func() { landed <- cmd() }()
	defer func() {
		close(release)
		if _, ok := (<-landed).(syncMsg); !ok {
			t.Error("the sync command did not produce a syncMsg")
		}
	}()
	if !m.syncing {
		t.Fatal("model is not marked syncing while the command is pending")
	}
	view := m.View()
	if !strings.Contains(view, "Local before sync") {
		t.Fatalf("initial view lacks the local Issue:\n%s", view)
	}
	if !strings.Contains(view, "syncing") {
		t.Fatalf("initial view lacks the sync indicator:\n%s", view)
	}
}

func TestTUIWithoutLedgerNeverSyncs(t *testing.T) {
	m, _, _ := newSyncTestModel(t, nil)
	if cmd := m.Init(); cmd != nil {
		t.Fatal("Init() scheduled a sync without a Ledger")
	}
	if m.syncing || strings.Contains(m.View(), "syncing") {
		t.Fatal("model shows a sync indicator without a Ledger")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Fatal("refresh scheduled a command without a Ledger or PR candidates")
	}
	if updated.(model).syncing {
		t.Fatal("refresh marked the model syncing without a Ledger")
	}
}

func TestSyncWithPulledChangesReloadsKeepingSelection(t *testing.T) {
	m, st, project := newSyncTestModel(t, func() (store.SyncResult, error) {
		return store.SyncResult{Pulled: 1}, nil
	})
	m.Init()
	// A Change pulled by the sync lands in the store before its message does;
	// the model still shows the pre-sync snapshot until it applies the result.
	if _, err := st.CreateIssue(project, store.NewIssue{Title: "Pulled from the Ledger", Status: "todo", Priority: "high"}); err != nil {
		t.Fatalf("create pulled issue: %v", err)
	}
	m.cursor().moveSelection(1)
	if strings.Contains(m.View(), "Pulled from the Ledger") {
		t.Fatal("the model reloaded before the sync result arrived")
	}
	selected, ok := m.selectedIssue()
	if !ok || selected.Title != "Local before sync" {
		t.Fatalf("selected before sync = %+v", selected)
	}

	updated, _ := m.Update(syncMsg{Project: project, Result: store.SyncResult{Pulled: 1}})
	m = updated.(model)
	if m.syncing {
		t.Fatal("model still syncing after the result")
	}
	view := m.View()
	if !strings.Contains(view, "Pulled from the Ledger") {
		t.Fatalf("view lacks the pulled Issue after sync:\n%s", view)
	}
	if strings.Contains(view, "syncing") {
		t.Fatalf("indicator still shown after sync:\n%s", view)
	}
	if selected, ok := m.selectedIssue(); !ok || selected.Title != "Local before sync" {
		t.Fatalf("selection moved after reload: %+v", selected)
	}
	m.mode = viewBoard
	m.width, m.height = 160, 40
	if view := m.View(); !strings.Contains(view, "Pulled from the") {
		t.Fatalf("the Board missed the pulled Issue:\n%s", view)
	}
}

func TestSyncWithNothingPulledChangesNothingVisible(t *testing.T) {
	m, st, project := newSyncTestModel(t, func() (store.SyncResult, error) {
		return store.SyncResult{Pushed: 2}, nil
	})
	m.Init()
	before := m.View()
	// A write that bypassed the model proves the model did not reload: a sync
	// that pulled nothing has no reason to re-read the store.
	if _, err := st.CreateIssue(project, store.NewIssue{Title: "Written aside", Status: "todo", Priority: "low"}); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	updated, _ := m.Update(syncMsg{Project: project, Result: store.SyncResult{Pushed: 2}})
	m = updated.(model)
	after := m.View()
	if strings.Contains(after, "Written aside") {
		t.Fatal("a sync that pulled nothing reloaded the model")
	}
	if strings.Contains(after, "syncing") || !strings.Contains(before, "syncing") {
		t.Fatal("the indicator did not go from shown to hidden")
	}
	if m.note != "" {
		t.Fatalf("a quiet sync left a notice: %q", m.note)
	}
}

func TestFailedSyncShowsDismissibleNoticeAndKeepsInputWorking(t *testing.T) {
	m, _, project := newSyncTestModel(t, func() (store.SyncResult, error) {
		return store.SyncResult{}, errors.New("pull: connection refused")
	})
	m.Init()
	updated, _ := m.Update(syncMsg{Project: project, Err: errors.New("pull: connection refused")})
	m = updated.(model)
	if m.syncing {
		t.Fatal("model still syncing after a failure")
	}
	if m.loadErr != nil {
		t.Fatalf("a failed sync became a load error: %v", m.loadErr)
	}
	view := m.View()
	if !strings.Contains(view, "sync failed: pull: connection refused") {
		t.Fatalf("view lacks the failure notice:\n%s", view)
	}
	if !strings.Contains(view, "Local before sync") {
		t.Fatalf("failure hid the local Issues:\n%s", view)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	m = updated.(model)
	if m.mode != viewBatches {
		t.Fatalf("mode after 2 = %q, want batches", m.mode)
	}
	if strings.Contains(m.View(), "sync failed") {
		t.Fatal("the notice survived a key press")
	}
}

func TestRefreshKeyTriggersSyncWhenLedgerConnected(t *testing.T) {
	calls := 0
	m, _, project := newSyncTestModel(t, func() (store.SyncResult, error) {
		calls++
		return store.SyncResult{}, nil
	})
	// While the launch sync is still in flight, r must not start a second one.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Fatal("refresh started a second sync while one was running")
	}
	updated, _ = updated.(model).Update(syncMsg{Project: project})
	updated, cmd = updated.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("refresh returned no command with a Ledger connected")
	}
	if !m.syncing {
		t.Fatal("refresh did not mark the model syncing")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				msg = c()
			}
		}
	}
	if _, ok := msg.(syncMsg); !ok {
		t.Fatalf("refresh command produced %T, want syncMsg", msg)
	}
	if calls != 1 {
		t.Fatalf("sync ran %d times, want 1", calls)
	}
}

func TestSyncResultForAnotherProjectIsDropped(t *testing.T) {
	m, _, _ := newSyncTestModel(t, func() (store.SyncResult, error) {
		return store.SyncResult{}, nil
	})
	m.Init()
	updated, _ := m.Update(syncMsg{Project: store.Project{ID: 999}, Err: errors.New("boom")})
	m = updated.(model)
	if strings.Contains(m.View(), "sync failed") {
		t.Fatal("a stale result from another Project surfaced a notice")
	}
	if !m.syncing {
		t.Fatal("a stale result ended the current Project's sync")
	}
}

func TestSwitchingProjectDisownsTheRunningSyncAndStartsANewOne(t *testing.T) {
	m, st, project := newSyncTestModel(t, func() (store.SyncResult, error) {
		return store.SyncResult{}, nil
	})
	other, err := st.CreateProject("other-app", "OTH", t.TempDir())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	m.Init()
	m.openProjectPicker()
	for i, p := range m.projects {
		if p.ID == other.ID {
			m.projectCursor = i
		}
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.project.ID != other.ID {
		t.Fatalf("switched to %d, want %d", m.project.ID, other.ID)
	}
	if cmd == nil || !m.syncing {
		t.Fatal("the switch did not start the new Project's sync")
	}
	updated, _ = m.Update(syncMsg{Project: project, Err: errors.New("late")})
	m = updated.(model)
	if !m.syncing || m.note != "" {
		t.Fatalf("the old Project's late result was applied: syncing=%v note=%q", m.syncing, m.note)
	}
}

func TestFailureNoticeShowsInIssueDetailAndFitsTheFrame(t *testing.T) {
	m, _, project := newSyncTestModel(t, nil)
	m.width, m.height = 60, 30
	issue, _ := m.issueInSections("SYN-1")
	m.showIssue(issue)
	updated, _ := m.Update(syncMsg{Project: project, Err: errors.New(strings.Repeat("x", 200))})
	m = updated.(model)
	view := m.View()
	if !strings.Contains(view, "sync failed: xxx") {
		t.Fatalf("detail view lacks the notice:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if runeLen(line) > 60 {
			t.Fatalf("notice line overflows the frame: %q", line)
		}
	}
}
