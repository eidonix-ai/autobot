package web

import (
	"context"
	"testing"
)

// An agent can read the whole charter, which the orientation packet only quotes from.
func TestCharterTool(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	tool := e.ui.charterTool(ws)
	c, _ := e.st.Charter(ws.ID, 0)
	if _, err := e.st.SaveCharter(ws.ID, c.Rev, "# Vision\nFast inference for everyone.", derek); err != nil {
		t.Fatal(err)
	}
	if out, err := tool.Run(context.Background(), nil); err != nil || out != "# Vision\nFast inference for everyone." {
		t.Fatalf("charter: %q %v", out, err)
	}
	if !contains(readOnlyTools, "read_charter") {
		t.Fatal("the writing session and sub-agents cannot read the charter")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
