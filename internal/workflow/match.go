package workflow

import (
	"regexp"
	"strings"
	"unicode"
)

// Summon phrases and stop tags are matched ignoring case, with ? standing for at most one character (any) and
// * for any run of characters on one line. A phrase matches only as a whole: "#autobot" is not
// found in "#autobots".
func phraseRE(phrase string) *regexp.Regexp {
	phrase = strings.TrimSpace(phrase)
	var b strings.Builder
	b.WriteString(`(?i)`)
	runes := []rune(phrase)
	word := func(r rune) bool { return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') }
	if len(runes) > 0 && word(runes[0]) {
		b.WriteString(`\b`)
	}
	for _, r := range runes {
		switch r {
		case '?':
			b.WriteString(`[^\n]?`)
		case '*':
			b.WriteString(`[^\n]*?`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if len(runes) > 0 && word(runes[len(runes)-1]) {
		b.WriteString(`\b`)
	}
	return regexp.MustCompile(b.String())
}

// Stops reports whether text contains the stop tag.
func Stops(text, stopTag string) bool {
	return strings.TrimSpace(stopTag) != "" && phraseRE(stopTag).MatchString(text)
}

// Summons reports whether text summons Autobot: it contains the summon phrase outside any stop tag,
// so "#autobot ignore" is a stop and not also a summons when the phrase is "#autobot". A text that
// stops the run never summons it.
func Summons(text, phrase, stopTag string) bool {
	if strings.TrimSpace(phrase) == "" || Stops(text, stopTag) {
		return false
	}
	return phraseRE(phrase).MatchString(text)
}
