package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repo reads one workspace's mirror. Readers may run together, and while a sync runs.
type Repo struct {
	m     *Mirrors
	dir   string
	token string // for the rare read that has git fetch a missing large file
}

// Repo is the reader of a workspace's mirror. Its token is the workspace's GitHub token, or "".
func (m *Mirrors) Repo(id int64, token string) *Repo {
	return &Repo{m: m, dir: m.path(id), token: token}
}

func (r *Repo) git(ctx context.Context, timeout time.Duration, limit int, args ...string) ([]byte, bool, error) {
	return r.gitEnv(ctx, timeout, limit, nil, args...)
}

func (r *Repo) gitEnv(ctx context.Context, timeout time.Duration, limit int, extra []string, args ...string) ([]byte, bool, error) {
	return r.m.gitEnv(ctx, timeout, limit, r.token, extra, append([]string{"--git-dir=" + r.dir}, args...)...)
}

// exitCode is the status a failed git command exited with, or -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// noLazy stops git fetching a missing object from GitHub by itself (git 2.44 and later).
var noLazy = []string{"GIT_NO_LAZY_FETCH=1"}

var (
	prRE     = regexp.MustCompile(`^pr/([1-9][0-9]{0,8})$`)
	errNoRef = errors.New(`a ref is a 40-digit commit id, "main" (or the default branch's name), or "pr/<number>"`)
)

// has reports whether the mirror has the commit.
func (r *Repo) has(ctx context.Context, sha string) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	_, _, err := r.gitEnv(ctx, readTimeout, 1<<10, noLazy, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// defaultBranch is the name of the branch GitHub's HEAD points to.
func (r *Repo) defaultBranch(ctx context.Context) (string, error) {
	out, _, err := r.git(ctx, readTimeout, 1<<10, "symbolic-ref", "-q", "HEAD")
	name := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/heads/")
	if err != nil || name == "" {
		return "", errors.New("the local copy has no default branch")
	}
	return name, nil
}

// Resolve turns a ref into a full commit id. It accepts only a 40-digit commit id that the mirror
// has, "main" or the default branch's name (also "HEAD" or nothing: the default branch), and
// "pr/<number>" for a pull request's head. Anything else, and anything that looks like an option,
// is refused.
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "-") {
		return "", errNoRef
	}
	if low := strings.ToLower(ref); shaRE.MatchString(low) {
		if !r.has(ctx, low) {
			return "", fmt.Errorf("commit %s is not in the local copy", low)
		}
		return low, nil
	}
	var full string
	switch m := prRE.FindStringSubmatch(ref); {
	case m != nil:
		full = "refs/pull/" + m[1] + "/head"
	case ref == "" || ref == "HEAD" || ref == "main":
		def, err := r.defaultBranch(ctx)
		if err != nil {
			return "", err
		}
		if ref == "main" {
			def = ref
		}
		full = "refs/heads/" + def
	default:
		if def, err := r.defaultBranch(ctx); err != nil || ref != def {
			return "", errNoRef
		}
		full = "refs/heads/" + ref
	}
	return r.rev(ctx, full, ref)
}

// Branch is the head of a branch by name, for Autobot's own use (a pull request's base branch):
// the name never comes from a model.
func (r *Repo) Branch(ctx context.Context, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "-") || strings.Contains(name, "..") || strings.ContainsAny(name, " ~^:?*[\\\x00") {
		return "", errNoRef
	}
	return r.rev(ctx, "refs/heads/"+name, name)
}

func (r *Repo) rev(ctx context.Context, full, shown string) (string, error) {
	out, _, err := r.gitEnv(ctx, readTimeout, 1<<10, noLazy, "rev-parse", "--verify", "--quiet", full+"^{commit}")
	sha := strings.TrimSpace(string(out))
	if err != nil || !shaRE.MatchString(sha) {
		return "", fmt.Errorf("%q is not in the local copy", shown)
	}
	return sha, nil
}

func needSHA(sha string) error {
	if !shaRE.MatchString(sha) {
		return errors.New("internal: a commit id is needed here")
	}
	return nil
}

