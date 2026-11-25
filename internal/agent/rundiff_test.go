package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunDiffToolStatAndFilter(t *testing.T) {
	patch := `diff --git a/helios/src/frontend/mod.rs b/helios/src/frontend/mod.rs
--- a/helios/src/frontend/mod.rs
+++ b/helios/src/frontend/mod.rs
@@ -1,2 +1,3 @@
 fn foo() {}
+fn bar() {}
-fn old() {}
diff --git a/ignition/preparation/frontend.py b/ignition/preparation/frontend.py
--- a/ignition/preparation/frontend.py
+++ b/ignition/preparation/frontend.py
@@ -1 +1,3 @@
 x = 1
+y = 2
+z = 3
`
	tool := RunDiffTool(patch)
	if tool.Spec.Name != "run_diff" {
		t.Fatalf("tool name = %q, want run_diff", tool.Spec.Name)
	}
	statOut, err := tool.Run(context.Background(), json.RawMessage(`{"stat":true}`))
	if err != nil {
		t.Fatalf("stat err: %v", err)
	}
	for _, want := range []string{"helios/src/frontend/mod.rs (+1 / -1)", "ignition/preparation/frontend.py (+2 / -0)", "Total: 2 files, +3 / -1 lines"} {
		if !strings.Contains(statOut, want) {
			t.Fatalf("stat missing %q in:\n%s", want, statOut)
		}
	}
	filtOut, err := tool.Run(context.Background(), json.RawMessage(`{"path":"helios/"}`))
	if err != nil {
		t.Fatalf("path filter err: %v", err)
	}
	if !strings.Contains(filtOut, "fn bar()") || strings.Contains(filtOut, "ignition/preparation/frontend.py") {
		t.Fatalf("unexpected filtered output:\n%s", filtOut)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"path":"nonexistent/"}`)); err == nil {
		t.Fatalf("expected error for unmatched path")
	}
}
