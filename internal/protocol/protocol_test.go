package protocol

import (
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// A script job has a script; a session has none, and a job cannot be both.
func TestJobValid(t *testing.T) {
	good := Job{Run: 1, Repo: "o/r", Commit: sha, Script: "make test", GPUs: 1, TimeoutSec: 60}
	session := Job{Run: 1, Repo: "o/r", Commit: sha, Session: true, GPUs: 8, TimeoutSec: 60}
	for name, tc := range map[string]struct {
		job  Job
		want bool
	}{
		"script":                  {good, true},
		"session":                 {session, true},
		"session without code":    {Job{Run: 1, Session: true, TimeoutSec: 60}, true},
		"empty script":            {Job{Run: 1, Repo: "o/r", Commit: sha, TimeoutSec: 60}, false},
		"session with a script":   {Job{Run: 1, Repo: "o/r", Commit: sha, Script: "x", Session: true, TimeoutSec: 60}, false},
		"session, no time":        {Job{Run: 1, Repo: "o/r", Commit: sha, Session: true}, false},
		"session over the limit":  {Job{Run: 1, Repo: "o/r", Commit: sha, Session: true, TimeoutSec: 3 * 3600}, false},
		"session, bad commit":     {Job{Run: 1, Repo: "o/r", Commit: "main", Session: true, TimeoutSec: 60}, false},
		"session, too many GPUs":  {Job{Run: 1, Repo: "o/r", Commit: sha, Session: true, GPUs: 9, TimeoutSec: 60}, false},
		"session, no run":         {Job{Repo: "o/r", Commit: sha, Session: true, TimeoutSec: 60}, false},
		"repo without commit":     {Job{Run: 1, Repo: "o/r", Session: true, TimeoutSec: 60}, false},
		"script with a long body": {Job{Run: 1, Script: strings.Repeat("x", 65<<10), TimeoutSec: 60}, false},
	} {
		if got := tc.job.Valid(); got != tc.want {
			t.Errorf("%s: Valid() = %v, want %v", name, got, tc.want)
		}
	}
}

// A command's id names a folder on the donor's disk, so only plain ids are accepted.
func TestCommandValid(t *testing.T) {
	for name, tc := range map[string]struct {
		c    Command
		want bool
	}{
		"script":          {Command{ID: "c1", Script: "ls", TimeoutSec: 10}, true},
		"file":            {Command{ID: "c2", Path: "src/a.rs", Data: []byte("x")}, true},
		"empty file":      {Command{ID: "c2", Path: "src/a.rs"}, true},
		"no id":           {Command{Script: "ls", TimeoutSec: 10}, false},
		"id with a dot":   {Command{ID: "../x", Script: "ls", TimeoutSec: 10}, false},
		"id with a slash": {Command{ID: "a/b", Path: "x"}, false},
		"long id":         {Command{ID: strings.Repeat("a", 33), Path: "x"}, false},
		"script no time":  {Command{ID: "c1", Script: "ls"}, false},
		"both":            {Command{ID: "c1", Script: "ls", TimeoutSec: 1, Path: "x"}, false},
		"neither":         {Command{ID: "c1"}, false},
		"huge file":       {Command{ID: "c1", Path: "x", Data: make([]byte, MaxFile+1)}, false},
	} {
		if got := tc.c.Valid(); got != tc.want {
			t.Errorf("%s: Valid() = %v, want %v", name, got, tc.want)
		}
	}
}
