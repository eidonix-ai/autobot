package connector

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

// Output is capped by keeping its beginning and its end.
func TestCapWriter(t *testing.T) {
	w := newCapWriter(100)
	w.Write([]byte(strings.Repeat("a", 30)))
	if w.String() != strings.Repeat("a", 30) {
		t.Fatalf("short output changed: %q", w.String())
	}
	w = newCapWriter(100)
	for i := 0; i < 1000; i++ { // 10 bytes at a time, 10,000 in all
		w.Write([]byte("0123456789"))
	}
	w.Write([]byte("THE END"))
	got := w.String()
	if !strings.HasPrefix(got, strings.Repeat("0123456789", 5)) || !strings.HasSuffix(got, "89THE END") || !strings.Contains(got, "9907 bytes of output left out") {
		t.Fatalf("%q", got)
	}
	if len(got) > 100+60 {
		t.Fatalf("%d bytes kept", len(got))
	}
	// Exactly at the cap nothing is left out; one write larger than the cap is cut too.
	w = newCapWriter(100)
	w.Write([]byte(strings.Repeat("x", 100)))
	if w.String() != strings.Repeat("x", 100) {
		t.Fatalf("%q", w.String())
	}
	w = newCapWriter(100)
	w.Write([]byte(strings.Repeat("a", 500) + strings.Repeat("b", 500)))
	if g := w.String(); !strings.HasPrefix(g, strings.Repeat("a", 50)+"\n[… 900 bytes") || !strings.HasSuffix(g, strings.Repeat("b", 50)) {
		t.Fatalf("%q", g)
	}
}

// A file lands in the session's own inbox folder under the command's id, never under a name the
// agent chose; an id that is not plain is refused.
func TestInboxNaming(t *testing.T) {
	run := t.TempDir()
	got, err := inboxFile(run, "c12")
	if err != nil || got != filepath.Join(run, "inbox", "c12", "data") {
		t.Fatalf("%q %v", got, err)
	}
	for _, id := range []string{"", "..", "../c1", "a/b", "c1/../../x", strings.Repeat("a", 33), "a b", "a\x00"} {
		if p, err := inboxFile(run, id); err == nil {
			t.Errorf("id %q gave %q", id, p)
		}
	}
}

// The wrapper around a command, and the copy that follows a file, keep what the agent wrote out of
// the shell's own syntax.
func TestCommandWrapping(t *testing.T) {
	script := "echo 'hi'; rm -rf / $(id)\n"
	argv := execCommand(script, 90)
	if strings.Join(argv[:5], " ") != "timeout -s KILL 90 bash" || argv[len(argv)-1] != script || strings.Contains(argv[5]+argv[6], "rm -rf") {
		t.Fatalf("%q", argv)
	}
	if !strings.Contains(argv[6], ". /work/.autobot/env") || !strings.Contains(argv[6], "cd /work/source") {
		t.Fatalf("the wrapper does not set up the environment: %q", argv[6])
	}
	put := putCommand("c3", "../../etc/x; reboot")
	if put[len(put)-2] != "../../etc/x; reboot" || put[len(put)-1] != "c3" || strings.Contains(put[6], "reboot") || !strings.Contains(put[6], `"/inbox/$2/data"`) {
		t.Fatalf("%q", put)
	}
}

// The prelude writes what it sets up where every command reads it, and only when it is complete.
func TestPreludeWritesEnvironment(t *testing.T) {
	for _, want := range []string{`setenv CARGO_HOME /work/cargo`, `setenv PYTHONPATH`, `mv /work/.autobot/env.tmp /work/.autobot/env`} {
		if !strings.Contains(prelude, want) {
			t.Errorf("the prelude lacks %q", want)
		}
	}
	if strings.Contains(prelude, "export CARGO_HOME") || strings.Contains(prelude, "export PYTHONPATH") {
		t.Error("the prelude exports a variable without recording it")
	}
}

// fakeEngine is Docker's exec API on a Unix socket: it records the commands and answers with a
// framed stream and an exit code.
type fakeEngine struct {
	mu    sync.Mutex
	cmds  [][]string
	files map[string]string // inbox files seen while a command ran, by command id
	inbox string
	code  int
	out   [][]byte // frames of the command's output, each stream-prefixed
}

func (f *fakeEngine) serve(t *testing.T) *docker {
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Cmd []string }
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.cmds = append(f.cmds, body.Cmd)
		// A copy command: see the file it will copy while it can still be read.
		if strings.Contains(strings.Join(body.Cmd, " "), "/inbox/") {
			cid := body.Cmd[len(body.Cmd)-1]
			if b, err := os.ReadFile(filepath.Join(f.inbox, cid, "data")); err == nil {
				f.files[cid] = string(b)
			}
		}
		f.mu.Unlock()
		io.WriteString(w, `{"Id":"e1"}`)
	})
	mux.HandleFunc("POST /exec/e1/start", func(w http.ResponseWriter, r *http.Request) {
		for _, fr := range f.out {
			var head [8]byte
			head[0] = fr[0]
			binary.BigEndian.PutUint32(head[4:], uint32(len(fr)-1))
			w.Write(head[:])
			w.Write(fr[1:])
		}
	})
	mux.HandleFunc("GET /exec/e1/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": f.code})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return newDocker(sock)
}

// A command runs through Docker's exec, its stdout and stderr come back in order, and its exit code
// is Docker's.
func TestRunCommand(t *testing.T) {
	run := t.TempDir()
	f := &fakeEngine{files: map[string]string{}, code: 3, inbox: inboxDir(run),
		out: [][]byte{append([]byte{1}, "out one\n"...), append([]byte{2}, "err two\n"...), append([]byte{1}, "out three\n"...)}}
	c := &Connector{docker: f.serve(t)}
	var log bytes.Buffer
	res := c.runCommand(context.Background(), "ctr", run, protocol.Command{ID: "c1", Script: "make", TimeoutSec: 30}, &log)
	if res.ExitCode != 3 || res.Output != "out one\nerr two\nout three\n" || res.Error != "" || res.TimedOut {
		t.Fatalf("%+v", res)
	}
	if len(f.cmds) != 1 || f.cmds[0][len(f.cmds[0])-1] != "make" || f.cmds[0][3] != "30" {
		t.Fatalf("exec %q", f.cmds)
	}
	if !strings.Contains(log.String(), "command c1: make: exit 3") {
		t.Fatalf("log %q", log.String())
	}

	// A file is written to the inbox, copied by a command, and removed afterwards.
	f.code, f.out = 0, nil
	res = c.runCommand(context.Background(), "ctr", run, protocol.Command{ID: "c2", Path: "src/new.rs", Data: []byte("fn main() {}\n")}, &log)
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("%+v", res)
	}
	if f.files["c2"] != "fn main() {}\n" {
		t.Fatalf("the copy did not find the file in the inbox: %v", f.files)
	}
	if _, err := os.Stat(filepath.Join(inboxDir(run), "c2")); !os.IsNotExist(err) {
		t.Fatalf("the inbox file was left behind: %v", err)
	}
	// Whatever name the agent gave the file, nothing outside the inbox is touched.
	before := len(f.cmds)
	for _, id := range []string{"../../escape", "a/b"} {
		res = c.runCommand(context.Background(), "ctr", run, protocol.Command{ID: id, Path: "x", Data: []byte("x")}, &log)
		if res.Error == "" || len(f.cmds) != before {
			t.Fatalf("id %q: %+v", id, res)
		}
	}
	if _, err := os.Stat(filepath.Join(run, "escape")); err == nil {
		t.Fatal("a file was written outside the inbox")
	}
}
