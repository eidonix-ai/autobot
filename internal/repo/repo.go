// Package repo keeps a local, current copy of each workspace's git repository and reads it, so
// agents can read, search and see the history of the code without a machine and without GitHub
// API calls.
//
// Each workspace has one bare mirror under <data dir>/repos/<workspace id>.git. It is cloned once
// (files over 1 MB are left behind and fetched only if someone reads them) and then only fetched:
// git sends just the objects the mirror lacks, so nothing is downloaded twice. It is lean, not an
// archive: it holds the default branch and the heads of the open pull requests
// (refs/pull/N/head), and a pull request that closes loses its ref.
//
// The system git runs with an argument list, never a shell, and every call has a time limit and a
// cap on its output. The workspace's GitHub token reaches git only through its environment, never
// on a command line and never in the repository's own config files.
package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// How often a healthy mirror is fetched, and the waits after failures (doubling, up to the longest).
const (
	Every        = 10 * time.Minute
	firstBackoff = 2 * time.Minute
	longestWait  = 60 * time.Minute

	cloneTimeout = 45 * time.Minute
	fetchTimeout = 15 * time.Minute
	readTimeout  = 30 * time.Second
	grepTimeout  = 20 * time.Second
)

// Mirrors are the local copies of all workspaces' repositories.
type Mirrors struct {
	// BaseURL is where repositories are cloned from: BaseURL/owner/repo.git. Tests use file:// URLs.
	BaseURL string
	// Git is the git program to run.
	Git string
	// TidyEvery is how often a mirror is rebuilt without its dead objects; zero means TidyEvery's constant.
	TidyEvery time.Duration

	dir    string // <data dir>/repos
	mu     sync.Mutex
	states map[int64]*state
	bg     sync.WaitGroup // cleanups running in the background
}

// state is what is remembered, in memory, about one workspace's mirror.
type state struct {
	write sync.Mutex // held by the one git writer of this repository

	mu    sync.Mutex
	last  time.Time // the last successful sync
	err   string    // why the last sync failed, or ""
	fails int       // failures in a row
	next  time.Time // not before this time
	busy  int       // syncs running or waiting

	tidying atomic.Bool // a tidy-up is running or waiting
}

// New makes the mirrors under dataDir (the folder of the SQLite file).
func New(dataDir string) *Mirrors {
	return &Mirrors{BaseURL: "https://github.com", Git: "git", dir: filepath.Join(dataDir, "repos"), states: map[int64]*state{}}
}

func (m *Mirrors) state(id int64) *state {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[id] == nil {
		m.states[id] = &state{}
	}
	return m.states[id]
}

func (m *Mirrors) path(id int64) string {
	return filepath.Join(m.dir, strconv.FormatInt(id, 10)+".git")
}

// Ready reports whether the workspace has a complete mirror: a clone that finished.
func (m *Mirrors) Ready(id int64) bool {
	_, err := os.Stat(filepath.Join(m.path(id), "HEAD"))
	return err == nil
}

// Status is what the settings page shows about a mirror.
type Status struct {
	Ready bool
	Last  time.Time // the last fetch; zero if never
	Err   string    // why the last sync failed, or ""
	Size  int64     // bytes on disk
}

// Status reports on a workspace's mirror.
func (m *Mirrors) Status(id int64) Status {
	s := m.state(id)
	s.mu.Lock()
	out := Status{Ready: m.Ready(id), Last: s.last, Err: s.err}
	s.mu.Unlock()
	if out.Last.IsZero() { // after a restart: when git last wrote FETCH_HEAD
		if fi, err := os.Stat(filepath.Join(m.path(id), "FETCH_HEAD")); err == nil {
			out.Last = fi.ModTime()
		}
	}
	filepath.WalkDir(m.path(id), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				out.Size += fi.Size()
			}
		}
		return nil
	})
	return out
}

// Due reports whether a workspace's mirror should be synced now: every Every, and after a failure
// only once its wait is over; never while a sync of it is running.
func (m *Mirrors) Due(id int64, now time.Time) bool {
	s := m.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy == 0 && !now.Before(s.next)
}

