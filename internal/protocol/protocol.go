// Package protocol is what Autobot and the connector on a donated machine send each other. The
// connector makes every request, over HTTPS, with its machine token as a bearer token:
//
//	POST /connector/v1/heartbeat         Status   -> Reply   every Heartbeat
//	GET  /connector/v1/runs/{id}/source           -> a private run's code, a GitHub tarball (gzipped tar)
//	POST /connector/v1/runs/{id}/output  raw text -> Reply   while a run goes, at most OutputChunk bytes
//	POST /connector/v1/runs/{id}/result  Result   -> Reply   once, when the run ends
//
// A session is a run that has no script: its container stays open for an agent, and the connector
// asks Autobot for the agent's commands while it lasts:
//
//	GET  /connector/v1/runs/{id}/commands                 -> Commands  held up to PollWait until some are queued
//	POST /connector/v1/runs/{id}/commands/{cid}/result  CommandResult -> Reply
//
// Both answer 404 unless the run is this machine's active session.
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
	"strings"
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
	// PollWait is how long Autobot holds a request for commands when it has none.
	PollWait = 25 * time.Second
	// MaxFile is the largest file a command may put into a session.
	MaxFile = 8 << 20
	// MaxCommandOutput is the most output one command returns: its beginning and its end.
	MaxCommandOutput = 1 << 20
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
	Script     string `json:"script"`  // bash, run with -e and -x; empty for a session
	Session    bool   `json:"session"` // keep the container open and take commands, instead of running a script
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
	if j.Run <= 0 || (j.Script == "") != j.Session || len(j.Script) > 64<<10 || j.GPUs < 0 || j.GPUs > 8 ||
		j.TimeoutSec <= 0 || j.TimeoutSec > int(MaxRun/time.Second) {
		return false
	}
	if j.Commit == "" {
		return j.Repo == ""
	}
	return commitRE.MatchString(j.Commit) && repoRE.MatchString(j.Repo)
}

// Command is one thing an agent asks its session for: a bash script to run, or a file to write.
type Command struct {
	ID         string `json:"id"`
	Script     string `json:"script,omitempty"`      // bash to run; empty when writing a file
	TimeoutSec int    `json:"timeout_sec,omitempty"` // for a script
	Path       string `json:"path,omitempty"`        // a file to write, absolute or relative to the code's folder
	Data       []byte `json:"data,omitempty"`        // its contents
}

// Commands is the reply to a request for commands: none when the wait ran out.
type Commands struct {
	Commands []Command `json:"commands"`
}

// CommandResult is how a command ended.
type CommandResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	TimedOut bool   `json:"timed_out"`
	Error    string `json:"error"` // what went wrong outside the command, or ""
}

var commandIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Valid checks a command before the connector acts on it. Its id names a folder on the donor's
// disk, so it must be plain.
func (c Command) Valid() bool {
	if !commandIDRE.MatchString(c.ID) {
		return false
	}
	if c.Path != "" {
		return c.Script == "" && len(c.Path) <= 4096 && !strings.ContainsRune(c.Path, 0) && len(c.Data) <= MaxFile
	}
	return c.Script != "" && len(c.Script) <= 256<<10 && len(c.Data) == 0 && c.TimeoutSec > 0 && c.TimeoutSec <= int(MaxRun/time.Second)
}
