package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gimigliano/ito/internal/store"
)

func TestDigestRendersEmptyFrame(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	view := newModel(store.New(db)).View()
	for _, want := range []string{"ito Digest", "No issues loaded yet."} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected Digest frame to contain %q, got %q", want, view)
		}
	}
}

func TestDigestQuitsOnQAndCtrlC(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			_, cmd := newModel(store.New(db)).Update(keyMsg(t, key))
			if cmd == nil {
				t.Fatalf("expected %s to return a quit command", key)
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("expected %s to return tea.QuitMsg, got %T", key, cmd())
			}
		})
	}
}

func keyMsg(t *testing.T, key string) tea.KeyMsg {
	t.Helper()
	switch key {
	case "q":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		t.Fatalf("unsupported key %q", key)
		return tea.KeyMsg{Type: tea.KeyNull}
	}
}