// Prune removes the mirrors of workspaces that no longer exist: every <number>.git folder (and
// <number>.git.new, a clone left unfinished) whose number is not in keep. Nothing else in the folder is touched.
func (m *Mirrors) Prune(keep []int64) {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		n, ok := strings.CutSuffix(strings.TrimSuffix(e.Name(), ".new"), ".git") // also a clone left unfinished
		id, err := strconv.ParseInt(n, 10, 64)
		if !ok || err != nil || !e.IsDir() || strconv.FormatInt(id, 10) != n {
			continue
		}
		found := false
		for _, k := range keep {
			found = found || k == id
		}
		if !found {
			os.RemoveAll(filepath.Join(m.dir, e.Name()))
			m.mu.Lock()
			delete(m.states, id)
			m.mu.Unlock()
		}
	}
}

// Pulls says which pull requests are open. A mirror holds the heads of open pull requests only.
// When Known is false (GitHub could not be asked this time) the mirror keeps the pull requests it
// has and fetches no more of them.
type Pulls struct {
	Open  []int
	Known bool
}

// Sync clones the workspace's repository if it has no mirror yet, and otherwise fetches what is
// new: the default branch and the open pull requests' heads. A pull request that is no longer
// open loses its ref, and once a day the mirror is rebuilt without the objects only such pull
// requests used. Only
// one sync of a repository runs at a time. Its outcome is remembered for Status and Due.
func (m *Mirrors) Sync(ctx context.Context, id int64, name, token string, pulls Pulls) error {
	s := m.state(id)
	s.mu.Lock()
	s.busy++
	s.mu.Unlock()
	s.write.Lock()
	defer s.write.Unlock()
	err := m.sync(ctx, id, name, token, pulls)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy--
	now := time.Now()
	if err == nil {
		s.last, s.err, s.fails, s.next = now, "", 0, now.Add(Every)
		m.collect(id, s)
		return nil
	}
	s.err = err.Error()
	s.fails++
	wait := firstBackoff
	for i := 1; i < s.fails && wait < longestWait; i++ {
		wait *= 2
	}
	s.next = now.Add(min(wait, longestWait))
	return err
}

// Ensure makes sure the mirror has a commit, fetching if it does not: just the head of pull
// request pr when there is one (and it is the commit's), otherwise the default branch. It fails
// if the commit is still missing afterwards. It does not change the mirror's schedule.
func (m *Mirrors) Ensure(ctx context.Context, id int64, name, token, commit string, pr int) error {
	r := m.Repo(id, token)
	if r.has(ctx, commit) {
		return nil
	}
	if pr <= 0 {
		if err := m.Sync(ctx, id, name, token, Pulls{}); err != nil {
			return err
		}
	} else {
		s := m.state(id)
		s.write.Lock()
		spec := "+refs/pull/" + strconv.Itoa(pr) + "/head:refs/pull/" + strconv.Itoa(pr) + "/head"
		_, _, err := m.git(ctx, fetchTimeout, 1<<16, token, "--git-dir="+m.path(id), "fetch", "--no-tags", "--quiet", "origin", spec)
		s.write.Unlock()
		if err != nil {
			return err
		}
	}
	if !r.has(ctx, commit) {
		return fmt.Errorf("commit %s is not in the local copy of %s", commit, name)
	}
	return nil
}

// MaxOpenPulls is the most open pull requests a mirror holds the heads of (the most recently
// updated ones, if there are more).
const MaxOpenPulls = 1000

func (m *Mirrors) sync(ctx context.Context, id int64, name, token string, pulls Pulls) error {
	url := strings.TrimSuffix(m.BaseURL, "/") + "/" + name + ".git"
	dir := m.path(id)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	if m.Ready(id) {
		return m.update(ctx, dir, url, token, pulls)
	}
	// Clone beside the final place and move it in when whole, so a half-made clone is never a mirror.
	// Only the default branch comes with the clone; large files stay behind until read.
	tmp := dir + ".new"
	os.RemoveAll(tmp)
	if _, _, err := m.git(ctx, cloneTimeout, 1<<16, token, "clone", "--bare", "--single-branch", "--no-tags", "--filter=blob:limit=1m", "--quiet", url, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := m.update(ctx, tmp, url, token, pulls); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, tidyMark), nil, 0o600) // nothing to tidy yet
}

