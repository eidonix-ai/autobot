package repo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// What the orientation packet reads of the history: who changed what, when. Only trees and commit
// messages are needed, which the partial clone has, so nothing here fetches a file.

// Commit is one commit of a log.
type Commit struct {
	SHA     string // short
	Date    string // yyyy-mm-dd
	Author  string
	Subject string
}

// literal makes git take every path as a plain path, never a glob or ":(magic)".
var literal = []string{"GIT_NO_LAZY_FETCH=1", "GIT_LITERAL_PATHSPECS=1"}

// maxPaths is the most paths one log is asked about; a pull request with more is asked about its
// folders by the caller.
const maxPaths = 400

func checkPaths(paths []string) error {
	if len(paths) == 0 || len(paths) > maxPaths {
		return errors.New("give between 1 and 400 paths")
	}
	for _, p := range paths {
		if err := checkPath(strings.TrimSuffix(p, "/")); err != nil {
			return err
		}
	}
	return nil
}

const commitFormat = "--format=%h%x09%ad%x09%an%x09%s"

func parseCommits(out []byte) []Commit {
	var list []Commit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) == 4 {
			list = append(list, Commit{f[0], f[1], f[2], f[3]})
		}
	}
	return list
}

// PathLog lists the commits up to tip that changed any of the paths (files or folders), newest
// first, at most limit, none older than since (the zero time: any age).
func (r *Repo) PathLog(ctx context.Context, tip string, since time.Time, paths []string, limit int) ([]Commit, error) {
	if err := needSHA(tip); err != nil {
		return nil, err
	}
	if err := checkPaths(paths); err != nil {
		return nil, err
	}
	args := []string{"log", "--max-count=" + strconv.Itoa(limit), "--date=short", commitFormat}
	if !since.IsZero() {
		args = append(args, "--since="+strconv.FormatInt(since.Unix(), 10))
	}
	args = append(args, tip, "--")
	out, _, err := r.gitEnv(ctx, readTimeout, 256<<10, literal, append(args, paths...)...)
	if err != nil {
		return nil, err
	}
	return parseCommits(out), nil
}

// RangeLog lists the commits that to has and from does not, which changed any of the paths, newest
// first, at most limit.
func (r *Repo) RangeLog(ctx context.Context, from, to string, paths []string, limit int) ([]Commit, error) {
	if err := needSHA(from); err != nil {
		return nil, err
	}
	if err := needSHA(to); err != nil {
		return nil, err
	}
	if err := checkPaths(paths); err != nil {
		return nil, err
	}
	args := []string{"log", "--max-count=" + strconv.Itoa(limit), "--date=short", commitFormat, from + ".." + to, "--"}
	out, _, err := r.gitEnv(ctx, readTimeout, 256<<10, literal, append(args, paths...)...)
	if err != nil {
		return nil, err
	}
	return parseCommits(out), nil
}

// MergeBase is the newest commit that both a and b have.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := needSHA(a); err != nil {
		return "", err
	}
	if err := needSHA(b); err != nil {
		return "", err
	}
	out, _, err := r.gitEnv(ctx, readTimeout, 1<<10, noLazy, "merge-base", a, b)
	sha := strings.TrimSpace(string(out))
	if err != nil || !shaRE.MatchString(sha) {
		return "", errors.New("the two commits share no history in the local copy")
	}
	return sha, nil
}

// Authors counts the commits up to tip that changed the path (a file or folder) since the time, by
// author, most first (ties by name), at most n of them.
func (r *Repo) Authors(ctx context.Context, tip string, since time.Time, path string, n int) ([]AuthorCount, error) {
	if err := needSHA(tip); err != nil {
		return nil, err
	}
	if err := checkPaths([]string{path}); err != nil {
		return nil, err
	}
	args := []string{"log", "--max-count=5000", "--format=%an", "--since=" + strconv.FormatInt(since.Unix(), 10), tip, "--", path}
	out, _, err := r.gitEnv(ctx, readTimeout, 1<<20, literal, args...)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			counts[name]++
		}
	}
	var list []AuthorCount
	for _, name := range topN(counts, n) {
		list = append(list, AuthorCount{name, counts[name]})
	}
	return list, nil
}

// AuthorCount is how many commits one author made.
type AuthorCount struct {
	Name    string
	Commits int
}

// HasPath reports whether the commit has a file or folder at the path.
func (r *Repo) HasPath(ctx context.Context, commit, path string) bool {
	if needSHA(commit) != nil || checkPath(strings.TrimSuffix(path, "/")) != nil {
		return false
	}
	_, _, err := r.gitEnv(ctx, readTimeout, 1<<10, literal, "cat-file", "-e", commit+":"+strings.TrimSuffix(path, "/"))
	return err == nil
}

// DefaultTip is the head of the default branch.
func (r *Repo) DefaultTip(ctx context.Context) (string, error) {
	return r.Resolve(ctx, "")
}
