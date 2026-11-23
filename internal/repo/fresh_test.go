package repo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grow adds about 750 KB of incompressible files to the source's default branch.
func grow(s *source) {
	files := map[string]string{}
	for i := 0; i < 3; i++ {
		b := make([]byte, 190_000)
		rand.Read(b)
		files["noise"+strconv.Itoa(i)+".txt"] = base64.StdEncoding.EncodeToString(b)
	}
	s.commit("grow", files)
	s.push("main")
}

// age makes the mirror's last fresh clone older.
func age(t *testing.T, m *Mirrors, by time.Duration) {
	t.Helper()
	mark := filepath.Join(m.path(1), cloneMark)
	when := time.Now().Add(-by)
	if err := os.Chtimes(mark, when, when); err != nil {
		t.Fatal(err)
	}
}

// clones counts the clones git was asked for so far.
func clones(argsLog string) int {
	b, _ := os.ReadFile(argsLog)
	return strings.Count(string(b), " clone ") + strings.Count(string(b), "\nclone ")
}

// A mirror is cloned afresh once it has grown past twice its size at its last fresh clone, or is
// 14 days old: the closed pull requests' objects are gone then, the open ones' are not, and the
// mirror serves throughout and afterwards.
func TestFreshCloneWhenTheMirrorHasGrownOrAged(t *testing.T) {
	s := newSource(t)
	first, _ := s.seed()
	m := newMirrors(t, s)
	gitPath, argsLog, _ := wrapper(t, s.realGit)
	m.Git = gitPath
	ctx := context.Background()

	syncPulls(t, m, 1, 2)
	was := recorded(t, m)
	if was <= 0 || was != dirSize(m.path(1))-int64(len(strconv.FormatInt(was, 10))) {
		t.Errorf("the clone's size was recorded as %d; the mirror is %d bytes", was, dirSize(m.path(1)))
	}
	syncPulls(t, m, 1, 2)
	if n := clones(argsLog); n != 1 {
		t.Fatalf("%d clones after a second sync of an unchanged mirror", n)
	}

	// Pull request 1 closes and the repository grows, but not yet past twice the size: fetched, not cloned.
	s.commit("small", map[string]string{"small.txt": "s\n"})
	s.push("main")
	syncPulls(t, m, 2)
	if n := clones(argsLog); n != 1 || !strings.Contains(objects(t, m), s.prs[0]) {
		t.Errorf("cloned afresh before the mirror had grown (%d clones)", n)
	}

	// Past twice its size.
	grow(s)
	syncPulls(t, m, 2)
	if n := clones(argsLog); n != 2 {
		t.Fatalf("%d clones after the mirror grew past twice its size (size %d, recorded %d)", n, dirSize(m.path(1)), was)
	}
	if all := objects(t, m); strings.Contains(all, s.prs[0]) || !strings.Contains(all, s.prs[1]) {
		t.Errorf("after the fresh clone: closed pull request's commit present = %v, open one's present = %v",
			strings.Contains(all, s.prs[0]), strings.Contains(all, s.prs[1]))
	}
	if got := refsOf(t, m); got != "refs/heads/main refs/pull/2/head" {
		t.Errorf("refs %s", got)
	}
	if now := recorded(t, m); now <= was {
		t.Errorf("the recorded size did not follow the new clone: %d, was %d", now, was)
	}
	for _, leftover := range []string{".new", ".old"} {
		if _, err := os.Stat(m.path(1) + leftover); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}
	r := m.Repo(1, "")
	if _, err := r.Resolve(ctx, first); err != nil {
		t.Errorf("an old commit of the default branch after the swap: %v", err)
	}
	if got, err := r.Resolve(ctx, "pr/2"); err != nil || got != s.prs[1] {
		t.Errorf("pr/2 after the swap: %q, %v", got, err)
	}
	if _, err := r.Resolve(ctx, "pr/1"); err == nil {
		t.Error("the closed pull request still resolves")
	}
	if text, err := r.ReadFile(ctx, s.git(s.work, "rev-parse", "HEAD"), "noise0.txt"); err != nil || len(text) < 100_000 {
		t.Errorf("reading after the swap: %v", err)
	}
	syncPulls(t, m, 2)
	if n := clones(argsLog); n != 2 {
		t.Errorf("%d clones: cloned again right after a fresh clone", n)
	}

	// Age: 13 days is not enough, 15 days is.
	age(t, m, 13*24*time.Hour)
	syncPulls(t, m, 2)
	if n := clones(argsLog); n != 2 {
		t.Errorf("%d clones at 13 days", n)
	}
	age(t, m, 15*24*time.Hour)
	syncPulls(t, m, 2)
	if n := clones(argsLog); n != 3 {
		t.Errorf("%d clones at 15 days, want 3", n)
	}
	if time.Since(markTime(t, m)) > time.Hour {
		t.Error("the fresh clone did not renew the mark's time")
	}

	// Without knowing which pull requests are open, a fresh clone would lose their heads: it waits.
	age(t, m, 15*24*time.Hour)
	syncOK(t, m, "")
	if n := clones(argsLog); n != 3 {
		t.Errorf("%d clones while the open pull requests were not known", n)
	}
}

