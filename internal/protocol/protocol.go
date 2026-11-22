// Package protocol is what Autobot and the connector on a donated machine send each other. The
// connector makes every request, over HTTPS, with its machine token as a bearer token:
//
//	POST /connector/v1/heartbeat         Status   -> Reply   every Heartbeat
//	GET  /connector/v1/runs/{id}/source           -> a private run's code, a GitHub tarball (gzipped tar)
//	POST /connector/v1/runs/{id}/output  raw text -> Reply   while a run goes, at most OutputChunk bytes
//	POST /connector/v1/runs/{id}/result  Result   -> Reply   once, when the run ends
//
// The connector downloads a public repository straight from GitHub, over the donor's own
// connection. Only a private repository's code comes through Autobot, which fetches it with the
// workspace's GitHub identity, so no GitHub token ever reaches a donated machine.
//
// Autobot keeps the state of every run, so either side can restart: a connector that comes back
// without the run Autobot expects makes Autobot mark the run lost.
package protocol

import (
	"regexp"
	"time"
)

const (
	// Heartbeat is how often the connector reports.
	Heartbeat = 2 * time.Second
	// OutputChunk is the most output one request carries.
	OutputChunk = 64 << 10
	// MaxOutput is the most output Autobot keeps for one run.
	MaxOutput = 8 << 20
	// MaxRun is the longest a run may last.
	MaxRun = 2 * time.Hour
)

// Status is the connector's report on its machine.
type Status struct {
	Version         string  `json:"version"`
	Arch            string  `json:"arch"`
	Image           string  `json:"image"`           // the work image jobs run in, by id
	JobCPUs         int     `json:"job_cpus"`        // what one job gets
	JobMemoryGiB    int     `json:"job_memory_gib"`  // RAM, including the scratch space
	JobScratchGiB   int     `json:"job_scratch_gib"` // the in-memory /work folder
	MemTotalMiB     int     `json:"mem_total_mib"`   // the host's
	MemAvailableMiB int     `json:"mem_available_mib"`
	GPUs            []GPU   `json:"gpus"`
	Models          []Model `json:"models"`  // what jobs find under /models
	Run             int64   `json:"run"`     // the run in progress, or 0
	Problem         string  `json:"problem"` // why the machine takes no new run now, or ""
}

// GPU is one GPU's readings. A nil value means the GPU does not report it.
type GPU struct {
	Index       int      `json:"index"`
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	TempC       *float64 `json:"temp_c"`
	PowerW      *float64 `json:"power_w"`
	PowerLimitW *float64 `json:"power_limit_w"`
	MemUsedMiB  *float64 `json:"mem_used_mib"`
	MemTotalMiB *float64 `json:"mem_total_mib"`
	UtilPct     *float64 `json:"util_pct"`
}

// Model is one model in the machine's models folder.
type Model struct {
	Name  string `json:"name"`  // org/name for a Hugging Face cache entry, else the path in /models
	Path  string `json:"path"`  // where a job finds it, under /models
	Bytes int64  `json:"bytes"` // its size on disk
}

// Job is a test run for the connector to start.
type Job struct {
	Run        int64  `json:"run"`
	Repo       string `json:"repo"`    // GitHub owner/repo, when Commit is set
	Commit     string `json:"commit"`  // copied to /work/source before the script; "" for none
	Private    bool   `json:"private"` // fetch the code from Autobot, not from GitHub
	Script     string `json:"script"`  // bash, run with -e and -x
	GPUs       int    `json:"gpus"`
	TimeoutSec int    `json:"timeout_sec"`
}

// Reply is Autobot's answer to any request.
type Reply struct {
	Job    *Job `json:"job,omitempty"`    // a run to start (heartbeat only, when the machine is idle)
	Cancel bool `json:"cancel,omitempty"` // stop the run in progress
}

// Result is how a run ended.
type Result struct {
	ExitCode  int    `json:"exit_code"` // the script's, or -1 if it did not run
	TimedOut  bool   `json:"timed_out"`
	Cancelled bool   `json:"cancelled"`
	OOMKilled bool   `json:"oom_killed"`
	Error     string `json:"error"` // what went wrong outside the script, or ""
}

var (
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Valid checks a job before the connector acts on it.
func (j Job) Valid() bool {
	if j.Run <= 0 || j.Script == "" || len(j.Script) > 64<<10 || j.GPUs < 0 || j.GPUs > 8 ||
		j.TimeoutSec <= 0 || j.TimeoutSec > int(MaxRun/time.Second) {
		return false
	}
	if j.Commit == "" {
		return j.Repo == ""
	}
	return commitRE.MatchString(j.Commit) && repoRE.MatchString(j.Repo)
}
