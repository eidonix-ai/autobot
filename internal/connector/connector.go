// Package connector is the program on a donated machine. It checks in with Autobot every two
// seconds, and when Autobot has a test run for it, it downloads the code, runs the run's script in
// a fresh job container that has no network and cannot change the machine, streams the output
// back, and removes the container.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

// Version is the connector's version, reported to Autobot.
var Version = "dev"

// Config is the connector's settings, from its environment.
type Config struct {
	URL        string // Autobot, such as https://autobot.eidonix.com
	Token      string // the machine token from Autobot
	Image      string // the work image jobs run in
	DataDir    string // a host folder, mounted at the same path in the connector's container
	ModelsDir  string // a host folder of models, mounted read-only at the same path; "" for none
	CPUs       int    // per job
	MemoryGiB  int    // per job, scratch included
	ScratchGiB int    // per job: the in-memory /work folder
	MaxTempC   float64
	Socket     string // Docker's API socket
}

// ConfigFromEnv reads the settings. Job limits default to half the machine.
func ConfigFromEnv() (Config, error) {
	total, _ := readMemory()
	c := Config{
		URL:        env("AUTOBOT_URL", "https://autobot.eidonix.com"),
		Token:      os.Getenv("CONNECTOR_TOKEN"),
		Image:      os.Getenv("WORK_IMAGE"),
		DataDir:    env("DATA_DIR", "/var/lib/autobot-connector"),
		ModelsDir:  os.Getenv("MODELS_DIR"),
		CPUs:       envInt("JOB_CPUS", max(1, runtime.NumCPU()/2)),
		MemoryGiB:  envInt("JOB_MEMORY_GIB", max(1, total/2/1024)),
		ScratchGiB: envInt("JOB_SCRATCH_GIB", 16),
		MaxTempC:   float64(envInt("MAX_GPU_TEMP", 83)),
		Socket:     env("DOCKER_SOCKET", "/var/run/docker.sock"),
	}
	switch {
	case !strings.HasPrefix(c.Token, "abm_"):
		return c, errors.New("CONNECTOR_TOKEN is missing; it is shown once when the machine is added in Autobot")
	case c.Image == "":
		return c, errors.New("WORK_IMAGE is missing: name the image jobs run in")
	case c.ScratchGiB >= c.MemoryGiB:
		return c, errors.New("JOB_SCRATCH_GIB must be less than JOB_MEMORY_GIB: the scratch folder lives in the job's memory")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// Connector runs one test run at a time.
type Connector struct {
	cfg     Config
	docker  *docker
	http    *http.Client
	imageID string
	github  string // GitHub's download host, for public repositories

	mu      sync.Mutex
	img     *imageInfo // what the work image has, found on first use
	run     *activeRun
	models  []protocol.Model
	problem string
}

type activeRun struct {
	id     int64
	cancel context.CancelFunc
	why    string // set when the connector stops the run itself
}

// New returns a connector.
func New(cfg Config) *Connector {
	return &Connector{cfg: cfg, docker: newDocker(cfg.Socket), http: &http.Client{Timeout: 30 * time.Second},
		github: "https://codeload.github.com"}
}

// Run checks in with Autobot until ctx ends.
func (c *Connector) Run(ctx context.Context) error {
	id, err := c.docker.imageID(ctx, c.cfg.Image)
	if err != nil {
		return fmt.Errorf("work image %s: %w", c.cfg.Image, err)
	}
	c.imageID = id
	c.cleanUp(ctx)
	go c.watchModels(ctx)
	log.Printf("autobot-connector %s: checking in with %s; jobs run in %s", Version, c.cfg.URL, c.cfg.Image)
	t := time.NewTicker(protocol.Heartbeat)
	defer t.Stop()
	failing := false
	for {
		var reply protocol.Reply
		status := c.status(ctx)
		if err := c.post(ctx, "/connector/v1/heartbeat", status, &reply); err != nil {
			if !failing {
				log.Printf("checking in: %v", err)
			}
			failing = true
		} else {
			if failing {
				log.Printf("checking in again")
			}
			failing = false
			if reply.Cancel {
				c.stop("cancelled")
			}
			if reply.Job != nil && status.Run == 0 && status.Problem == "" {
				c.start(ctx, *reply.Job)
			}
		}
		select {
		case <-ctx.Done():
			c.stop("the connector stopped")
			return nil
		case <-t.C:
		}
	}
}

// cleanUp removes job containers and downloaded code left by an earlier connector.
func (c *Connector) cleanUp(ctx context.Context) {
	ids, err := c.docker.leftovers(ctx)
	if err != nil {
		log.Printf("listing leftover job containers: %v", err)
	}
	for _, id := range ids {
		log.Printf("removing leftover job container %.12s", id)
		c.docker.remove(ctx, id)
	}
	os.RemoveAll(filepath.Join(c.cfg.DataDir, "runs"))
	os.RemoveAll(filepath.Join(c.cfg.DataDir, "cache", "tmp"))
}

// arch is this machine's CPU as Python wheel names spell it.
func (c *Connector) arch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// probeScript lists the work image's Python version and installed packages.
const probeScript = `import sys, re, importlib.metadata as m
print(f"cp{sys.version_info.major}{sys.version_info.minor}")
for d in m.distributions():
    n = d.metadata.get("Name")
    if n:
        print(re.sub(r"[-_.]+", "-", n).lower() + "==" + d.version)`

// image finds what the work image already has, once, by running its Python in a job container.
func (c *Connector) image(ctx context.Context) (imageInfo, error) {
	c.mu.Lock()
	if c.img != nil {
		defer c.mu.Unlock()
		return *c.img, nil
	}
	c.mu.Unlock()
	s := spec{Name: fmt.Sprintf("autobot-probe-%d", time.Now().UnixNano()), Image: c.imageID,
		Cmd: []string{"python3", "-c", probeScript}, CPUs: 1, MemoryGiB: 2, ScratchGB: 1}
	bg := context.Background()
	id, err := c.docker.create(bg, s)
	if err != nil {
		return imageInfo{}, err
	}
	defer c.docker.remove(bg, id)
	if err := c.docker.start(bg, id); err != nil {
		return imageInfo{}, err
	}
	var out bytes.Buffer
	c.docker.logs(ctx, id, &out)
	if code, err := c.docker.wait(ctx, id); err != nil || code != 0 {
		return imageInfo{}, fmt.Errorf("python3 in the work image exited %d: %v", code, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	info := imageInfo{pythonTag: strings.TrimSpace(lines[0]), installed: map[string]bool{}}
	for _, l := range lines[1:] {
		info.installed[strings.TrimSpace(l)] = true
	}
	c.mu.Lock()
	c.img = &info
	c.mu.Unlock()
	return info, nil
}

func (c *Connector) watchModels(ctx context.Context) {
	for {
		models := scanModels(c.cfg.ModelsDir)
		c.mu.Lock()
		c.models = models
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (c *Connector) status(ctx context.Context) protocol.Status {
	total, avail := readMemory()
	gpus, gpuErr := readGPUs(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := protocol.Status{Version: Version, Arch: runtime.GOARCH, Image: c.imageID, JobCPUs: c.cfg.CPUs,
		JobMemoryGiB: c.cfg.MemoryGiB, JobScratchGiB: c.cfg.ScratchGiB, MemTotalMiB: total, MemAvailableMiB: avail,
		GPUs: gpus, Models: c.models}
	switch {
	case gpuErr != nil:
		s.Problem = "cannot read the GPUs: " + gpuErr.Error()
	case total > 0 && avail*10 < total:
		s.Problem = fmt.Sprintf("host memory is low: %d of %d MiB available", avail, total)
	}
	for _, g := range gpus {
		if g.TempC != nil && *g.TempC >= c.cfg.MaxTempC {
			s.Problem = fmt.Sprintf("GPU %d is at %.0f °C", g.Index, *g.TempC)
			if c.run != nil && c.run.why == "" {
				c.run.why = "stopped: " + s.Problem
				c.run.cancel()
			}
		}
	}
	if c.run != nil {
		s.Run = c.run.id
	}
	return s
}

func (c *Connector) post(ctx context.Context, path string, body, out any) error {
	var rd io.Reader
	ctype := "application/json"
	if b, ok := body.([]byte); ok {
		rd, ctype = bytes.NewReader(b), "text/plain; charset=utf-8"
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(c.cfg.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", "autobot-connector/"+Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// stop ends the run in progress, if any, giving why as the reason.
func (c *Connector) stop(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil {
		if c.run.why == "" {
			c.run.why = why
		}
		c.run.cancel()
	}
}

// start begins a run. It returns at once; the run reports its own result.
func (c *Connector) start(ctx context.Context, job protocol.Job) {
	if !job.Valid() {
		log.Printf("refusing an invalid job for run %d", job.Run)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &activeRun{id: job.Run, cancel: cancel}
	c.mu.Lock()
	c.run = r
	c.mu.Unlock()
	go func() {
		defer cancel()
		res := c.execute(ctx, job, r)
		c.mu.Lock()
		c.run = nil
		c.mu.Unlock()
		c.report(job.Run, res)
	}()
}

// report sends a run's result, retrying for ten minutes in case Autobot is restarting.
func (c *Connector) report(run int64, res protocol.Result) {
	path := fmt.Sprintf("/connector/v1/runs/%d/result", run)
	for wait, deadline := time.Second, time.Now().Add(10*time.Minute); ; wait = min(2*wait, 30*time.Second) {
		err := c.post(context.Background(), path, res, nil)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("could not report run %d: %v", run, err)
			return
		}
		time.Sleep(wait)
	}
}

// prelude runs before the job's own script. It copies the code into the job's scratch folder,
// points Cargo at the fetched crates (keeping the image's git dependencies), installs the fetched
// Python wheels ahead of the image's packages, then runs the script with -e (stop at the first
// failure) and -x (show each command), its commands and output in one stream so they stay in order.
//
// Every variable it sets up is also written, as exports, to /work/.autobot/env, which appears only
// when the set-up is done. A session's commands run in new processes, so each sources that file to
// see what a script would; the connector waits for it before taking the first command.
const prelude = `set -e
mkdir -p "$TMPDIR" /work/.autobot
: > /work/.autobot/env.tmp
setenv() { export "$1=$2"; printf 'export %s=%q\n' "$1" "$2" >> /work/.autobot/env.tmp; }
if [ -d /deps/crates ]; then
  image_cargo="${CARGO_HOME:-$HOME/.cargo}"
  setenv CARGO_HOME /work/cargo
  mkdir -p "$CARGO_HOME"
  if [ -d "$image_cargo/git" ]; then cp -a "$image_cargo/git" "$CARGO_HOME/"; fi
  printf '[source.crates-io]\nreplace-with = "autobot-deps"\n\n[source.autobot-deps]\ndirectory = "/deps/crates"\n\n[net]\noffline = true\n' > "$CARGO_HOME/config.toml"
fi
if [ -s /deps/wheels/requirements.txt ]; then
  echo "Installing $(wc -l < /deps/wheels/requirements.txt) Python packages the work image lacks"
  uv pip install --quiet --offline --no-index --find-links /deps/wheels --no-deps --target /work/python -r /deps/wheels/requirements.txt
  setenv PYTHONPATH "/work/python${PYTHONPATH:+:$PYTHONPATH}"
fi
if [ -d /input/source ]; then cp -a /input/source /work/source; cd /work/source; fi
mv /work/.autobot/env.tmp /work/.autobot/env
printf '%s\n' "$1" > "$TMPDIR/run.sh"
exec bash -ex "$TMPDIR/run.sh" 2>&1`

// sessionScript is what a session's container runs after the prelude: nothing, until it is killed.
const sessionScript = `exec sleep infinity`

func (c *Connector) execute(ctx context.Context, job protocol.Job, r *activeRun) protocol.Result {
	res := protocol.Result{ExitCode: -1}
	out := newOutput(c, job.Run, r)
	defer out.close()
	dir := filepath.Join(c.cfg.DataDir, "runs", strconv.FormatInt(job.Run, 10))
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Error = err.Error()
		return res
	}

	script := job.Script
	if job.Session {
		script = sessionScript
	}
	s := spec{Name: fmt.Sprintf("autobot-run-%d", job.Run), Image: c.imageID, Cmd: []string{"bash", "-c", prelude, "bash", script},
		Run: job.Run, CPUs: c.cfg.CPUs, MemoryGiB: c.cfg.MemoryGiB, ScratchGB: c.cfg.ScratchGiB}
	if c.cfg.ModelsDir != "" {
		s.Binds = append(s.Binds, c.cfg.ModelsDir+":/models:ro")
	}
	if job.Session {
		// Files for the agent arrive here, written by this connector, and are copied in by a command.
		if err := os.MkdirAll(inboxDir(dir), 0o755); err != nil {
			res.Error = err.Error()
			return res
		}
		s.Binds = append(s.Binds, inboxDir(dir)+":/inbox:ro")
	}
	gpus, _ := readGPUs(ctx)
	if job.GPUs > len(gpus) {
		res.Error = fmt.Sprintf("the run asks for %d GPUs; this machine has %d", job.GPUs, len(gpus))
		return res
	}
	for _, g := range gpus[:job.GPUs] {
		s.GPUs = append(s.GPUs, "nvidia.com/gpu="+g.UUID)
	}

	timeout := time.Duration(job.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if job.Commit != "" {
		from := "GitHub"
		if job.Private {
			from = "Autobot (private repository)"
		}
		fmt.Fprintf(out, "Downloading %s at %s from %s\n", job.Repo, job.Commit, from)
		if err := c.fetchSource(ctx, job, filepath.Join(dir, "source")); err != nil {
			res.Error = err.Error()
			return c.ended(ctx, r, res)
		}
		s.Binds = append(s.Binds, filepath.Join(dir, "source")+":/input/source:ro")
		binds, err := c.prepareDeps(ctx, filepath.Join(dir, "source"), dir, out)
		if err != nil {
			res.Error = "dependencies: " + err.Error()
			return c.ended(ctx, r, res)
		}
		s.Binds = append(s.Binds, binds...)
	}

	// The container outlives a cancelled ctx just long enough to be killed and removed.
	bg := context.Background()
	id, err := c.docker.create(bg, s)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer c.docker.remove(bg, id)
	if err := c.docker.start(bg, id); err != nil {
		res.Error = err.Error()
		return res
	}
	logsDone := make(chan struct{})
	go func() { c.docker.logs(bg, id, out); close(logsDone) }()
	exited := make(chan int, 1)
	go func() {
		code, err := c.docker.wait(bg, id)
		if err != nil {
			code = -1
		}
		exited <- code
	}()
	if job.Session {
		// The commands stop with the container: when it exits, or when the run is over.
		sctx, stopSession := context.WithCancel(ctx)
		defer stopSession()
		go c.serveSession(sctx, job.Run, id, dir, out)
	}
	select {
	case res.ExitCode = <-exited:
	case <-ctx.Done():
		c.docker.kill(bg, id)
		res.ExitCode = <-exited
		res = c.ended(ctx, r, res)
	}
	<-logsDone
	res.OOMKilled = c.docker.oomKilled(bg, id)
	return res
}

// ended explains why ctx ended a run: its time limit, or a stop.
func (c *Connector) ended(ctx context.Context, r *activeRun, res protocol.Result) protocol.Result {
	if ctx.Err() == nil {
		return res
	}
	c.mu.Lock()
	why := r.why
	c.mu.Unlock()
	switch {
	case why == "cancelled":
		res.Cancelled = true
	case why != "":
		res.Error = why
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
	}
	return res
}

// output collects a run's output and sends it to Autobot about once a second.
type output struct {
	c    *Connector
	run  *activeRun
	path string
	mu   sync.Mutex
	buf  []byte
	sent int
	done chan struct{}
	quit chan struct{}
}

func newOutput(c *Connector, id int64, r *activeRun) *output {
	o := &output{c: c, run: r, path: fmt.Sprintf("/connector/v1/runs/%d/output", id), done: make(chan struct{}), quit: make(chan struct{})}
	go o.loop()
	return o
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sent+len(o.buf) < protocol.MaxOutput {
		o.buf = append(o.buf, p...)
	}
	return len(p), nil
}

func (o *output) loop() {
	defer close(o.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-o.quit:
			o.flush()
			return
		case <-t.C:
			o.flush()
		}
	}
}

func (o *output) flush() {
	for {
		o.mu.Lock()
		n := min(len(o.buf), protocol.OutputChunk)
		chunk := append([]byte(nil), o.buf[:n]...)
		o.mu.Unlock()
		if n == 0 {
			return
		}
		var reply protocol.Reply
		if err := o.c.post(context.Background(), o.path, chunk, &reply); err != nil {
			return // keep it, and try again next time
		}
		o.mu.Lock()
		o.buf, o.sent = o.buf[n:], o.sent+n
		o.mu.Unlock()
		if reply.Cancel {
			o.c.stop("cancelled")
		}
	}
}

func (o *output) close() {
	close(o.quit)
	<-o.done
}
