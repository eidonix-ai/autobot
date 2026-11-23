package web

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
)

// The local copy keeps the heads of the open pull requests: the sync is told which are open, across
// pages, and told it does not know when GitHub cannot be asked.
func TestOpenPullsForTheLocalCopy(t *testing.T) {
	gh := &pollFake{lines: map[int]int{}, roles: map[string]string{}, ci: map[string]string{}}
	now := time.Now()
	total := github.PageSize + 5 // more than one page
	for i := 1; i <= total; i++ {
		gh.prs = append(gh.prs, &github.Listed{Number: i, Kind: "pr", Open: true, Updated: now.Add(-time.Duration(i) * time.Minute)})
	}
	gh.prs = append(gh.prs, &github.Listed{Number: 900, Kind: "pr", Open: false, Updated: now})
	e := newEnvWith(t, gh)
	ws, _ := e.st.CreateWorkspace("o/r", e.user("derek", true), "", "")
	got := e.ui.openPulls(context.Background(), ws, "")
	if !got.Known || len(got.Open) != total || slices.Contains(got.Open, 900) {
		t.Errorf("open pull requests: known %v, %d of %d, closed one listed %v", got.Known, len(got.Open), total, slices.Contains(got.Open, 900))
	}
	gh.err = errors.New("rate limited")
	if got := e.ui.openPulls(context.Background(), ws, ""); got.Known || len(got.Open) != 0 {
		t.Errorf("when GitHub cannot be asked: %+v", got)
	}
}