// checkPath refuses a path that is not a plain, relative path inside the repository.
func checkPath(p string) error {
	if p == "" || strings.ContainsAny(p, "\x00\n") || strings.HasPrefix(p, "/") {
		return errors.New("path not allowed: give a path relative to the repository's root")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("path not allowed: give a path relative to the repository's root, without . or .. parts")
		}
	}
	return nil
}

// checkSpec refuses a path filter that is anything but a path or a glob inside the repository
// (git's own ":(...)" forms and options are not accepted).
func checkSpec(p string) error {
	if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "-") {
		return errors.New("path filter not allowed: give a path or a glob such as internal/*.go")
	}
	return checkPath(strings.TrimSuffix(p, "/"))
}

// ListFiles lists every file of the commit.
func (r *Repo) ListFiles(ctx context.Context, commit string) ([]string, error) {
	if err := needSHA(commit); err != nil {
		return nil, err
	}
	out, trunc, err := r.git(ctx, readTimeout, 32<<20, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil, err
	}
	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if trunc {
		files = files[:len(files)-1] // the last name may be cut
	}
	sort.Strings(files)
	return files, nil
}

// MaxFile is the most of a file ReadFile returns.
const MaxFile = 4 << 20

// ReadFile reads a file of the commit. A directory, a binary file or a missing path is an error.
func (r *Repo) ReadFile(ctx context.Context, commit, p string) (string, error) {
	if err := needSHA(commit); err != nil {
		return "", err
	}
	if err := checkPath(p); err != nil {
		return "", err
	}
	out, trunc, err := r.git(ctx, readTimeout, MaxFile, "cat-file", "blob", commit+":"+p)
	if err != nil {
		return "", fmt.Errorf("%s is not a file at that commit", p)
	}
	if bytes.IndexByte(out[:min(len(out), 8000)], 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file", p)
	}
	text := string(out)
	if trunc {
		text += fmt.Sprintf("\n[the file is larger than %d MB; the rest is not shown]\n", MaxFile>>20)
	}
	return text, nil
}

// Matches and line caps for Grep and Log.
const (
	MaxGrepLines = 300
	MaxLogLines  = 50
	maxLineChars = 400
)

// Grep searches the commit's files for a regular expression (extended syntax; it is always taken
// as a pattern, never as an option), optionally only those matching a path or glob. It returns
// path:line:text lines, at most MaxGrepLines of them.
func (r *Repo) Grep(ctx context.Context, commit, pattern, pathspec string, ignoreCase bool) (string, error) {
	if err := needSHA(commit); err != nil {
		return "", err
	}
	if pattern == "" || len(pattern) > 500 || strings.ContainsAny(pattern, "\x00\n") {
		return "", errors.New("give one line of up to 500 characters as the pattern")
	}
	args := []string{"grep", "-n", "-I", "-E", "--no-color"}
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern, commit, "--")
	if pathspec != "" {
		if err := checkSpec(pathspec); err != nil {
			return "", err
		}
		args = append(args, pathspec)
	}
	// Large files git left behind are not searched (not fetched just for a search).
	out, trunc, err := r.gitEnv(ctx, grepTimeout, 1<<20, noLazy, args...)
	if err != nil {
		if exitCode(err) == 1 && len(out) == 0 {
			return "No matches.", nil
		}
		if len(out) == 0 {
			return "", err
		}
	}
	var b strings.Builder
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if trunc {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		if i == MaxGrepLines {
			trunc = true
			break
		}
		l = strings.TrimPrefix(l, commit+":")
		if len(l) > maxLineChars {
			l = strings.ToValidUTF8(l[:maxLineChars], "") + "…"
		}
		b.WriteString(l + "\n")
	}
	if trunc {
		fmt.Fprintf(&b, "[more than %d matches; narrow the pattern or the path]\n", MaxGrepLines)
	}
	return b.String(), nil
}

// Log lists the commits up to the commit, newest first (at most MaxLogLines): those whose message
// contains the text (any case), and those that changed the path (a file or a folder).
func (r *Repo) Log(ctx context.Context, commit, text, p string) (string, error) {
	if err := needSHA(commit); err != nil {
		return "", err
	}
	if len(text) > 200 || strings.ContainsAny(text, "\x00\n") {
		return "", errors.New("give one line of up to 200 characters to search for")
	}
	args := []string{"log", "--max-count=" + strconv.Itoa(MaxLogLines), "--date=short", "--format=%h %ad %an: %s"}
	if text != "" {
		args = append(args, "--fixed-strings", "--regexp-ignore-case", "--grep="+text)
	}
	args = append(args, commit, "--")
	if p != "" {
		if err := checkPath(strings.TrimSuffix(p, "/")); err != nil {
			return "", err
		}
		args = append(args, p)
	}
	out, _, err := r.git(ctx, readTimeout, 256<<10, args...)
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "No commits match.", nil
	}
	return string(out), nil
}

