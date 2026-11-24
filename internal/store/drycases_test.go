package store

import (
	"errors"
	"testing"
)

// A dry run's case file is its own: no lookup by the item's number, no listing of real cases and no
// count of them ever has it, and the item's real case is a different file. [M]
func TestDryCaseIsNeverTheItemsCase(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	run, err := st.StartWorkflowRun(WorkflowRun{WorkspaceID: ws.ID, Kind: "pr", EventNode: "ev", Item: 7, DryRun: true, Graph: "{}"}, nil, admin)
	if err != nil {
		t.Fatal(err)
	}
	// With no real case at all.
	dry, created, err := st.OpenDryCase(ws, run, 7, "pr", "Retry uploads", "Dry")
	if err != nil || !created || dry.DryRunOf != run || dry.Number != 7 || dry.Title != "Retry uploads (dry run)" || dry.Label() != "PR #7" {
		t.Fatalf("%+v %v %v", dry, created, err)
	}
	if again, created, _ := st.OpenDryCase(ws, run, 7, "pr", "Retry uploads", "x"); created || again.UUID != dry.UUID {
		t.Fatal("a second dry case opened for the same run")
	}
	if _, err := st.CaseByNumber(ws.ID, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the item's case was found: %v", err)
	}
	if got, err := st.DryCase(run); err != nil || got.UUID != dry.UUID {
		t.Fatalf("the run's case: %v", err)
	}
	// The item's real case opens beside it, as a different file.
	real, created, err := st.OpenCase(ws, 7, "pr", "Retry uploads", "Real")
	if err != nil || !created || real.UUID == dry.UUID || real.DryRunOf != 0 {
		t.Fatalf("%+v %v %v", real, created, err)
	}
	if got, _ := st.CaseByNumber(ws.ID, 7); got.UUID != real.UUID {
		t.Fatal("the lookup by number found the dry run's case")
	}
	if list, _ := st.Cases(ws.ID, 10); len(list) != 1 || list[0].UUID != real.UUID {
		t.Fatalf("Cases: %+v", list)
	}
	if list, _ := st.FindCases(ws.ID, "7", 10); len(list) != 1 || list[0].DryRunOf != 0 {
		t.Fatalf("FindCases: %+v", list)
	}
	if list, _ := st.FindDryCases(ws.ID, "#7", 10); len(list) != 1 || list[0].DryRunOf != run || list[0].Number != 7 {
		t.Fatalf("FindDryCases: %+v", list)
	}
	if n, _ := st.CaseCount(ws.ID); n != 1 {
		t.Fatalf("CaseCount %d", n)
	}
	// A second dry run on the same item has its own.
	run2, _ := st.StartWorkflowRun(WorkflowRun{WorkspaceID: ws.ID, Kind: "pr", EventNode: "ev", Item: 7, DryRun: true, Graph: "{}"}, nil, admin)
	if d2, _, err := st.OpenDryCase(ws, run2, 7, "pr", "Retry uploads", "Dry"); err != nil || d2.UUID == dry.UUID {
		t.Fatalf("%+v %v", d2, err)
	}
}
