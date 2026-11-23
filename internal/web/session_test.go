package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

const headSHA = "0123456789abcdef0123456789abcdef01234567"

var idleStatus = protocol.Status{Image: "sha256:x", Arch: "amd64", JobCPUs: 4, JobMemoryGiB: 24, JobScratchGiB: 16,
	GPUs: []protocol.GPU{{UUID: "GPU-1", Name: "NVIDIA Test GPU"}}}

// req sends a connector request with a machine token.
func (e *env) req(t *testing.T, method, token, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A session's commands go only to the machine that holds it, and only while it is open.
func TestSessionCommandsGuarded(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	cn7, tok7, _ := e.st.AddMachine(ws.ID, "cn7", admin)
	_, tokB, _ := e.st.AddMachine(ws.ID, "other", admin)
	plain, tokP, _ := e.st.AddMachine(ws.ID, "plain", admin)

	se, err := e.ui.OpenSession(cn7, headSHA, headSHA, false, 1, time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/connector/v1/runs/%d/commands", se.RunID())

	// Queued, not yet taken: nothing to serve.
	if code, _ := e.req(t, "GET", tok7, path, nil); code != http.StatusNotFound {
		t.Fatalf("commands of a session that has not started: %d", code)
	}
	// The connector takes the session as a job with no script.
	_, body := e.req(t, "POST", tok7, "/connector/v1/heartbeat", idleStatus)
	var r protocol.Reply
	json.Unmarshal([]byte(body), &r)
	if r.Job == nil || !r.Job.Session || r.Job.Script != "" || !r.Job.Valid() || r.Job.GPUs != 1 || r.Job.Commit != headSHA {
		t.Fatalf("job %+v", r.Job)
	}

	// Another machine's token gets nothing, however it asks.
	if code, _ := e.req(t, "GET", tokB, path, nil); code != http.StatusNotFound {
		t.Fatalf("another machine took the session's commands: %d", code)
	}
	if code, _ := e.req(t, "GET", "abm_wrong", path, nil); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", code)
	}

	// The machine that holds it takes a command and answers it.
	type answer struct {
		res agentResult
		err error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := se.Run(context.Background(), "ls", 10*time.Second)
		got <- answer{agentResult{res.ExitCode, res.Output}, err}
	}()
	code, body := e.req(t, "GET", tok7, path, nil)
	var cmds protocol.Commands
	json.Unmarshal([]byte(body), &cmds)
	if code != http.StatusOK || len(cmds.Commands) != 1 || cmds.Commands[0].Script != "ls" || cmds.Commands[0].TimeoutSec != 10 || !cmds.Commands[0].Valid() {
		t.Fatalf("commands: %d %s", code, body)
	}
	cid := cmds.Commands[0].ID
	result := fmt.Sprintf("%s/%s/result", path, cid)
	if code, _ := e.req(t, "POST", tokB, result, protocol.CommandResult{Output: "forged"}); code != http.StatusNotFound {
		t.Fatalf("another machine answered the command: %d", code)
	}
	if code, _ := e.req(t, "POST", tok7, path+"/nope/result", protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("an answer to no command: %d", code)
	}
	if code, _ := e.req(t, "POST", tok7, result, protocol.CommandResult{ExitCode: 3, Output: "a\nb\n"}); code != http.StatusOK {
		t.Fatalf("the answer: %d", code)
	}
	if a := <-got; a.err != nil || a.res.code != 3 || a.res.out != "a\nb\n" {
		t.Fatalf("answer %+v", a)
	}
	// An answer counts once.
	if code, _ := e.req(t, "POST", tok7, result, protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("a second answer: %d", code)
	}

	// A run that is not a session has no commands to serve, and takes no command results.
	if _, err := e.st.QueueRun(plain, "true", "", "", false, 0, time.Minute, admin); err != nil {
		t.Fatal(err)
	}
	e.req(t, "POST", tokP, "/connector/v1/heartbeat", idleStatus)
	pr, _ := e.st.ActiveRun(plain.ID)
	e.ui.sessions.open(pr.ID) // even if a queue existed for it
	if code, _ := e.req(t, "GET", tokP, fmt.Sprintf("/connector/v1/runs/%d/commands", pr.ID), nil); code != http.StatusNotFound {
		t.Fatalf("commands of an ordinary run: %d", code)
	}
	if code, _ := e.req(t, "POST", tokP, fmt.Sprintf("/connector/v1/runs/%d/commands/c1/result", pr.ID), protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("a command result for an ordinary run: %d", code)
	}

	// Closed, the session serves nothing more, and its machine is told to stop it.
	se.Close()
	if code, _ := e.req(t, "GET", tok7, path, nil); code != http.StatusNotFound {
		t.Fatalf("commands after close: %d", code)
	}
	if code, _ := e.req(t, "POST", tok7, result, protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("result after close: %d", code)
	}
	busy := idleStatus
	busy.Run = se.RunID()
	_, body = e.req(t, "POST", tok7, "/connector/v1/heartbeat", busy)
	json.Unmarshal([]byte(body), &r)
	if !r.Cancel {
		t.Fatalf("the machine was not told to stop the closed session: %s", body)
	}
	if _, err := se.Run(context.Background(), "ls", time.Second); err == nil {
		t.Fatal("a closed session ran a command")
	}
	// The connector ends the run; a finished session serves nothing either.
	e.req(t, "POST", tok7, fmt.Sprintf("/connector/v1/runs/%d/result", se.RunID()), protocol.Result{Cancelled: true, ExitCode: 137})
	if run, _ := e.st.Run(se.RunID()); run.State != "passed" || run.Summary != "Session closed." {
		t.Fatalf("run %s %q", run.State, run.Summary)
	}
	if code, _ := e.req(t, "GET", tok7, path, nil); code != http.StatusNotFound {
		t.Fatalf("commands of a finished session: %d", code)
	}

	// Stopped from its run page, a session serves nothing more, though its agent has not noticed yet.
	stopped, err := e.ui.OpenSession(cn7, "", "", false, 0, time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer stopped.Close()
	e.req(t, "POST", tok7, "/connector/v1/heartbeat", idleStatus)
	stoppedPath := fmt.Sprintf("/connector/v1/runs/%d/commands", stopped.RunID())
	e.st.RequestCancel(stopped.RunID())
	if code, _ := e.req(t, "GET", tok7, stoppedPath, nil); code != http.StatusNotFound {
		t.Fatalf("commands of a session whose stop was asked for: %d", code)
	}
	if code, _ := e.req(t, "POST", tok7, stoppedPath+"/c1/result", protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("a result for a session whose stop was asked for: %d", code)
	}

	// A session that ends on its own (its machine went quiet) while its queue still exists: a run
	// that is over serves nothing, and a command waiting on it is answered with why.
	other, _ := e.st.MachineByToken(tokB)
	lost, err := e.ui.OpenSession(other, "", "", false, 0, time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer lost.Close()
	e.req(t, "POST", tokB, "/connector/v1/heartbeat", idleStatus)
	lostPath := fmt.Sprintf("/connector/v1/runs/%d/commands", lost.RunID())
	waiting := make(chan error, 1)
	go func() {
		_, err := lost.Run(context.Background(), "ls", time.Second)
		waiting <- err
	}()
	code, body = e.req(t, "GET", tokB, lostPath, nil)
	if code != http.StatusOK || !strings.Contains(body, `"ls"`) {
		t.Fatalf("commands: %d %s", code, body)
	}
	e.st.FinishRun(lost.RunID(), "error", "The machine stopped checking in during the run.")
	if code, _ := e.req(t, "GET", tokB, lostPath, nil); code != http.StatusNotFound {
		t.Fatalf("commands of a run that is over: %d", code)
	}
	if code, _ := e.req(t, "POST", tokB, lostPath+"/c1/result", protocol.CommandResult{}); code != http.StatusNotFound {
		t.Fatalf("a result for a run that is over: %d", code)
	}
	select {
	case err := <-waiting:
		if err == nil || !strings.Contains(err.Error(), "the session ended: The machine stopped checking in") {
			t.Fatalf("the waiting command got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command kept waiting on a run that is over")
	}
}

type agentResult struct {
	code int
	out  string
}

// A session an agent leaves idle is closed by Autobot.
func TestSessionIdle(t *testing.T) {
	e := newEnv(t)
	e.ui.idleLimit = 50 * time.Millisecond
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	cn7, _, _ := e.st.AddMachine(ws.ID, "cn7", admin)
	se, err := e.ui.OpenSession(cn7, "", "", false, 0, time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the idle session to be closed", func() bool {
		_, _, _, err := e.st.RunState(se.RunID())
		run, _ := e.st.Run(se.RunID())
		return err == nil && !run.Active()
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Autobot's restart cannot leave a session open: its commands were in memory.
func TestLeftoverSessionsEnded(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	cn7, tok7, _ := e.st.AddMachine(ws.ID, "cn7", admin)
	plain, _, _ := e.st.AddMachine(ws.ID, "plain", admin)
	se, err := e.ui.OpenSession(cn7, "", "", false, 0, time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	e.req(t, "POST", tok7, "/connector/v1/heartbeat", idleStatus) // it is running now
	script, _ := e.st.QueueRun(plain, "true", "", "", false, 0, time.Minute, admin)

	if err := e.st.StopOrphanRuns(); err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.Run(se.RunID())
	if run.Active() || !strings.Contains(run.Summary, "restarted") {
		t.Fatalf("the session is %s %q", run.State, run.Summary)
	}
	if r, _ := e.st.Run(script.ID); !r.Active() {
		t.Fatal("an ordinary run was ended: its connector still reports it")
	}
	// Its connector, still holding the container, is told to remove it.
	busy := idleStatus
	busy.Run = se.RunID()
	_, body := e.req(t, "POST", tok7, "/connector/v1/heartbeat", busy)
	var r protocol.Reply
	json.Unmarshal([]byte(body), &r)
	if !r.Cancel {
		t.Fatalf("the connector was not told to stop: %s", body)
	}
}

// fakeConnector plays a machine's connector for a session: it takes the job, answers each command
// with exit 0 and the output "ran: <script>", and ends the run when told to stop.
func (e *env) fakeConnector(t *testing.T, token string) (stop func(), cmdLog func() []string) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var cmds []string
	var wg sync.WaitGroup
	run := int64(0)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			st := idleStatus
			mu.Lock()
			st.Run = run
			mu.Unlock()
			b, _ := json.Marshal(st)
			req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/connector/v1/heartbeat", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				var r protocol.Reply
				json.NewDecoder(resp.Body).Decode(&r)
				resp.Body.Close()
				if r.Job != nil {
					mu.Lock()
					run = r.Job.Run
					mu.Unlock()
					wg.Add(1)
					go func() {
						defer wg.Done()
						for ctx.Err() == nil {
							path := fmt.Sprintf("/connector/v1/runs/%d/commands", r.Job.Run)
							req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
							req.Header.Set("Authorization", "Bearer "+token)
							resp, err := http.DefaultClient.Do(req)
							if err != nil {
								return
							}
							var cs protocol.Commands
							json.NewDecoder(resp.Body).Decode(&cs)
							resp.Body.Close()
							if resp.StatusCode != 200 {
								return
							}
							for _, c := range cs.Commands {
								mu.Lock()
								cmds = append(cmds, c.Script)
								mu.Unlock()
								b, _ := json.Marshal(protocol.CommandResult{Output: "ran: " + c.Script})
								req, _ := http.NewRequest("POST", e.srv.URL+path+"/"+c.ID+"/result", bytes.NewReader(b))
								req.Header.Set("Authorization", "Bearer "+token)
								if resp, err := http.DefaultClient.Do(req); err == nil {
									resp.Body.Close()
								}
							}
						}
					}()
				}
				if r.Cancel {
					mu.Lock()
					id := run
					run = 0
					mu.Unlock()
					b, _ := json.Marshal(protocol.Result{Cancelled: true, ExitCode: 137})
					req, _ := http.NewRequest("POST", fmt.Sprintf("%s/connector/v1/runs/%d/result", e.srv.URL, id), bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer "+token)
					if resp, err := http.DefaultClient.Do(req); err == nil {
						resp.Body.Close()
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	stop = func() { cancel(); wg.Wait() }
	t.Cleanup(stop)
	return stop, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, cmds...) }
}

// An agent with a machine works in a session on it, and the session is closed when the agent ends.
func TestAgentSessionClosesWhenAgentEnds(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	cn7, tok7, _ := e.st.AddMachine(ws.ID, "cn7", derek)

	// The model runs one command, then finishes.
	var mu sync.Mutex
	var systems []string
	llm := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		systems = append(systems, req.Messages[0].Content)
		mu.Unlock()
		reply := `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"cargo test\"}"}}]}`
		if req.Messages[len(req.Messages)-1].Role == "tool" {
			reply = `{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"finish","arguments":"{\"status\":\"met\",\"report\":\"Tests ran.\"}"}}]}`
		}
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, reply)
	})
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, false)
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	a := workflow.DefaultAgent(workflow.PR)
	a.Model, a.Internet, a.Machines = workflow.ModelRef{Provider: p.ID, Model: "m"}, false, []int64{cn7.ID}
	raw, _ := json.Marshal(a)
	a.Verify = workflow.ModelRef{Provider: p.ID, Model: "not-lent"} // its start fails after the session is queued
	rawBad, _ := json.Marshal(a)
	g, _ := json.Marshal(workflow.Graph{Nodes: []workflow.Node{{ID: "ag", Type: workflow.Agent, Name: "Tester", Settings: raw},
		{ID: "bad", Type: workflow.Agent, Name: "Broken", Settings: rawBad}}, Links: []workflow.Link{}})
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(g), derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	tok := b.csrf()
	base := "/w/o/r/workflows/pr/nodes/ag/test"
	form := url.Values{"csrf": {tok}, "tag_pr": {"https://github.com/o/r/pull/7"}}

	// With no machine free, the form says so.
	if code, _, body := b.post(base, form); code != http.StatusBadRequest || !strings.Contains(body, "no machine is free") || !strings.Contains(body, "cn7 (never connected)") {
		t.Fatalf("no machine: %d\n%s", code, body)
	}
	if run, err := e.st.ActiveRun(cn7.ID); err == nil {
		t.Fatalf("a session was queued with no machine free: %+v", run)
	}

	_, cmdLog := e.fakeConnector(t, tok7)
	waitFor(t, "the connector to check in", func() bool { _, on := e.ui.live.get(cn7.ID); return on })
	// An agent that fails to start after its session was queued closes it.
	if code, _, body := b.post("/w/o/r/workflows/pr/nodes/bad/test", form); code != http.StatusBadRequest || !strings.Contains(body, "the check") {
		t.Fatalf("a broken agent: %d\n%s", code, body)
	}
	waitFor(t, "the session of the agent that failed to start to be closed", func() bool {
		runs, _ := e.st.Runs(cn7.ID, 5)
		return len(runs) == 1 && runs[0].Session && !runs[0].Active()
	})
	code, loc, body := b.post(base, form)
	if code != http.StatusSeeOther {
		t.Fatalf("start: %d\n%s", code, body)
	}
	var page string
	waitFor(t, "the agent to finish", func() bool {
		_, page = b.get(loc)
		return !strings.Contains(page, `/static/live.js`)
	})
	if !strings.Contains(page, "Goal met") || !strings.Contains(page, "[exit 0]") {
		t.Fatalf("run page:\n%s", page)
	}
	if cmds := cmdLog(); len(cmds) != 2 || cmds[1] != "cargo test" { // the readiness check, then the agent's
		t.Fatalf("commands %q", cmds)
	}
	mu.Lock()
	system := systems[0]
	mu.Unlock()
	if !strings.Contains(system, "a shell on the machine cn7") || !strings.Contains(system, "NVIDIA Test GPU") || !strings.Contains(system, "no internet") ||
		!strings.Contains(system, headSHA[:12]) {
		t.Fatalf("system prompt lacks the sandbox:\n%s", system)
	}
	// The session was closed: its run ended, and the machine is free again.
	waitFor(t, "the session to be closed", func() bool {
		runs, _ := e.st.Runs(cn7.ID, 5)
		return len(runs) == 2 && runs[0].Session && !runs[0].Active()
	})
}

func httptestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