// Commits lists the commits that to has and from does not, newest first (at most MaxLogLines), or
// "No new commits." It fails if from is not in the local copy: a branch rewritten since may have
// lost it.
func (r *Repo) Commits(ctx context.Context, from, to string) (string, error) {
	if err := needSHA(from); err != nil {
		return "", err
	}
	if err := needSHA(to); err != nil {
		return "", err
	}
	if !r.has(ctx, from) {
		return "", fmt.Errorf("commit %s is not in the local copy", from)
	}
	out, _, err := r.git(ctx, readTimeout, 256<<10, "log", "--max-count="+strconv.Itoa(MaxLogLines), "--date=short",
		"--format=%h %ad %an: %s", from+".."+to, "--")
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "No new commits.", nil
	}
	return string(out), nil
}

// MaxDiff is the most of a diff Diff returns.
const MaxDiff = 4 << 20

// Diff is what the head commit changes since it left the base's history (git diff base...head).
func (r *Repo) Diff(ctx context.Context, base, head string) (string, error) {
	if err := needSHA(base); err != nil {
		return "", err
	}
	if err := needSHA(head); err != nil {
		return "", err
	}
	out, trunc, err := r.git(ctx, 2*readTimeout, MaxDiff, "diff", "--no-color", "--no-ext-diff", "--no-textconv", base+"..."+head, "--")
	if err != nil {
		return "", err
	}
	text := string(out)
	if trunc {
		text += "\n[the diff is longer than " + strconv.Itoa(MaxDiff>>20) + " MB; the rest is not shown]\n"
	}
	return text, nil
}

var languages = map[string]string{".go": "Go", ".py": "Python", ".js": "JavaScript", ".ts": "TypeScript", ".tsx": "TypeScript",
	".jsx": "JavaScript", ".java": "Java", ".kt": "Kotlin", ".c": "C", ".h": "C/C++ header", ".cc": "C++", ".cpp": "C++",
	".cu": "CUDA", ".rs": "Rust", ".rb": "Ruby", ".php": "PHP", ".cs": "C#", ".swift": "Swift", ".sh": "Shell",
	".md": "Markdown", ".html": "HTML", ".css": "CSS", ".sql": "SQL", ".yaml": "YAML", ".yml": "YAML", ".json": "JSON",
	".toml": "TOML", ".proto": "Protocol Buffers", ".lua": "Lua", ".scala": "Scala", ".m": "Objective-C", ".mm": "Objective-C++"}

// Overview describes the commit's tree: its top-level folders with their file counts, and its main
// file types.
func (r *Repo) Overview(ctx context.Context, commit string) (string, error) {
	files, err := r.ListFiles(ctx, commit)
	if err != nil {
		return "", err
	}
	folders, exts := map[string]int{}, map[string]int{}
	root := 0
	for _, f := range files {
		if top, _, ok := strings.Cut(f, "/"); ok {
			folders[top+"/"]++
		} else {
			root++
		}
		if e := strings.ToLower(path.Ext(f)); e != "" {
			exts[e]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d files.\n\nTop-level folders (files):\n", len(files))
	for _, k := range topN(folders, 40) {
		fmt.Fprintf(&b, "  %s (%d)\n", k, folders[k])
	}
	if root > 0 {
		fmt.Fprintf(&b, "  and %d files in the root\n", root)
	}
	b.WriteString("\nMain file types (files):\n")
	for _, k := range topN(exts, 12) {
		name := k
		if l := languages[k]; l != "" {
			name = k + " " + l
		}
		fmt.Fprintf(&b, "  %s (%d)\n", name, exts[k])
	}
	return b.String(), nil
}

// topN is the keys with the biggest counts, biggest first (ties by name), at most n.
func topN(counts map[string]int, n int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	return keys[:min(n, len(keys))]
}
