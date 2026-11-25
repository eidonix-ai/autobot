package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCapturePatchScriptWithoutGit verifies that capturePatchScript produces a clean unified diff
// respecting .gitignore when /work/source was unpacked from a GitHub tarball (no .git directory)
// and the unmodified base tree lives at /input/source.
func TestCapturePatchScriptWithoutGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	inDir := filepath.Join(root, "input_source")
	workDir := filepath.Join(root, "work_source")
	for _, d := range []string{inDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".gitignore"), []byte("target/\n*.tmp\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "hello.txt"), []byte("a orbit's\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Modify an existing file, add a new tracked file, and add gitignored build artifacts.
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("an orbit's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "new_test.rs"), []byte("fn ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "target", "debug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "target", "debug", "big.o"), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	const wantSHA = "deadbeef1234567890abcdef1234567890abcdef"
	script := capturePatchScript(wantSHA)
	script = strings.ReplaceAll(script, "/input/source", inDir)
	script = strings.ReplaceAll(script, "/work/source", workDir)
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capture script failed: %v\n%s", err, out)
	}
	head, diff, ok := strings.Cut(string(out), patchBoundary+"\n")
	if !ok {
		t.Fatalf("missing patch boundary:\n%s", out)
	}
	if strings.TrimSpace(head) != wantSHA {
		t.Fatalf("head = %q, want %q", strings.TrimSpace(head), wantSHA)
	}
	for _, want := range []string{"diff --git a/hello.txt b/hello.txt", "-a orbit's", "+an orbit's", "diff --git a/new_test.rs b/new_test.rs", "+fn ok() {}"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "target/debug/big.o") {
		t.Errorf("diff included gitignored target/ artifact:\n%s", diff)
	}

	// Verify git apply applies that exact diff cleanly to a fresh non-git copy of inDir.
	applyDir := filepath.Join(root, "apply_copy")
	if err := os.MkdirAll(applyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gitignore", "hello.txt"} {
		b, _ := os.ReadFile(filepath.Join(inDir, name))
		os.WriteFile(filepath.Join(applyDir, name), b, 0o644)
	}
	patchFile := filepath.Join(root, "out.patch")
	os.WriteFile(patchFile, []byte(diff), 0o644)
	apply := exec.Command("git", "apply", "--whitespace=nowarn", patchFile)
	apply.Dir = applyDir
	if aout, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("git apply outside git repo failed: %v\n%s", err, aout)
	}
	gotHello, _ := os.ReadFile(filepath.Join(applyDir, "hello.txt"))
	gotNew, _ := os.ReadFile(filepath.Join(applyDir, "new_test.rs"))
	if string(gotHello) != "an orbit's\n" || string(gotNew) != "fn ok() {}\n" {
		t.Fatalf("applied files mismatch: hello=%q new=%q", gotHello, gotNew)
	}
}

func TestRestoreRunPatch(t *testing.T) {
	rs := &runState{
		builtIn: map[string]string{"lfa:title": "Model X"},
		tags: map[string]string{
			"master_plan":     "plan text",
			"run:patch":       "diff --git a/a b/a\n",
			"run:base_commit": "abc123",
		},
	}
	restoreRunPatch(rs)
	if rs.builtIn["run:patch"] != "diff --git a/a b/a\n" || rs.builtIn["run:base_commit"] != "abc123" {
		t.Fatalf("builtIn not restored: %+v", rs.builtIn)
	}
	if _, ok := rs.tags["run:patch"]; ok {
		t.Fatalf("run:patch still in tags")
	}
	if rs.tags["master_plan"] != "plan text" {
		t.Fatalf("unrelated tag altered: %+v", rs.tags)
	}
}
