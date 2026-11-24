package workflow

import "testing"

func TestPhraseMatching(t *testing.T) {
	const summon, stop = "#autobot", "#autobot? ignore"
	for _, c := range []struct {
		text           string
		summons, stops bool
	}{
		{"#autobot please review", true, false},
		{"Hey #AutoBot, take a look", true, false},
		{"#autobot: ignore", false, true},
		{"#AUTOBOT- Ignore this one", false, true},
		{"#autobots are great", false, false},              // only as a whole word
		{"x#autobot", true, false},                         // "#" starts the phrase, so nothing is needed before it
		{"#autobot ignore", false, true},                   // ? may also stand for no character
		{"#autobot review\n#autobot: ignore", false, true}, // a stop anywhere wins
	} {
		if got := Summons(c.text, summon, stop); got != c.summons {
			t.Errorf("Summons(%q) = %v", c.text, got)
		}
		if got := Stops(c.text, stop); got != c.stops {
			t.Errorf("Stops(%q) = %v", c.text, got)
		}
	}
	// The stop tag's own text is not also a summons, and * spans words but not lines.
	if Summons("#autobot ignore", "#autobot", "#autobot ignore") {
		t.Error("a stop tag counted as a summons")
	}
	if !Stops("autobot, please ignore this", "autobot*ignore") || Stops("autobot\nignore", "autobot*ignore") {
		t.Error("* matching")
	}
	// Literal regexp characters in a phrase are just characters.
	if !Summons("ping (bot) now", "(bot)", "stop") || Summons("ping bot now", "(bot)", "stop") {
		t.Error("special characters")
	}
	// A tag that matches the summon phrase itself would make every summons an opt-out: Summons says no.
	if Summons("#autobot ignore-me", "#autobot ignore-me", "#autobot ignore*") {
		t.Error("a summons that is also an opt-out summoned")
	}
}
