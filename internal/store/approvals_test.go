package store

import (
	"testing"
	"time"
)

// The question is kept with the details; a request stored before the column existed reads with
// none, and a second open returns the first.
func TestApprovalQuestion(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	run, _ := st.StartWorkflowRun(WorkflowRun{WorkspaceID: ws.ID, Kind: "pr", EventNode: "ev", Item: 7, Graph: "{}"}, []string{"ev", "ap", "old"}, admin)
	a, err := st.OpenApproval(run, "ap", "Found a typo fix.", "Skip the review?", time.Now().Add(time.Hour))
	if err != nil || a.Request != "Found a typo fix." || a.Question != "Skip the review?" {
		t.Fatalf("%+v %v", a, err)
	}
	if again, _ := st.OpenApproval(run, "ap", "Other.", "Other?", time.Now().Add(time.Hour)); again.ID != a.ID || again.Question != "Skip the review?" {
		t.Fatalf("a second open replaced the first: %+v", again)
	}
	if got, _ := st.ApprovalFor(run, "ap"); got.Question != "Skip the review?" {
		t.Fatalf("%+v", got)
	}
	// A row from before the column has the default.
	if _, err := st.db.Exec(`INSERT INTO approvals (run_id, node_id, request, expires_at) VALUES (?, 'old', 'Run it?', ?)`, run, time.Now().Unix()+60); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ApprovalFor(run, "old"); got.Request != "Run it?" || got.Question != "" {
		t.Fatalf("%+v", got)
	}
}
