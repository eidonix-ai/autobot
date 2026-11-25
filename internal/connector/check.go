package connector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// checkScript runs inside a job container and fails if the container is not isolated as it must be.
const checkScript = `fail=0
ok()  { echo "ok    $1"; }
bad() { echo "FAIL  $1"; fail=1; }
[ "$(id -u):$(id -g)" = 0:0 ] && ok "runs as uid 0 inside container (for package installation)" || bad "runs as $(id -u):$(id -g)"
grep -q '^CapEff:[[:space:]]*00000000000000eb$' /proc/self/status && ok "has only minimal package-install capabilities" || bad "unexpected capabilities: $(grep CapEff /proc/self/status)"
grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status && ok "cannot gain privileges" || bad "can gain privileges"
touch /root-write-test && rm -f /root-write-test && ok "ephemeral root filesystem is writable" || bad "root filesystem is not writable"
touch /work/write-test && ok "can write /work" || bad "cannot write /work"
ifaces=$(ls /sys/class/net | grep -vx lo | tr '\n' ' ')
[ -n "$ifaces" ] && ok "has bridge network interface: $ifaces" || bad "has no network interface besides loopback"
[ "$(cat /sys/fs/cgroup/memory.max)" != max ] && ok "memory is capped at $(( $(cat /sys/fs/cgroup/memory.max) >> 30 )) GiB" || bad "memory is not capped"
[ "$(cat /sys/fs/cgroup/memory.swap.max 2>/dev/null || echo 0)" = 0 ] && ok "has no swap" || bad "can swap"
[ "$(cat /sys/fs/cgroup/pids.max)" != max ] && ok "processes are capped" || bad "processes are not capped"
if [ -d /models ]; then
  touch /models/write-test 2>/dev/null && bad "can write /models" || ok "/models is read-only ($(ls /models | wc -l) entries)"
fi
if [ -n "$EXPECT_GPUS" ]; then
  n=$(nvidia-smi -L 2>/dev/null | grep -c '^GPU ')
  [ "$n" = "$EXPECT_GPUS" ] && ok "sees exactly its $n GPU(s): $(nvidia-smi -L | head -1)" || bad "sees $n GPUs, expected $EXPECT_GPUS"
fi
exit $fail`

// Check starts a job container with the settings every run gets and confirms from inside that it
// is isolated: minimal capabilities, no privilege gain, ephemeral writable root with bridge
// networking, capped resources, read-only models, and only the GPU it was given. It writes the
// report to w.
func Check(ctx context.Context, cfg Config, w io.Writer) error {
	d := newDocker(cfg.Socket)
	image, err := d.imageID(ctx, cfg.Image)
	if err != nil {
		return fmt.Errorf("work image %s: %w", cfg.Image, err)
	}
	fmt.Fprintf(w, "Work image %s (%s)\n", cfg.Image, image)
	s := spec{Name: fmt.Sprintf("autobot-check-%d", time.Now().Unix()), Image: image, Run: 0,
		CPUs: cfg.CPUs, MemoryGiB: cfg.MemoryGiB, ScratchGB: cfg.ScratchGiB}
	expect := ""
	if gpus, err := readGPUs(ctx); err == nil && len(gpus) > 0 {
		s.GPUs, expect = []string{"nvidia.com/gpu=" + gpus[0].UUID}, "1"
		fmt.Fprintf(w, "Giving the job GPU %d (%s) of %d\n", gpus[0].Index, gpus[0].Name, len(gpus))
	} else {
		fmt.Fprintf(w, "No GPUs found (%v); checking without one\n", err)
	}
	if cfg.ModelsDir != "" {
		if _, err := os.Stat(cfg.ModelsDir); err != nil {
			return fmt.Errorf("MODELS_DIR: %w", err)
		}
		s.Binds = append(s.Binds, cfg.ModelsDir+":/models:ro")
		fmt.Fprintf(w, "Models: %d found in %s\n", len(scanModels(cfg.ModelsDir)), cfg.ModelsDir)
	}
	s.Cmd = []string{"bash", "-c", "EXPECT_GPUS=" + expect + "\n" + checkScript}

	bg := context.Background()
	id, err := d.create(bg, s)
	if err != nil {
		return err
	}
	defer d.remove(bg, id)
	if err := d.start(bg, id); err != nil {
		return err
	}
	var out bytes.Buffer
	d.logs(ctx, id, io.MultiWriter(w, &out))
	code, err := d.wait(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 || strings.Contains(out.String(), "FAIL") {
		return fmt.Errorf("the job container is not isolated as required (see FAIL above)")
	}
	fmt.Fprintln(w, "The job container is isolated as required.")
	return nil
}
