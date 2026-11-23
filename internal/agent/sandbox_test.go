package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSession is a sandbox with a few files. It understands just the commands the file tools send.
type fakeSession struct {
	mu      sync.Mutex
	files   map[string]string
	scripts []string // everything Run was asked, in order
	puts    []string // paths written
	limits  []time.Duration
	reply   func(script string) (CommandResult, bool) // answers a script, when set and it knows it
}

var sedRE = regexp.MustCompile(`^sed -n '(\d+),(\d+)p' -- (.*)$`)

func unq(s string) string {
	return strings.TrimPrefix(strings.ReplaceAll(strings.Trim(s, "'"), `'\''`, "'"), "/work/source/")
}

func (f *fakeSession) Run(ctx context.Context, script string, timeout time.Duration) (CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	f.limits = append(f.limits, timeout)
	if f.reply != nil {
		if r, ok := f.reply(script); ok {
			return r, nil
		}
	}
	missing := func(p string) CommandResult {
		return CommandResult{ExitCode: 1, Output: "cat: " + p + ": No such file or directory\n"}
	}
	if p, ok := strings.CutPrefix(script, "cat -- "); ok {
		text, ok := f.files[unq(p)]
		if !ok {
			return missing(unq(p)), nil
		}
		return CommandResult{Output: text}, nil
	}
	if m := sedRE.FindStringSubmatch(script); m != nil {
		text, ok := f.files[unq(m[3])]
		if !ok {
			return missing(unq(m[3])), nil
		}
		from, _ := strconv.Atoi(m[1])
		to, _ := strconv.Atoi(m[2])
		lines := strings.SplitAfter(text, "\n")
		var out string
		for i := from; i <= to && i <= len(lines); i++ {
			out += lines[i-1]
		}
		return CommandResult{Output: out}, nil
	}
	return CommandResult{}, nil
}

func (f *fakeSession) Put(ctx context.Context, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, path)
	f.files[path] = string(data)
	return nil
}

func tool(t *testing.T, tools []Tool, name string) Tool {
	for _, tl := range tools {
		if tl.Spec.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return Tool{}
}

func do(t *testing.T, tl Tool, args string) (string, error) {
	t.Helper()
	return tl.Run(context.Background(), json.RawMessage(args))
}

// edit_file changes a file only when old_text is there exactly once.
func TestEditFileMatchesExactlyOnce(t *testing.T) {
	f := &fakeSession{files: map[string]string{"src/a.rs": "fn a() {}\nfn b() {}\nfn a() {}\n"}}
	edit := tool(t, SandboxTools(f), "edit_file")

	for _, c := range []struct{ args, want string }{
		{`{"path":"src/a.rs","old_text":"fn a() {}","new_text":"x"}`, "matches 2 places"},
		{`{"path":"src/a.rs","old_text":"fn c() {}","new_text":"x"}`, "is not in src/a.rs"},
		{`{"path":"src/a.rs","old_text":"","new_text":"x"}`, "give the path and the text"},
		{`{"path":"src/none.rs","old_text":"a","new_text":"x"}`, "No such file"},
	} {
		if _, err := do(t, edit, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.args, err, c.want)
		}
	}
	if len(f.puts) != 0 || f.files["src/a.rs"] != "fn a() {}\nfn b() {}\nfn a() {}\n" {
		t.Fatalf("a refused edit changed the file: puts %v, %q", f.puts, f.files["src/a.rs"])
	}

	out, err := do(t, edit, `{"path":"src/a.rs","old_text":"fn b() {}\n","new_text":"fn b() { 1 }\nfn bb() {}\n"}`)
	if err != nil || !strings.HasPrefix(out, "[exit 0]\n") {
		t.Fatalf("%q %v", out, err)
	}
	if got := f.files["src/a.rs"]; got != "fn a() {}\nfn b() { 1 }\nfn bb() {}\nfn a() {}\n" || len(f.puts) != 1 || f.puts[0] != "src/a.rs" {
		t.Fatalf("file %q, puts %v", got, f.puts)
	}
	// The same edit again finds nothing to replace.
	if _, err := do(t, edit, `{"path":"src/a.rs","old_text":"fn b() {}\n","new_text":"y"}`); err == nil {
		t.Fatal("an edit applied twice")
	}
}

