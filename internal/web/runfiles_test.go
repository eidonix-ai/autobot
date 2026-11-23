package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A test run's page shows its whole output from the run's file, though its row keeps only the last
// 4 KB; after 30 days the output is deleted and the page says so.
func TestRunPageReadsTheFile(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	m, _, err := e.st.AddMachine(ws.ID, "cn1", derek)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.QueueRun(m, "make test", "", "", false, 0, time.Minute, derek)
	e.st.StartRun(run.ID)
	var out strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&out, "line %04d of the test output\n", i)
	}
	e.st.AppendOutput(run.ID, out.String(), 1<<20)
	e.st.FinishRun(run.ID, "passed", "Passed.")
	b := e.signIn("derek")
	path := fmt.Sprintf("/w/o/r/runs/%d", run.ID)
	if code, body := b.get(path); code != 200 || !strings.Contains(body, "line 0000 of") || !strings.Contains(body, "line 0599 of") {
		t.Fatalf("%d: the page lacks the start of the output", code)
	}
	if got, _ := e.st.Run(run.ID); len(got.Output) > 4096 {
		t.Fatalf("the row holds %d bytes", len(got.Output))
	}
	e.st.PruneRuns(time.Now().Add(31 * 24 * time.Hour))
	if _, body := b.get(path); strings.Contains(body, "line 0599") || !strings.Contains(body, "deleted after 30 days") {
		t.Fatalf("after 30 days:\n%s", body)
	}
}
