package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Session is a shell in a sandbox on a donated machine: a container with the code in it, no
// network, and no way to touch the machine. The harness runs nothing itself; these tools send
// every command through a Session, which Autobot's web package implements over the connector.
type Session interface {
	// Run runs a bash script in the code's folder and waits for it to end or for timeout.
	Run(ctx context.Context, script string, timeout time.Duration) (CommandResult, error)
	// Put writes a file in the sandbox, at an absolute path or one relative to the code's folder.
	Put(ctx context.Context, path string, data []byte) error
}

// CommandResult is how a command ended. Output is its stdout and stderr together, in order, with
// the middle left out when it is very long.
type CommandResult struct {
	ExitCode int
	Output   string
	TimedOut bool
	Elapsed  time.Duration
}

const (
	defaultBashTimeout = 300 * time.Second
	quickTimeout       = 60 * time.Second // reading, searching and listing
	maxWrite           = 2 << 20          // the largest file the agent may write or edit
	maxLines           = 2000             // lines read_file shows at once
	maxMatches         = 300              // lines grep shows
	maxPaths           = 500              // paths glob shows
)

// shq quotes a string for bash as one word.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// header is the first line of a command's result, so the model (and a report) can cite how it ended.
func header(r CommandResult, limit time.Duration) string {
	switch {
	case r.TimedOut:
		return fmt.Sprintf("[timed out after %.0f s]", limit.Seconds())
	case r.Elapsed >= time.Second:
		return fmt.Sprintf("[exit %d, %.1f s]", r.ExitCode, r.Elapsed.Seconds())
	}
	return fmt.Sprintf("[exit %d]", r.ExitCode)
}

// shown is a command's result as the tools show it: the header, then what it printed.
func shown(r CommandResult, limit time.Duration) string {
	out := strings.ToValidUTF8(r.Output, "�")
	if strings.TrimSpace(out) == "" {
		return header(r, limit) + "\n(no output)"
	}
	return header(r, limit) + "\n" + out
}

// within clamps a timeout to what is left of the agent's time.
func within(ctx context.Context, want time.Duration) (time.Duration, error) {
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl) - 5*time.Second
		if left <= 0 {
			return 0, errors.New("there is no time left to run a command")
		}
		want = min(want, left)
	}
	return max(want, time.Second), nil
}

// relPath is a path as the agent may cite it: without a leading ./ or the code's folder.
func relPath(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	return strings.TrimPrefix(p, "/work/source/")
}

var bgIDRE = regexp.MustCompile(`^bg[0-9]{1,6}$`)

const bgDir = "/work/.autobot/bg"

