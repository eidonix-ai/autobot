package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeCode is a repository's local copy with two commits, "default" (the default branch's head)
// and "head" (a pull request's head), that records what it was asked.
type fakeCode struct {
	asked []string // "<what> <commit>"
}

const (
	defaultSHA = "d000000000000000000000000000000000000000"
	headSHA    = "e000000000000000000000000000000000000000"
	baseSHA    = "b000000000000000000000000000000000000000"
)

func (f *fakeCode) Resolve(_ context.Context, ref string) (string, error) {
	switch ref {
	case "", "main":
		return defaultSHA, nil
	case "pr/7":
		return headSHA, nil
	}
	return "", errors.New("not a ref")
}
func (f *fakeCode) Branch(_ context.Context, name string) (string, error) {
	if name == "release" {
		return baseSHA, nil
	}
	return "", errors.New("no such branch")
}
func (f *fakeCode) ListFiles(_ context.Context, c string) ([]string, error) {
	f.asked = append(f.asked, "list "+c)
	return []string{"a_test.go", "internal/x/a.go", "README.md"}, nil
}
func (f *fakeCode) ReadFile(_ context.Context, c, p string) (string, error) {
	f.asked = append(f.asked, "read "+c+" "+p)
	return "one\ntwo\nthree", nil
}
func (f *fakeCode) Grep(_ context.Context, c, pat, spec string, ic bool) (string, error) {
	f.asked = append(f.asked, "grep "+c+" "+pat+" "+spec)
	return "a.go:1:" + pat + "\n", nil
}
func (f *fakeCode) Log(_ context.Context, c, text, p string) (string, error) {
	f.asked = append(f.asked, "log "+c+" "+text+" "+p)
	return "abc1234 2026-01-01 T: subject\n", nil
}
func (f *fakeCode) Diff(_ context.Context, base, head string) (string, error) {
	f.asked = append(f.asked, "diff "+base+" "+head)
	return "diff --git a/x/one.go b/x/one.go\n+one\ndiff --git a/y/two.go b/y/two.go\n+two\n", nil
}
func (f *fakeCode) Overview(_ context.Context, c string) (string, error) {
	f.asked = append(f.asked, "overview "+c)
	return "3 files.", nil
}

func find(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Spec.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return Tool{}
}

func useTool(t *testing.T, tl Tool, args string) string {
	t.Helper()
	out, err := tl.Run(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s %s: %v", tl.Spec.Name, args, err)
	}
	return out
}

func names(tools []Tool) []string {
	var out []string
	for _, tl := range tools {
		out = append(out, tl.Spec.Name)
	}
	slices.Sort(out)
	return out
}

func TestCodeToolsSet(t *testing.T) {
	got := strings.Join(names(CodeTools(&fakeCode{}, nil)), " ")
	if got != "list_files read_file repo_overview search_code search_history" {
		t.Errorf("without a pull request: %s", got)
	}
	got = strings.Join(names(CodeTools(&fakeCode{}, &LocalPR{Number: 7, Head: headSHA, Base: "release"})), " ")
	if got != "list_files pr_diff read_file repo_overview search_code search_history" {
		t.Errorf("with a pull request: %s", got)
	}
}

