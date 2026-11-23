package decisions

import (
	"os"
	"testing"

	"github.com/eidonix-ai/autobot/internal/netguard"
)

// The tests' stand-in servers listen on this machine, which netguard refuses unless trusted, as an
// admin trusts a local inference server.
func TestMain(m *testing.M) {
	netguard.SetTrusted([]string{"127.0.0.1"})
	os.Exit(m.Run())
}
