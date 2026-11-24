package workflow

import (
	"encoding/json"
	"errors"
)

// RunDefaults are the starting values a preset gives a block's run settings when the block chooses it
// (Derek, 2026-11-24). nil is no starting value: the block's input is left as it is. They are only
// filled in the dialog; a block's own settings are stored and used as ever.
type RunDefaults struct {
	MaxTurns        *int `json:"max_turns,omitempty"`
	MaxMinutes      *int `json:"max_minutes,omitempty"`
	TimeWarnPercent *int `json:"time_warn_percent,omitempty"`
	SubAgents       *int `json:"sub_agents,omitempty"`
	SubAgentsPerRun *int `json:"sub_agents_per_run,omitempty"` // 0: no limit
}

// NewRunDefaults makes a RunDefaults from the five values in order: turns, minutes, warning percent, sub-agents
// at a time, sub-agents per run.
func NewRunDefaults(turns, minutes, warn, subs, perRun int) RunDefaults {
	return RunDefaults{&turns, &minutes, &warn, &subs, &perRun}
}

// ParseRunDefaults reads the stored JSON; an empty or unreadable one is no starting values.
func ParseRunDefaults(s string) RunDefaults {
	var r RunDefaults
	if s != "" && json.Unmarshal([]byte(s), &r) != nil {
		return RunDefaults{}
	}
	return r
}

// JSON is the stored form: "" when no value is set.
func (r RunDefaults) JSON() string {
	if r == (RunDefaults{}) {
		return ""
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// Check applies the bounds AgentSettings.Check has for the same settings.
func (r RunDefaults) Check() error {
	in := func(p *int, lo, hi int) bool { return p == nil || (*p >= lo && *p <= hi) }
	switch {
	case !in(r.MaxTurns, 1, 500):
		return errors.New("the turn limit is 1 to 500")
	case !in(r.MaxMinutes, 1, 240):
		return errors.New("the time limit is 1 to 240 minutes")
	case !in(r.TimeWarnPercent, 0, 50):
		return errors.New("the time warning is 0 to 50 percent of the time limit")
	case !in(r.SubAgents, 0, 16):
		return errors.New("sub-agents at a time are 0 to 16")
	case !in(r.SubAgentsPerRun, 0, 500):
		return errors.New("sub-agents per run are 0 (no limit) to 500")
	}
	return nil
}
