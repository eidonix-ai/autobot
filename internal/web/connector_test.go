package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

func (e *env) connector(t *testing.T, token, path string, body any) (int, protocol.Reply) {
	t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply protocol.Reply
	json.NewDecoder(resp.Body).Decode(&reply)
	return resp.StatusCode, reply
}

// Machine tokens and run ownership guard the connector API.
func TestConnectorAPI(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	cn7, tok7, _ := e.st.AddMachine(ws.ID, "cn7", admin)
	_, tokB, _ := e.st.AddMachine(ws.ID, "other", admin)
	idle := protocol.Status{Image: "sha256:x", GPUs: []protocol.GPU{{UUID: "GPU-1"}}}

	for _, tok := range []string{"", "abm_wrong", "not-a-token"} {
		if code, _ := e.connector(t, tok, "/connector/v1/heartbeat", idle); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}

	// A queued run goes to its machine on the next check-in, once.
	run, err := e.st.QueueRun(cn7, "echo hi", "main", "0123456789abcdef0123456789abcdef01234567", 1, time.Minute, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, r := e.connector(t, tokB, "/connector/v1/heartbeat", idle); r.Job != nil {
		t.Fatal("another machine was given cn7's run")
	}
	_, r := e.connector(t, tok7, "/connector/v1/heartbeat", idle)
	if r.Job == nil || r.Job.Run != run.ID || r.Job.Repo != "o/r" || r.Job.Script != "echo hi" || !r.Job.Valid() {
		t.Fatalf("job %+v", r.Job)
	}
	busy := idle
	busy.Run = run.ID
	if _, r := e.connector(t, tok7, "/connector/v1/heartbeat", busy); r.Job != nil || r.Cancel {
		t.Fatalf("second check-in: %+v", r)
	}

	// Only the machine running it can add output or finish it.
	out := "/connector/v1/runs/" + itoa(run.ID) + "/output"
	if code, _ := e.connector(t, tokB, out, "forged"); code != http.StatusNotFound {
		t.Fatalf("other machine's output: %d", code)
	}
	if code, _ := e.connector(t, tok7, out, "+ echo hi\nhi\n"); code != http.StatusOK {
		t.Fatalf("output: %d", code)
	}
	if code, _ := e.connector(t, tokB, "/connector/v1/runs/"+itoa(run.ID)+"/result", protocol.Result{}); code != http.StatusNotFound {
		t.Fatalf("other machine's result: %d", code)
	}
	if code, _ := e.connector(t, tok7, "/connector/v1/runs/"+itoa(run.ID)+"/result", protocol.Result{ExitCode: 0}); code != http.StatusOK {
		t.Fatalf("result: %d", code)
	}
	got, _ := e.st.Run(run.ID)
	if got.State != "passed" || got.Output != "+ echo hi\nhi\n" {
		t.Fatalf("run %s %q", got.State, got.Output)
	}

	// A connector that comes back without the run Autobot gave it: the run is lost.
	run2, _ := e.st.QueueRun(cn7, "sleep 60", "", "", 0, time.Minute, admin)
	e.connector(t, tok7, "/connector/v1/heartbeat", idle) // takes run2
	e.connector(t, tok7, "/connector/v1/heartbeat", idle) // restarted: no run
	if got, _ := e.st.Run(run2.ID); got.State != "error" || !strings.Contains(got.Summary, "lost") {
		t.Fatalf("lost run: %s %q", got.State, got.Summary)
	}

	// A removed machine's token stops working.
	e.st.RevokeMachine(cn7, admin)
	if code, _ := e.connector(t, tok7, "/connector/v1/heartbeat", idle); code != http.StatusUnauthorized {
		t.Fatalf("removed machine: %d", code)
	}
}
