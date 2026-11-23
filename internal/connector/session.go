package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

// A session is a job container that stays open for an agent. Its commands come from Autobot (a
// request for them is held open until there are some) and run in the container with Docker's exec:
// the same container as a script would get, so a command can never reach the donor's machine, its
// network or its files. Several may run at once.
//
// A file the agent writes cannot be sent into the container, which has no network and a read-only
// root. The connector writes it on the host, in the session's own inbox folder, under the command's
// id (checked to be plain, never a name the agent chose), and the container sees that folder
// read-only at /inbox; a command then copies the file to where the agent asked.

// inboxDir is a session's inbox, inside the run's folder (which is removed when the run ends).
func inboxDir(runDir string) string { return filepath.Join(runDir, "inbox") }

var cidRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// maxRunning is how many of a session's commands run at once; the rest wait their turn.
const maxRunning = 8

// inboxFile is where a command's file is written on the host.
func inboxFile(runDir, cid string) (string, error) {
	if !cidRE.MatchString(cid) {
		return "", fmt.Errorf("bad command id %q", cid)
	}
	return filepath.Join(inboxDir(runDir), cid, "data"), nil
}

// Every command sources the environment the prelude set up, then runs in the code's folder (or
// /work when there is no code) with the script as its argument.
const sessionWrapper = `. /work/.autobot/env; cd /work/source 2>/dev/null || cd /work; eval "$1"`

// execCommand is the process a bash command runs as. timeout kills it, and everything it started,
// when the time is up.
func execCommand(script string, timeoutSec int) []string {
	return []string{"timeout", "-s", "KILL", strconv.Itoa(timeoutSec), "bash", "-c", sessionWrapper, "bash", script}
}

// putCommand copies a file from the inbox to its path, making the folders above it. A relative path
// is relative to the code's folder. Both names are arguments, never part of the script.
func putCommand(cid, path string) []string {
	return []string{"timeout", "-s", "KILL", "60", "bash", "-c",
		`cd /work/source 2>/dev/null || cd /work; mkdir -p -- "$(dirname -- "$1")" && cat -- "/inbox/$2/data" > "$1"`, "bash", path, cid}
}

// capWriter keeps the beginning and the end of what is written to it, about max bytes in all.
type capWriter struct {
	half  int
	head  []byte
	tail  []byte
	total int64
}

func newCapWriter(max int) *capWriter { return &capWriter{half: max / 2} }

func (w *capWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	rest := p
	if room := w.half - len(w.head); room > 0 {
		n := min(room, len(rest))
		w.head = append(w.head, rest[:n]...)
		rest = rest[n:]
	}
	w.tail = append(w.tail, rest...)
	if len(w.tail) > 2*w.half {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-w.half:]...)
	}
	return len(p), nil
}

func (w *capWriter) String() string {
	tail := w.tail
	if len(tail) > w.half {
		tail = tail[len(tail)-w.half:]
	}
	if left := w.total - int64(len(w.head)) - int64(len(tail)); left > 0 {
		return string(w.head) + fmt.Sprintf("\n[… %d bytes of output left out …]\n", left) + string(tail)
	}
	return string(w.head) + string(tail)
}

// serveSession takes the session's commands from Autobot and runs them until ctx ends or Autobot
// says the session is over.
func (c *Connector) serveSession(ctx context.Context, run int64, container, runDir string, out io.Writer) {
	// The container is ready when the prelude has set up the code and the environment.
	for {
		code, err := c.docker.exec(ctx, container, []string{"test", "-f", "/work/.autobot/env"}, io.Discard)
		if err == nil && code == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	long := &http.Client{Timeout: protocol.PollWait + 15*time.Second}
	// At most this many commands run at once, however many arrive: Docker on the donor's machine
	// starts a process for each.
	slots := make(chan struct{}, maxRunning)
	for ctx.Err() == nil {
		cmds, status, err := c.pollCommands(ctx, long, run)
		if status == http.StatusNotFound {
			return // the run is over, or is not this machine's session
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, cmd := range cmds {
			go func() {
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				res := c.runCommand(ctx, container, runDir, cmd, out)
				<-slots
				c.reportCommand(ctx, run, cmd.ID, res)
			}()
		}
	}
}

// pollCommands asks Autobot for commands, which it holds the request open for.
func (c *Connector) pollCommands(ctx context.Context, client *http.Client, run int64) ([]protocol.Command, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/connector/v1/runs/%d/commands", strings.TrimSuffix(c.cfg.URL, "/"), run), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "autobot-connector/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("commands: %s", resp.Status)
	}
	var cs protocol.Commands
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*protocol.MaxFile)).Decode(&cs); err != nil {
		return nil, resp.StatusCode, err
	}
	return cs.Commands, resp.StatusCode, nil
}

// reportCommand sends a command's result, trying again for a minute in case Autobot is busy.
func (c *Connector) reportCommand(ctx context.Context, run int64, cid string, res protocol.CommandResult) {
	path := fmt.Sprintf("/connector/v1/runs/%d/commands/%s/result", run, cid)
	for try, wait := 0, time.Second; try < 6; try, wait = try+1, wait*2 {
		err := c.post(ctx, path, res, nil)
		if err == nil || strings.Contains(err.Error(), "404") || ctx.Err() != nil {
			return
		}
		time.Sleep(wait)
	}
	log.Printf("could not report command %s of run %d", cid, run)
}

// runCommand runs one command or writes one file in the session's container.
func (c *Connector) runCommand(ctx context.Context, container, runDir string, cmd protocol.Command, out io.Writer) protocol.CommandResult {
	res := protocol.CommandResult{ExitCode: -1}
	if !cmd.Valid() {
		res.Error = "the command is not valid"
		return res
	}
	start := time.Now()
	buf := newCapWriter(protocol.MaxCommandOutput)
	var argv []string
	timeout := time.Minute
	what := "write " + cmd.Path
	if cmd.Path != "" {
		file, err := inboxFile(runDir, cmd.ID)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			res.Error = err.Error()
			return res
		}
		defer os.RemoveAll(filepath.Dir(file))
		if err := os.WriteFile(file, cmd.Data, 0o644); err != nil {
			res.Error = err.Error()
			return res
		}
		argv = putCommand(cmd.ID, cmd.Path)
	} else {
		argv = execCommand(cmd.Script, cmd.TimeoutSec)
		timeout = time.Duration(cmd.TimeoutSec) * time.Second
		what, _, _ = strings.Cut(strings.TrimSpace(cmd.Script), "\n")
		if len(what) > 100 {
			what = what[:100] + "…"
		}
	}
	// timeout inside the container ends the command; this ends the wait if that did not work.
	ectx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	code, err := c.docker.exec(ectx, container, argv, buf)
	res.Output, res.ExitCode = buf.String(), code
	switch {
	case ectx.Err() != nil && ctx.Err() == nil:
		res.TimedOut, res.Error = true, ""
	case err != nil:
		res.Error = err.Error()
	case code == 137 && cmd.Path == "" && time.Since(start) >= timeout-time.Second:
		res.TimedOut = true // killed by timeout
	}
	fmt.Fprintf(out, "[command %s: %s: exit %d, %.1f s]\n", cmd.ID, what, res.ExitCode, time.Since(start).Seconds())
	return res
}
