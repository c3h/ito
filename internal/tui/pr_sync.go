package tui

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/c3h/ito/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

type pullRequest struct {
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
}

type issueStatusMove struct {
	ID     string
	Status string
}

// prSyncCandidate is the slice of an Issue the sync reads. Carrying it instead
// of the store.Issue keeps the whole Digest — bodies, labels and links included
// — from staying alive in the command closure for the duration of the gh call.
type prSyncCandidate struct {
	ID     string
	Branch string
	Status string
}

type prSyncMsg struct {
	Project store.Project
	Moves   []issueStatusMove
}

// prSyncCandidates keeps the Issues a sync could act on: the registered branch
// is the signal that work exists to sync, and done stays final — never demoted.
func prSyncCandidates(issues []store.Issue) []prSyncCandidate {
	candidates := make([]prSyncCandidate, 0, len(issues))
	for _, issue := range issues {
		if issue.Branch == "" || issue.Status == "done" {
			continue
		}
		candidates = append(candidates, prSyncCandidate{ID: issue.ID, Branch: issue.Branch, Status: issue.Status})
	}
	return candidates
}

func mapPRStatusMoves(issues []prSyncCandidate, prs []pullRequest) []issueStatusMove {
	// gh lists PRs newest-first, so first-wins keeps the latest PR when a
	// branch was reused after an earlier one was closed.
	states := make(map[string]string, len(prs))
	for _, pr := range prs {
		if _, exists := states[pr.HeadRefName]; !exists {
			states[pr.HeadRefName] = pr.State
		}
	}

	moves := make([]issueStatusMove, 0)
	for _, issue := range issues {
		switch states[issue.Branch] {
		case "MERGED", "CLOSED":
			moves = append(moves, issueStatusMove{ID: issue.ID, Status: "done"})
		case "OPEN":
			if issue.Status != "in_review" {
				moves = append(moves, issueStatusMove{ID: issue.ID, Status: "in_review"})
			}
		}
	}
	return moves
}

// syncPRsCmd asks gh for the Project's pull requests, or nothing at all when no
// Issue has a branch registered — the common case on a Project the agents have
// not started, and one that would otherwise pay a subprocess on every refresh.
func syncPRsCmd(project store.Project, issues []store.Issue) tea.Cmd {
	candidates := prSyncCandidates(issues)
	if len(candidates) == 0 {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The sync is best-effort: gh missing, unauthenticated, offline, or a
		// root outside a GitHub repo all end here, and the local refresh that
		// already ran must stay untouched by any of it.
		cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "all", "--json", "headRefName,state", "--limit", "200")
		// The PRs belong to the open Project's repo, not to wherever the TUI
		// happened to be launched from. A Project with no root registered falls
		// back to the process directory.
		if project.RootPath != nil {
			cmd.Dir = *project.RootPath
		}
		output, err := cmd.Output()
		if err != nil {
			return nil
		}
		var prs []pullRequest
		if err := json.Unmarshal(output, &prs); err != nil {
			return nil
		}
		return prSyncMsg{Project: project, Moves: mapPRStatusMoves(candidates, prs)}
	}
}
