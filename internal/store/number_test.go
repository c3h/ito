package store

import (
	"errors"
	"testing"

	"github.com/c3h/ito/internal/ledger"
)

func TestIssueNumbersComeFromTheLedgerAcrossDevices(t *testing.T) {
	l := ledger.NewMemory()
	a := openSyncDevice(t, "a")
	b := openSyncDevice(t, "b")
	a.st.SetIssueNumberer(l)
	b.st.SetIssueNumberer(l)

	var ids []string
	for i, device := range []syncDevice{a, b, a, b} {
		issue, err := device.st.CreateIssue(device.p, NewIssue{Title: "Numbered", Status: "todo", Priority: "medium"})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, issue.ID)
	}
	want := []string{"SHR-1", "SHR-2", "SHR-3", "SHR-4"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}

	// Both Devices hold all four after a sync, and the next number is 5 even
	// though each Device only created two locally.
	syncDevices(t, l, a, b, a)
	for name, device := range map[string]syncDevice{"A": a, "B": b} {
		issues, err := device.st.ListIssues(ListOptions{ProjectID: device.p.ID, IncludeDone: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 4 {
			t.Fatalf("%s holds %d issues, want 4", name, len(issues))
		}
	}
	issue, err := b.st.CreateIssue(b.p, NewIssue{Title: "Fifth", Status: "todo", Priority: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.ID != "SHR-5" {
		t.Fatalf("next id = %s, want SHR-5", issue.ID)
	}
}

func TestIssueNumbersStayLocalWithoutANumberer(t *testing.T) {
	a := openSyncDevice(t, "a")
	first, err := a.st.CreateIssue(a.p, NewIssue{Title: "One", Status: "todo", Priority: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.st.CreateIssue(a.p, NewIssue{Title: "Two", Status: "todo", Priority: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "SHR-1" || second.ID != "SHR-2" {
		t.Fatalf("ids = %s, %s; want SHR-1, SHR-2", first.ID, second.ID)
	}
}

type failingNumberer struct{ err error }

func (f failingNumberer) ReserveIssueNumber(string, int64) (int64, error) { return 0, f.err }

func TestAFailedReservationWritesNothing(t *testing.T) {
	a := openSyncDevice(t, "a")
	cause := errors.New("connection refused")
	a.st.SetIssueNumberer(failingNumberer{cause})

	_, err := a.st.CreateIssue(a.p, NewIssue{Title: "Never", Status: "todo", Priority: "medium", Labels: []string{"bug"}})
	var reservation *ReservationError
	if !errors.As(err, &reservation) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want a ReservationError wrapping the cause", err)
	}
	issues, err := a.st.ListIssues(ListOptions{ProjectID: a.p.ID, IncludeDone: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("store holds %d issues after a failed reservation, want none", len(issues))
	}
	pushed, err := a.st.Push(ledger.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	// Only the Project's own Change is pending; the Issue left none.
	if pushed != 1 {
		t.Fatalf("pushed %d Changes, want only the Project's", pushed)
	}
}

type countingNumberer struct{ calls int64 }

func (c *countingNumberer) ReserveIssueNumber(_ string, floor int64) (int64, error) {
	c.calls++
	return floor + 1, nil
}

func TestARefusedCreationReservesNoNumber(t *testing.T) {
	a := openSyncDevice(t, "a")
	numberer := &countingNumberer{}
	a.st.SetIssueNumberer(numberer)

	if _, err := a.st.CreateIssue(a.p, NewIssue{Title: "Homeless", Status: "todo", Priority: "medium", Batch: "missing"}); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("err = %v, want ErrBatchNotFound", err)
	}
	if numberer.calls != 0 {
		t.Fatalf("a refused creation reserved %d numbers, want none", numberer.calls)
	}
}
