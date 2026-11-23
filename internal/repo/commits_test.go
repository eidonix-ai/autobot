package repo

import (
	"context"
	"strings"
	"testing"
)

// Commits lists what the new head has that the old one did not, and fails plainly when the old one
// is not in the copy (a rewritten branch).
func TestCommitsSince(t *testing.T) {
	s := newSource(t)
	first, second := s.seed()
	m := newMirrors(t, s)
	syncPulls(t, m)
	r := m.Repo(1, "")
	ctx := context.Background()
	out, err := r.Commits(ctx, first, second)
	if err != nil || !strings.Contains(out, "add util: fix the Hello bug") || strings.Contains(out, "first commit") || strings.Count(out, "\n") != 1 {
		t.Fatalf("%q, %v", out, err)
	}
	if out, err := r.Commits(ctx, second, second); err != nil || out != "No new commits." {
		t.Fatalf("%q, %v", out, err)
	}
	if _, err := r.Commits(ctx, "0123456789abcdef0123456789abcdef01234567", second); err == nil || !strings.Contains(err.Error(), "not in the local copy") {
		t.Fatalf("a commit the copy lacks: %v", err)
	}
	if _, err := r.Commits(ctx, "--all", second); err == nil {
		t.Fatal("an option as a commit")
	}
}