func recorded(t *testing.T, m *Mirrors) int64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.path(1), cloneMark))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.ParseInt(string(b), 10, 64)
	return n
}

func markTime(t *testing.T, m *Mirrors) time.Time {
	fi, err := os.Stat(filepath.Join(m.path(1), cloneMark))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// A fresh clone that fails, or fails its checks, leaves the old mirror exactly as it was, whole and
// serving; the failure is shown, and not retried at once. When it can succeed, the swap happens.
func TestFailedFreshCloneKeepsTheOldMirror(t *testing.T) {
	for i, fail := range []string{"clone", "fsck", "HEAD^{commit}"} {
		t.Run("case"+strconv.Itoa(i), func(t *testing.T) {
			s := newSource(t)
			s.seed()
			m := newMirrors(t, s)
			gitPath, argsLog, _ := wrapper(t, s.realGit)
			m.Git = gitPath
			ctx := context.Background()
			syncPulls(t, m, 1, 2)
			before, refs := objects(t, m), refsOf(t, m)
			head, _ := m.Repo(1, "").Resolve(ctx, "")
			age(t, m, 15*24*time.Hour)
			failFile := filepath.Join(filepath.Dir(argsLog), "fail")
			os.WriteFile(failFile, []byte(fail), 0o600)

			syncPulls(t, m, 1, 2)
			if n := clones(argsLog); n != 2 { // the first clone, and the attempt
				t.Fatalf("%d clones; the failing one should have been tried once", n)
			}
			st := m.Status(1)
			if !st.Ready || !strings.Contains(st.Err, "fresh clone") {
				t.Errorf("status %+v", st)
			}
			if objects(t, m) != before || refsOf(t, m) != refs {
				t.Error("the old mirror changed")
			}
			if got, err := m.Repo(1, "").Resolve(ctx, ""); err != nil || got != head {
				t.Errorf("the old mirror does not serve: %q, %v", got, err)
			}
			if _, err := os.Stat(m.path(1) + ".new"); err == nil {
				t.Error("the failed clone was left behind")
			}
			if _, err := os.Stat(m.path(1) + ".old"); err == nil {
				t.Error("an .old folder was left behind")
			}
			syncPulls(t, m, 1, 2) // no new attempt for a while
			if n := clones(argsLog); n != 2 {
				t.Errorf("%d clones: the failed one was retried at once", n)
			}

			os.Remove(failFile) // whatever failed is fine now, and the wait is over
			m.state(1).freshAfter = time.Time{}
			syncPulls(t, m, 1, 2)
			if n := clones(argsLog); n != 3 || m.Status(1).Err != "" || time.Since(markTime(t, m)) > time.Hour {
				t.Errorf("after recovering: %d clones, status %+v", n, m.Status(1))
			}
		})
	}
}

// If the swap itself cannot be made, the old mirror is put back.
func TestSwapPutsTheOldMirrorBack(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	syncPulls(t, m, 1, 2)
	before := objects(t, m)
	st := m.state(1)
	// A new mirror that is not there: renaming it into place fails.
	if err := m.swap(1, filepath.Join(m.dir, "missing.new"), st); err == nil {
		t.Fatal("swapped in a mirror that does not exist")
	}
	if !m.Ready(1) || objects(t, m) != before {
		t.Error("the old mirror was not put back whole")
	}
	if _, err := os.Stat(m.path(1) + ".old"); err == nil {
		t.Error("an .old folder was left behind")
	}
}
