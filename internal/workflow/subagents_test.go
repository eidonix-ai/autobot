package workflow

import (
	"encoding/json"
	"testing"
)

func form(m map[string]string) AgentSettings {
	base := map[string]string{"persona": "custom", "max_turns": "30", "max_minutes": "20", "rerun": "resume", "sub_agents": "2"}
	for k, v := range m {
		base[k] = v
	}
	return AgentFromForm(func(k string) string { return base[k] }, func(string) []string { return nil })
}

func valid() AgentSettings {
	s := DefaultAgent(PR)
	s.Model = ModelRef{Provider: 1, Model: "m"}
	return s
}

// The limit on sub-agents per run is off unless its checkbox is on: unchecked saves 0 whatever the number
// says; checked takes the number, which must be 1 to 500.
func TestSubAgentsPerRunSetting(t *testing.T) {
	if got := form(map[string]string{"sub_agents_per_run": "8"}); got.SubAgentsPerRun != 0 || got.SubAgents != 2 {
		t.Fatalf("unchecked: %+v", got)
	}
	if got := form(map[string]string{"limit_sub_agents": "on", "sub_agents_per_run": "8"}); got.SubAgentsPerRun != 8 {
		t.Fatalf("checked: %+v", got)
	}
	for _, bad := range []string{"", "0", "-3", "many", "501"} {
		s := valid()
		s.SubAgentsPerRun = form(map[string]string{"limit_sub_agents": "on", "sub_agents_per_run": bad}).SubAgentsPerRun
		if s.Check(PR) == nil {
			t.Errorf("a limit of %q was accepted", bad)
		}
	}
	s := valid()
	s.SubAgentsPerRun = 8
	if err := s.Check(PR); err != nil {
		t.Fatal(err)
	}
	// An old block has none.
	var old AgentSettings
	json.Unmarshal([]byte(`{"sub_agents":3}`), &old)
	if old.SubAgentsPerRun != 0 {
		t.Fatal("an old block has a limit")
	}
}

// The time warning is 10% of the time limit by default and for a block saved before it was a setting;
// 0 means none; the form sets it.
func TestTimeWarningSetting(t *testing.T) {
	if DefaultAgent(PR).EffectiveTimeWarn() != 10 {
		t.Fatal("the default is not 10")
	}
	var old AgentSettings
	json.Unmarshal([]byte(`{"max_minutes":20}`), &old)
	if old.EffectiveTimeWarn() != 10 {
		t.Fatalf("an old block: %d", old.EffectiveTimeWarn())
	}
	var none AgentSettings
	json.Unmarshal([]byte(`{"max_minutes":20,"time_warn_percent":0}`), &none)
	if none.EffectiveTimeWarn() != 0 {
		t.Fatalf("an explicit 0: %d", none.EffectiveTimeWarn())
	}
	for in, want := range map[string]int{"25": 25, "0": 0} {
		if got := form(map[string]string{"time_warn_percent": in}); got.EffectiveTimeWarn() != want {
			t.Errorf("%q: %d", in, got.EffectiveTimeWarn())
		}
	}
	if got := form(nil); got.TimeWarnPercent != nil || got.EffectiveTimeWarn() != 10 { // a form that does not send it
		t.Fatalf("%+v", got)
	}
	for _, bad := range []string{"-1", "51", "soon"} {
		s := valid()
		s.TimeWarnPercent = form(map[string]string{"time_warn_percent": bad}).TimeWarnPercent
		if s.Check(PR) == nil {
			t.Errorf("a warning of %q was accepted", bad)
		}
	}
	// 0 survives saving and loading.
	b, _ := json.Marshal(form(map[string]string{"time_warn_percent": "0"}))
	var back AgentSettings
	json.Unmarshal(b, &back)
	if back.TimeWarnPercent == nil || back.EffectiveTimeWarn() != 0 {
		t.Fatalf("%s", b)
	}
}