// SandboxTools are the tools of an agent with a shell: run commands (in the foreground or the
// background), read, write and edit files, search and list them.
func SandboxTools(s Session) []Tool {
	var editing sync.Mutex // two edits of one file in one turn must not overwrite each other
	var jobs atomic.Int32
	sandbox := "The sandbox has no network and its root is read-only; only /work and /tmp are writable. "
	return []Tool{
		{Spec: ToolSpec{Name: "bash", Description: "Run a bash command in the sandbox, in the repository's folder (/work/source), and get its output and exit code. " +
			sandbox + "A command ends at its timeout (default 300 seconds). For something long, such as a build or a test suite, set background to true: " +
			"it starts at once and returns an id, and you read its output later with bash_output.",
			Parameters: schema(`{"type":"object","properties":{"command":{"type":"string"},
				"timeout_sec":{"type":"integer","description":"default 300; at most the time you have left"},
				"background":{"type":"boolean","description":"start it and return at once with an id for bash_output and bash_kill"}},"required":["command"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Command    string
					TimeoutSec int  `json:"timeout_sec"`
					Background bool `json:"background"`
				}](raw)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Command) == "" {
					return "", errors.New("give the command to run")
				}
				if a.Background {
					id := fmt.Sprintf("bg%d", jobs.Add(1))
					// The command is written to a file, so no quoting can change it.
					if err := s.Put(ctx, bgDir+"/"+id+".sh", []byte(a.Command)); err != nil {
						return "", err
					}
					r, err := s.Run(ctx, startBackground(id), quickTimeout)
					if err != nil {
						return "", err
					}
					if r.ExitCode != 0 {
						return "", fmt.Errorf("could not start it: %s", strings.TrimSpace(r.Output))
					}
					return fmt.Sprintf("%s\nStarted background job %s. Read its output with bash_output (id %s); stop it with bash_kill.", header(r, 0), id, id), nil
				}
				limit := time.Duration(a.TimeoutSec) * time.Second
				if limit <= 0 {
					limit = defaultBashTimeout
				}
				if limit, err = within(ctx, limit); err != nil {
					return "", err
				}
				r, err := s.Run(ctx, a.Command, limit)
				if err != nil {
					return "", err
				}
				return shown(r, limit), nil
			}},
		{Spec: ToolSpec{Name: "bash_output", Description: "Read a background job's output so far (its last 20 KB) and say whether it is still running.",
			Parameters: schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ ID string }](raw)
				if err != nil || !bgIDRE.MatchString(a.ID) {
					return "", errors.New("id is the one bash returned, such as bg1")
				}
				r, err := s.Run(ctx, readBackground(a.ID), quickTimeout)
				if err != nil {
					return "", err
				}
				state, log, _ := strings.Cut(r.Output, "\n")
				if r.ExitCode != 0 {
					return "", fmt.Errorf("there is no background job %s", a.ID)
				}
				if strings.TrimSpace(log) == "" {
					log = "(no output yet)"
				}
				return fmt.Sprintf("[%s: %s]\n%s", a.ID, state, log), nil
			}},
		{Spec: ToolSpec{Name: "bash_kill", Description: "Stop a background job.",
			Parameters: schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ ID string }](raw)
				if err != nil || !bgIDRE.MatchString(a.ID) {
					return "", errors.New("id is the one bash returned, such as bg1")
				}
				r, err := s.Run(ctx, killBackground(a.ID), quickTimeout)
				if err != nil {
					return "", err
				}
				if r.ExitCode != 0 {
					return "", fmt.Errorf("there is no background job %s", a.ID)
				}
				return fmt.Sprintf("[%s: %s]", a.ID, strings.TrimSpace(r.Output)), nil
			}},
		{Spec: ToolSpec{Name: "read_file", Description: "Read a file in the sandbox (the repository's files are under /work/source, the working folder), " +
			"with line numbers, optionally only some lines. At most 2000 lines at once.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"from_line":{"type":"integer"},"to_line":{"type":"integer"},
				"numbered":{"type":"boolean","description":"default true"}},"required":["path"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				return []string{relPath(a.Path)}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path     string
					FromLine int   `json:"from_line"`
					ToLine   int   `json:"to_line"`
					Numbered *bool `json:"numbered"`
				}](raw)
				if err != nil {
					return "", err
				}
				if a.Path == "" {
					return "", errors.New("give the path of the file")
				}
				from := max(a.FromLine, 1)
				to, windowed := a.ToLine, false
				if to < from || to-from >= maxLines {
					to, windowed = from+maxLines-1, true
				}
				r, err := s.Run(ctx, fmt.Sprintf("sed -n '%d,%dp' -- %s", from, to, shq(a.Path)), quickTimeout)
				if err != nil {
					return "", err
				}
				if r.ExitCode != 0 {
					return "", errors.New(strings.TrimSpace(r.Output))
				}
				if strings.ContainsRune(r.Output, 0) {
					return "", errors.New("this looks like a binary file")
				}
				text := strings.TrimSuffix(r.Output, "\n")
				if a.Numbered != nil && !*a.Numbered {
					return header(r, 0) + "\n" + text, nil
				}
				var b strings.Builder
				n := 0
				for i, l := range strings.Split(text, "\n") {
					fmt.Fprintf(&b, "%6d  %s\n", from+i, l)
					n++
				}
				if r.Output == "" {
					return header(r, 0) + "\n(nothing at those lines)", nil
				}
				if windowed && n == maxLines {
					fmt.Fprintf(&b, "[lines %d to %d shown; ask for more with from_line=%d]\n", from, to, to+1)
				}
				return header(r, 0) + "\n" + b.String(), nil
			}},
		{Spec: ToolSpec{Name: "write_file", Description: "Write a file in the sandbox, replacing it if it exists, with the folders above it made. A relative path is relative to /work/source. Up to 2 MB.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				return []string{relPath(a.Path)}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Path, Content string }](raw)
				if err != nil {
					return "", err
				}
				if a.Path == "" || len(a.Content) > maxWrite {
					return "", errors.New("give a path and content of up to 2 MB")
				}
				editing.Lock()
				defer editing.Unlock()
				if err := s.Put(ctx, a.Path, []byte(a.Content)); err != nil {
					return "", err
				}
				return fmt.Sprintf("[exit 0]\nwrote %d bytes to %s", len(a.Content), a.Path), nil
			}},
		{Spec: ToolSpec{Name: "edit_file", Description: "Change a file in the sandbox by replacing old_text with new_text. old_text must match the file exactly (whitespace included) " +
			"and exactly once: include enough surrounding lines to make it unique.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}},"required":["path","old_text","new_text"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				return []string{relPath(a.Path)}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path    string
					OldText string `json:"old_text"`
					NewText string `json:"new_text"`
				}](raw)
				if err != nil {
					return "", err
				}
				if a.Path == "" || a.OldText == "" {
					return "", errors.New("give the path and the text to replace")
				}
				editing.Lock()
				defer editing.Unlock()
				r, err := s.Run(ctx, "cat -- "+shq(a.Path), quickTimeout)
				if err != nil {
					return "", err
				}
				if r.ExitCode != 0 {
					return "", errors.New(strings.TrimSpace(r.Output))
				}
				if cutRE.MatchString(r.Output) {
					return "", errors.New("the file is too large to edit this way; change it with bash (sed, python) instead")
				}
				switch n := strings.Count(r.Output, a.OldText); {
				case n == 0:
					return "", fmt.Errorf("old_text is not in %s: it must match the file exactly, whitespace included; read the file again", a.Path)
				case n > 1:
					return "", fmt.Errorf("old_text matches %d places in %s; add surrounding lines to make it unique", n, a.Path)
				}
				text := strings.Replace(r.Output, a.OldText, a.NewText, 1)
				if len(text) > maxWrite {
					return "", errors.New("the edited file would be over 2 MB")
				}
				if err := s.Put(ctx, a.Path, []byte(text)); err != nil {
					return "", err
				}
				return fmt.Sprintf("[exit 0]\nedited %s: replaced %d lines with %d", a.Path, strings.Count(a.OldText, "\n")+1, strings.Count(a.NewText, "\n")+1), nil
			}},
		{Spec: ToolSpec{Name: "grep", Description: "Search file contents under a path (default the repository) for a regular expression. Shows file:line:text, up to 300 lines.",
			Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"a file or folder; default ."}},"required":["pattern"]}`)},
			Reads: func(raw json.RawMessage) []string {
				// Searching one file shows what is in it; searching a folder says nothing about which files.
				if a, _ := args[struct{ Path string }](raw); path.Ext(a.Path) != "" {
					return []string{relPath(a.Path)}
				}
				return nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Pattern, Path string }](raw)
				if err != nil {
					return "", err
				}
				if a.Pattern == "" {
					return "", errors.New("give the pattern to search for")
				}
				if a.Path == "" {
					a.Path = "."
				}
				script := fmt.Sprintf(`if command -v rg >/dev/null 2>&1; then rg -n --no-heading --color never -e %[1]s -- %[2]s; else grep -rnIE -e %[1]s -- %[2]s; fi`, shq(a.Pattern), shq(a.Path))
				r, err := s.Run(ctx, script, quickTimeout)
				if err != nil {
					return "", err
				}
				switch r.ExitCode {
				case 0:
					lines := strings.Split(strings.TrimSuffix(r.Output, "\n"), "\n")
					out := strings.Join(lines[:min(len(lines), maxMatches)], "\n")
					if len(lines) > maxMatches {
						out += fmt.Sprintf("\n[%d of %d matching lines shown; narrow the pattern or the path]", maxMatches, len(lines))
					}
					return header(r, 0) + "\n" + out, nil
				case 1:
					return header(r, 0) + "\nNo matches.", nil
				}
				return "", errors.New(strings.TrimSpace(r.Output))
			}},
		{Spec: ToolSpec{Name: "glob", Description: "List files whose path matches a pattern such as *.rs, Cargo.toml or src/*/mod.rs (* also matches across folders). Up to 500 paths.",
			Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Pattern string }](raw)
				if err != nil {
					return "", err
				}
				pat := strings.TrimPrefix(strings.TrimSpace(a.Pattern), "./")
				if pat == "" {
					return "", errors.New("give the pattern to match")
				}
				test := "-name " + shq(pat)
				if strings.Contains(pat, "/") {
					test = "-path " + shq("./"+strings.ReplaceAll(strings.TrimPrefix(pat, "/"), "**", "*"))
				}
				script := `find . \( -name .git -o -name node_modules \) -prune -o -type f ` + test + ` -print | sort`
				r, err := s.Run(ctx, script, quickTimeout)
				if err != nil {
					return "", err
				}
				paths := strings.Split(strings.TrimSpace(r.Output), "\n")
				if len(paths) == 1 && paths[0] == "" {
					return header(r, 0) + "\nNo files match.", nil
				}
				for i := range paths {
					paths[i] = strings.TrimPrefix(paths[i], "./")
				}
				out := strings.Join(paths[:min(len(paths), maxPaths)], "\n")
				if len(paths) > maxPaths {
					out += fmt.Sprintf("\n[%d of %d paths shown; narrow the pattern]", maxPaths, len(paths))
				}
				return header(r, 0) + "\n" + out, nil
			}},
	}
}