// update brings a repository up to date: it points it at its address (a renamed workspace has a
// new one), fetches the default branch and the open pull requests, and drops the refs of
// everything else.
func (m *Mirrors) update(ctx context.Context, dir, url, token string, pulls Pulls) error {
	g := "--git-dir=" + dir
	if _, _, err := m.git(ctx, readTimeout, 1<<16, token, g, "config", "remote.origin.url", url); err != nil {
		return err
	}
	// The default branch is whatever GitHub's HEAD points to now; when asking fails, the one we had.
	def := ""
	if out, _, err := m.git(ctx, readTimeout, 1<<16, token, g, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		if rest, ok := strings.CutPrefix(string(out), "ref: refs/heads/"); ok {
			def, _, _ = strings.Cut(rest, "\t")
		}
	}
	if def == "" {
		if out, _, err := m.git(ctx, readTimeout, 1<<10, token, g, "symbolic-ref", "-q", "HEAD"); err == nil {
			def = strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/heads/")
		}
	}
	if def == "" || strings.HasPrefix(def, "-") || strings.ContainsAny(def, " ~^:?*[\\\x00\n") {
		return fmt.Errorf("could not tell the repository's default branch")
	}
	args := []string{g, "fetch", "--no-tags", "--quiet", "origin", "+refs/heads/" + def + ":refs/heads/" + def}
	open := map[int]bool{}
	if pulls.Known {
		for _, n := range pulls.Open[:min(len(pulls.Open), MaxOpenPulls)] {
			if n > 0 && !open[n] {
				open[n] = true
				args = append(args, "+refs/pull/"+strconv.Itoa(n)+"/head:refs/pull/"+strconv.Itoa(n)+"/head")
			}
		}
	}
	if _, _, err := m.git(ctx, fetchTimeout, 1<<16, token, args...); err != nil {
		return err
	}
	if _, _, err := m.git(ctx, readTimeout, 1<<16, token, g, "symbolic-ref", "HEAD", "refs/heads/"+def); err != nil {
		return err
	}
	// Drop the refs that are no longer wanted: other branches, and pull requests that closed.
	out, _, err := m.git(ctx, readTimeout, 4<<20, token, g, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(string(out)) {
		n, isPull := strings.CutPrefix(ref, "refs/pull/")
		n, isPull = strings.CutSuffix(n, "/head")
		num, err := strconv.Atoi(n)
		drop := (strings.HasPrefix(ref, "refs/heads/") && ref != "refs/heads/"+def) || strings.HasPrefix(ref, "refs/tags/") ||
			(pulls.Known && isPull && !(err == nil && open[num]))
		if drop {
			if _, _, err := m.git(ctx, readTimeout, 1<<10, token, g, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// TidyEvery is how often a mirror is rebuilt to drop the objects nothing refers to any more: those
// only closed pull requests used.
const TidyEvery = 24 * time.Hour

// tidyMark is a file in the mirror whose time says when it was last tidied (or made).
const tidyMark = "autobot-tidy"

// collect starts the tidy-up in the background when a day has passed since the last one. It holds
// the repository's writer lock, so no sync or fetch runs meanwhile. s.mu is held by the caller.
func (m *Mirrors) collect(id int64, s *state) {
	every := m.TidyEvery
	if every == 0 {
		every = TidyEvery
	}
	if fi, err := os.Stat(filepath.Join(m.path(id), tidyMark)); err == nil && time.Since(fi.ModTime()) < every {
		return
	}
	if !s.tidying.CompareAndSwap(false, true) {
		return
	}
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		defer s.tidying.Store(false)
		s.write.Lock()
		defer s.write.Unlock()
		if err := m.tidy(context.Background(), id); err != nil {
			s.mu.Lock()
			s.err = "tidying up: " + err.Error()
			s.mu.Unlock()
		}
	}()
}

// tidy rebuilds the mirror's objects as one pack of just what its refs reach, and deletes what the
// old packs held beyond that. (git gc cannot do this here: in a partial clone it keeps every object
// of the packs the server sent, reachable or not.) Large files git left behind stay behind: nothing
// is fetched. The new pack is whole before any old one is removed.
func (m *Mirrors) tidy(ctx context.Context, id int64) error {
	dir := m.path(id)
	packs := filepath.Join(dir, "objects", "pack")
	old, _ := filepath.Glob(filepath.Join(packs, "*.pack"))
	out, _, err := m.git(ctx, cloneTimeout, 1<<10, "", "-c", "pack.threads=2", "--git-dir="+dir, "pack-objects", "--revs", "--all",
		"--missing=allow-promisor", "--delta-base-offset", "--quiet", filepath.Join(packs, "pack"))
	if err != nil {
		return err
	}
	name := strings.TrimSpace(string(out))
	if !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(name) {
		return fmt.Errorf("git packed objects without saying where: %q", name)
	}
	fresh := filepath.Join(packs, "pack-"+name)
	if err := os.WriteFile(fresh+".promisor", nil, 0o600); err != nil { // the new pack is a partial clone's, like the old ones
		return err
	}
	for _, p := range old {
		base := strings.TrimSuffix(p, ".pack")
		if base == fresh {
			continue
		}
		if _, err := os.Stat(base + ".keep"); err == nil {
			continue
		}
		for _, ext := range []string{".pack", ".idx", ".rev", ".bitmap", ".promisor", ".mtimes"} {
			os.Remove(base + ext)
		}
	}
	// What was loose (small fetches arrive loose) and is now in a pack, or reaches nothing.
	for _, args := range [][]string{{"prune-packed", "--quiet"}, {"prune", "--expire=now"}} {
		if _, _, err := m.git(ctx, fetchTimeout, 1<<16, "", append([]string{"--git-dir=" + dir}, args...)...); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, tidyMark), nil, 0o600)
}

// env is the environment git runs in: nothing inherited, no prompts, no system or user config. The
// token, when there is one, goes in as an extra header for GitHub's address only, through git's
// environment configuration: it is not on any command line and git never writes it to a file.
func (m *Mirrors) env(token string, extra ...string) []string {
	scheme, _, _ := strings.Cut(m.BaseURL, ":")
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + m.dir, "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ALLOW_PROTOCOL=" + scheme}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+strings.TrimSuffix(m.BaseURL, "/")+"/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	return append(env, extra...)
}

// capWriter keeps the first limit bytes written to it and, past that, cancels the command.
type capWriter struct {
	buf       bytes.Buffer
	limit     int
	cancel    context.CancelFunc
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); len(p) > room {
		w.buf.Write(p[:max(room, 0)])
		w.truncated = true
		if w.cancel != nil {
			w.cancel()
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// git runs one git command with a time limit and at most limit bytes of output, and returns the
// output and whether it was cut short. Output cut short is not an error. A failure's message is
// git's own, with the token wiped out of it.
func (m *Mirrors) git(ctx context.Context, timeout time.Duration, limit int, token string, args ...string) ([]byte, bool, error) {
	return m.gitEnv(ctx, timeout, limit, token, nil, args...)
}

func (m *Mirrors) gitEnv(ctx context.Context, timeout time.Duration, limit int, token string, extra []string, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.Git, append([]string{"-c", "gc.autoDetach=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = m.env(token, extra...)
	cmd.WaitDelay = 5 * time.Second
	out := &capWriter{limit: limit, cancel: cancel}
	errs := &capWriter{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = out, errs
	err := cmd.Run()
	if out.truncated {
		return out.buf.Bytes(), true, nil
	}
	if err != nil {
		msg := strings.TrimSpace(errs.buf.String())
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			msg = "timed out after " + timeout.String()
		case msg == "":
			msg = err.Error()
		}
		return out.buf.Bytes(), false, &gitError{msg: msg, err: err}
	}
	return out.buf.Bytes(), false, nil
}

// gitError is a failed git command. Unwrap gives the exit status, which tells grep's "nothing
// found" (1) from a real failure.
type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string { return "git: " + e.msg }
func (e *gitError) Unwrap() error { return e.err }

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
