package repo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// at makes the next commits carry a date.
func at(t *testing.T, day string) {
	t.Setenv("GIT_COMMITTER_DATE", day+"T12:00:00Z")
	t.Setenv("GIT_AUTHOR_DATE", day+"T12:00:00Z")
}

// The history reads: the commits that touched a path, only recent ones when asked, what landed on
// the default branch since a pull request branched, who works where, and which paths exist.
func TestHistoryReads(t *testing.T) {
	s := newSource(t)
	at(t, "2026-01-05")
	base := s.commit("first", map[string]string{"core/a.go": "a\n", "docs/x.md": "x\n"})
	s.push("main")
	at(t, "2026-10-01")
	s.git(s.work, "checkout", "-q", "-b", "feature")
	head := s.commit("feature work", map[string]string{"core/a.go": "a2\n", "new/n.go": "n\n"})
	s.push("HEAD:refs/pull/1/head")
	s.git(s.work, "checkout", "-q", "main")
	at(t, "2026-10-20")
	s.commit("tune core (#12)", map[string]string{"core/a.go": "a3\n"})
	at(t, "2026-11-10")
	tip := s.commit("write docs (#13)", map[string]string{"docs/y.md": "y\n"})
	s.push("main")
	m := newMirrors(t, s)
	syncPulls(t, m, 1)
	r := m.Repo(1, "")
	ctx := context.Background()

	// Recent commits of a folder: the old one is cut by the date, the order is newest first.
	got, err := r.PathLog(ctx, tip, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), []string{"core"}, 8)
	if err != nil || len(got) != 1 || got[0].Subject != "tune core (#12)" || got[0].Date != "2026-10-20" || got[0].Author != "T" || len(got[0].SHA) < 7 {
		t.Fatalf("recent core commits: %+v %v", got, err)
	}
	if all, _ := r.PathLog(ctx, tip, time.Time{}, []string{"core", "docs"}, 8); len(all) != 3 || all[0].Subject != "write docs (#13)" {
		t.Fatalf("every commit of two folders: %+v", all)
	}
	if one, _ := r.PathLog(ctx, tip, time.Time{}, []string{"core", "docs"}, 2); len(one) != 2 {
		t.Fatalf("the limit: %+v", one)
	}

	// What landed on the default branch after the pull request branched, on the files it changes.
	mb, err := r.MergeBase(ctx, tip, head)
	if err != nil || mb != base {
		t.Fatalf("merge-base = %q, %v; want %s", mb, err, base)
	}
	landed, err := r.RangeLog(ctx, mb, tip, []string{"core/a.go", "new/n.go"}, 20)
	if err != nil || len(landed) != 1 || landed[0].Subject != "tune core (#12)" {
		t.Fatalf("landed: %+v %v", landed, err)
	}

	// Authors by commit count in a folder.
	as, err := r.Authors(ctx, tip, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "core", 3)
	if err != nil || len(as) != 1 || as[0].Name != "T" || as[0].Commits != 2 {
		t.Fatalf("authors: %+v %v", as, err)
	}

	if !r.HasPath(ctx, tip, "core/a.go") || !r.HasPath(ctx, tip, "docs/") || r.HasPath(ctx, tip, "new/n.go") || r.HasPath(ctx, tip, "../x") {
		t.Fatal("HasPath")
	}

	// Paths are plain paths: no glob, no option, no git magic.
	for _, bad := range []string{"a/../b", "/abs", ""} {
		if _, err := r.PathLog(ctx, tip, time.Time{}, []string{bad}, 5); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	for _, odd := range []string{"core/*", ":(top)core", "-p"} { // a name, not a glob, git magic or an option
		if got, err := r.PathLog(ctx, tip, time.Time{}, []string{odd}, 5); err != nil || len(got) != 0 {
			t.Fatalf("%q matched: %+v %v", odd, got, err)
		}
	}
	if _, err := r.PathLog(ctx, "--all", time.Time{}, []string{"core"}, 5); err == nil || !strings.Contains(err.Error(), "commit id") {
		t.Fatalf("an option as a commit: %v", err)
	}
}