// cutRE finds the note the connector leaves where it cut the middle out of very long output.
var cutRE = regexp.MustCompile(`\[… \d+ bytes of output left out …\]`)

// startBackground starts the job whose command is in bgDir/id.sh, in its own session so that it
// outlives the command that starts it, with its output in id.log, its process group's leader in
// id.pid (written by the job's own shell, since setsid may or may not fork) and its exit code, when
// it ends by itself, in id.exit. It returns once the pid is written.
func startBackground(id string) string {
	return fmt.Sprintf(`cd %[1]s && nohup setsid bash -c 'echo $$ > %[1]s/%[2]s.pid; . /work/.autobot/env; cd /work/source 2>/dev/null || cd /work; bash %[1]s/%[2]s.sh; echo $? > %[1]s/%[2]s.exit' > %[2]s.log 2>&1 < /dev/null &
for i in $(seq 100); do [ -s %[1]s/%[2]s.pid ] && break; sleep 0.05; done; [ -s %[1]s/%[2]s.pid ]`, bgDir, id)
}

// readBackground prints a job's state on one line, then the end of its log.
func readBackground(id string) string {
	return fmt.Sprintf(`cd %[1]s 2>/dev/null && [ -f %[2]s.pid ] || exit 3
if kill -0 "$(cat %[2]s.pid)" 2>/dev/null; then echo "still running"
elif [ -f %[2]s.exit ]; then echo "finished, exit $(cat %[2]s.exit)"
else echo "stopped without an exit code (killed)"; fi
tail -c 20000 %[2]s.log`, bgDir, id)
}

// killBackground stops a job and everything it started, and says what it found.
func killBackground(id string) string {
	return fmt.Sprintf(`cd %[1]s 2>/dev/null && [ -f %[2]s.pid ] || exit 3
pid="$(cat %[2]s.pid)"
if kill -0 "$pid" 2>/dev/null; then kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid"; echo killed; else echo "was not running"; fi`, bgDir, id)
}