// Files the session's tools read count as read for the report's citations; failed reads do not.
func TestSandboxReadsFeedCitations(t *testing.T) {
	f := &fakeSession{files: map[string]string{"src/lib.rs": "pub fn a() {}\n", "tests/it.rs": "#[test] fn t() {}\n"}}
	m := &script{replies: []Message{
		calls(call("1", "read_file", `{"path":"src/lib.rs"}`), call("2", "read_file", `{"path":"gone.rs"}`)),
		call("4", "read_file", `{"path":"/work/source/tests/it.rs"}`),
		call("5", "finish", `{"status":"met","report":"lib.rs is fine; gone.rs and tests/it.rs are too."}`),
	}}
	tr := Run(context.Background(), Config{Model: m, Tools: SandboxTools(f), MaxTurns: 10}, nil)
	if tr.Outcome != Met || tr.Report != "lib.rs is fine; gone.rs (not opened) and tests/it.rs are too." {
		t.Fatalf("%s: %q", tr.Outcome, tr.Report)
	}
	if got := strings.Join(tr.Read, " "); got != "src/lib.rs tests/it.rs" {
		t.Fatalf("read: %q", got)
	}
}

// Every command's result opens with how it ended, and a command may not outlast the agent's time.
func TestBashResults(t *testing.T) {
	f := &fakeSession{files: map[string]string{}}
	f.reply = func(s string) (CommandResult, bool) {
		switch s {
		case "make":
			return CommandResult{ExitCode: 2, Output: "boom\n", Elapsed: 3200 * time.Millisecond}, true
		case "slow":
			return CommandResult{ExitCode: 137, TimedOut: true, Elapsed: 2 * time.Second}, true
		case "quiet":
			return CommandResult{}, true
		}
		return CommandResult{}, false
	}
	bash := tool(t, SandboxTools(f), "bash")
	for _, c := range []struct{ args, want string }{
		{`{"command":"make"}`, "[exit 2, 3.2 s]\nboom\n"},
		{`{"command":"quiet"}`, "[exit 0]\n(no output)"},
		{`{"command":"slow","timeout_sec":7}`, "[timed out after 7 s]"},
	} {
		if out, err := do(t, bash, c.args); err != nil || !strings.HasPrefix(out, c.want) {
			t.Errorf("%s: %q %v, want %q", c.args, out, err, c.want)
		}
	}
	if f.limits[0] != 300*time.Second || f.limits[2] != 7*time.Second {
		t.Fatalf("limits %v", f.limits)
	}
	// With 20 seconds of the agent's time left, a command gets at most what is left.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bash.Run(ctx, json.RawMessage(`{"command":"make","timeout_sec":3000}`))
	if got := f.limits[len(f.limits)-1]; got > 16*time.Second || got < 10*time.Second {
		t.Fatalf("timeout %v with 20 s left", got)
	}
	exp, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := bash.Run(exp, json.RawMessage(`{"command":"make"}`)); err == nil {
		t.Fatal("a command ran with no time left")
	}
}

