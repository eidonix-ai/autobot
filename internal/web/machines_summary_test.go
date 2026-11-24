package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

// {machines} summarises the workspace's machines as they are now, with people's notes.
func TestMachinesSummary(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	if got := e.ui.machinesSummary(ws); got != "This workspace has no test machines." {
		t.Fatalf("%q", got)
	}
	m, _, _ := e.st.AddMachine(ws.ID, "cn5", derek)
	mem := 98304.0
	e.ui.live.set(m.ID, protocol.Status{Arch: "x86_64", JobCPUs: 32, JobMemoryGiB: 256, JobScratchGiB: 64,
		GPUs:   []protocol.GPU{{Name: "RTX PRO 6000", MemTotalMiB: &mem}, {Name: "RTX PRO 6000", MemTotalMiB: &mem}},
		Models: []protocol.Model{{Name: "GLM-5.3", Bytes: 412 << 30}}})
	e.st.SetMachineNotes(m, "SM120, driver 580", derek)
	m2, _, _ := e.st.AddMachine(ws.ID, "cn7", derek)
	_ = m2
	got := e.ui.machinesSummary(ws)
	for _, want := range []string{"cn5: Ready", "2 × RTX PRO 6000, 96 GiB each · x86_64", "32 CPUs, 256 GiB RAM, 64 GiB scratch", "GLM-5.3 (412 GiB)", "Notes: SM120, driver 580",
		"cn7: Never connected", "has not reported its hardware yet"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