// With a pull request the tools read at its head; without one, at the default branch; and a ref
// argument reads elsewhere (and a bad one is refused by the repository).
func TestCodeToolsReadAtTheRightCommit(t *testing.T) {
	f := &fakeCode{}
	pr := CodeTools(f, &LocalPR{Number: 7, Head: headSHA, Base: "release"})
	useTool(t, find(t, pr, "read_file"), `{"path":"a.go"}`)
	useTool(t, find(t, pr, "search_code"), `{"pattern":"x"}`)
	useTool(t, find(t, pr, "read_file"), `{"path":"a.go","ref":"main"}`)
	plain := CodeTools(f, nil)
	useTool(t, find(t, plain, "read_file"), `{"path":"a.go"}`)
	useTool(t, find(t, plain, "search_history"), `{"text":"fix"}`)
	useTool(t, find(t, plain, "repo_overview"), `{}`)
	useTool(t, find(t, plain, "list_files"), `{"pattern":"*_test.go"}`)
	want := []string{"read " + headSHA + " a.go", "grep " + headSHA + " x ", "read " + defaultSHA + " a.go", "read " + defaultSHA + " a.go",
		"log " + defaultSHA + " fix ", "overview " + defaultSHA, "list " + defaultSHA}
	if !slices.Equal(f.asked, want) {
		t.Errorf("asked %q\nwant  %q", f.asked, want)
	}
	if _, err := find(t, plain, "read_file").Run(context.Background(), json.RawMessage(`{"path":"a.go","ref":"--upload-pack=x"}`)); err == nil {
		t.Error("a bad ref was read")
	}
	if out := useTool(t, find(t, plain, "list_files"), `{"pattern":"*_test.go"}`); out != "a_test.go" {
		t.Errorf("list_files pattern: %q", out)
	}
	if out := useTool(t, find(t, plain, "read_file"), `{"path":"a.go","from_line":2,"to_line":2}`); !strings.Contains(out, "     2  two") || strings.Contains(out, "one") {
		t.Errorf("read_file lines: %q", out)
	}
}

// Reading a file with read_file, or searching just one file with search_code, counts as having
// read it, so a report citing it is not flagged; a search across files does not.
func TestCodeToolsReadsHooks(t *testing.T) {
	pr := CodeTools(&fakeCode{}, &LocalPR{Number: 7, Head: headSHA, Base: "release", Files: []string{"x/one.go", "y/two.go"}})
	reads := func(name, args string) []string { return find(t, pr, name).Reads(json.RawMessage(args)) }
	for _, c := range []struct {
		tool, args string
		want       []string
	}{
		{"read_file", `{"path":"internal/a.go"}`, []string{"internal/a.go"}},
		{"search_code", `{"pattern":"x","path":"internal/a.go"}`, []string{"internal/a.go"}},
		{"search_code", `{"pattern":"x","path":"internal/*.go"}`, nil},
		{"search_code", `{"pattern":"x","path":"internal/"}`, nil},
		{"search_code", `{"pattern":"x"}`, nil},
		{"pr_diff", `{"path":"one"}`, []string{"x/one.go"}},
		{"pr_diff", `{}`, []string{"x/one.go", "y/two.go"}},
	} {
		if got := reads(c.tool, c.args); !slices.Equal(got, c.want) {
			t.Errorf("%s %s read %v; want %v", c.tool, c.args, got, c.want)
		}
	}
	for _, n := range []string{"list_files", "search_history", "repo_overview"} {
		if find(t, pr, n).Reads != nil {
			t.Errorf("%s names files it read", n)
		}
	}
}

func TestPRDiffIsAgainstTheBaseBranch(t *testing.T) {
	f := &fakeCode{}
	pr := CodeTools(f, &LocalPR{Number: 7, Head: headSHA, Base: "release", Files: []string{"x/one.go", "y/two.go"}})
	d := find(t, pr, "pr_diff")
	if out := useTool(t, d, `{"path":"two"}`); !strings.Contains(out, "+two") || strings.Contains(out, "+one") {
		t.Errorf("filtered diff: %q", out)
	}
	if out := useTool(t, d, `{}`); !strings.Contains(out, "+one") || !strings.Contains(out, "+two") {
		t.Errorf("whole diff: %q", out)
	}
	if want := "diff " + baseSHA + " " + headSHA; f.asked[0] != want {
		t.Errorf("asked %q, want %q", f.asked[0], want)
	}
	if _, err := d.Run(context.Background(), json.RawMessage(`{"path":"nothing"}`)); err == nil {
		t.Error("a diff of no file")
	}
	gone := find(t, CodeTools(f, &LocalPR{Head: headSHA, Base: "deleted-branch"}), "pr_diff")
	if _, err := gone.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("a diff against a base the copy lacks")
	}
}