// A background command is written to a file, started apart, and read by id; ids are checked.
func TestBackgroundCommands(t *testing.T) {
	f := &fakeSession{files: map[string]string{}}
	f.reply = func(s string) (CommandResult, bool) {
		if strings.Contains(s, "tail -c 20000") {
			return CommandResult{Output: "finished, exit 0\nall tests passed\n"}, true
		}
		if strings.Contains(s, "kill -KILL") {
			return CommandResult{Output: "killed\n"}, true
		}
		return CommandResult{}, false
	}
	tools := SandboxTools(f)
	out, err := do(t, tool(t, tools, "bash"), `{"command":"cargo test 'x' && echo \"$HOME\"","background":true}`)
	if err != nil || !strings.Contains(out, "bg1") {
		t.Fatalf("%q %v", out, err)
	}
	if f.files["/work/.autobot/bg/bg1.sh"] != `cargo test 'x' && echo "$HOME"` || !strings.Contains(f.scripts[0], "nohup setsid") || !strings.Contains(f.scripts[0], "bg1.log") {
		t.Fatalf("files %v, script %q", f.files, f.scripts[0])
	}
	if out, err := do(t, tool(t, tools, "bash_output"), `{"id":"bg1"}`); err != nil || out != "[bg1: finished, exit 0]\nall tests passed\n" {
		t.Fatalf("%q %v", out, err)
	}
	if out, err := do(t, tool(t, tools, "bash_kill"), `{"id":"bg1"}`); err != nil || out != "[bg1: killed]" {
		t.Fatalf("%q %v", out, err)
	}
	n := len(f.scripts)
	for _, id := range []string{"../x", "bg1; rm -rf /", "1", ""} {
		if _, err := do(t, tool(t, tools, "bash_output"), fmt.Sprintf(`{"id":%q}`, id)); err == nil {
			t.Errorf("id %q was accepted", id)
		}
		if _, err := do(t, tool(t, tools, "bash_kill"), fmt.Sprintf(`{"id":%q}`, id)); err == nil {
			t.Errorf("id %q was accepted by bash_kill", id)
		}
	}
	if len(f.scripts) != n {
		t.Fatal("a bad id reached the shell")
	}
}

// read_file numbers lines from where it starts; write_file and grep pass what they are given safely.
func TestFileTools(t *testing.T) {
	f := &fakeSession{files: map[string]string{"a.txt": "one\ntwo\nthree\nfour\n"}}
	tools := SandboxTools(f)
	out, err := do(t, tool(t, tools, "read_file"), `{"path":"a.txt","from_line":2,"to_line":3}`)
	if err != nil || out != "[exit 0]\n     2  two\n     3  three\n" {
		t.Fatalf("%q %v", out, err)
	}
	if out, err := do(t, tool(t, tools, "read_file"), `{"path":"a.txt","numbered":false}`); err != nil || out != "[exit 0]\none\ntwo\nthree\nfour" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := do(t, tool(t, tools, "read_file"), `{"path":"nope.txt"}`); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("%v", err)
	}
	if out, err := do(t, tool(t, tools, "write_file"), `{"path":"new/b.txt","content":"hi"}`); err != nil || out != "[exit 0]\nwrote 2 bytes to new/b.txt" || f.files["new/b.txt"] != "hi" {
		t.Fatalf("%q %v", out, err)
	}
	big := strings.Repeat("x", maxWrite+1)
	if _, err := do(t, tool(t, tools, "write_file"), fmt.Sprintf(`{"path":"big","content":%q}`, big)); err == nil {
		t.Fatal("a 2 MB+ file was written")
	}
	f.reply = func(s string) (CommandResult, bool) { return CommandResult{ExitCode: 1}, true }
	if out, err := do(t, tool(t, tools, "grep"), `{"pattern":"it's"}`); err != nil || out != "[exit 1]\nNo matches." {
		t.Fatalf("%q %v", out, err)
	}
	if last := f.scripts[len(f.scripts)-1]; !strings.Contains(last, `-e 'it'\''s' -- '.'`) {
		t.Fatalf("grep script %q", last)
	}
}

// shq makes any text one bash word.
func TestShq(t *testing.T) {
	for _, s := range []string{"plain", "it's", "a b", `"$HOME"`, "x;rm -rf /", "$(id)", "`id`", "line\nbreak", "\\", "''", ""} {
		out, err := exec.Command("bash", "-c", "printf %s "+shq(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q came back as %q (%v)", s, out, err)
		}
	}
}
