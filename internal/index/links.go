package index

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
)

// What one piece of text (a title, a description or a comment) says about other items of the same
// repository. Three things count:
//
//	#123                                                    a mention
//	https://github.com/<owner>/<repo>/(pull|issues)/123     a mention (this repository only)
//	<owner>/<repo>#123                                      a mention (this repository only)
//
// and any of them right after fix, fixes, fixed, close, closes, closed, resolve, resolves or
// resolved (GitHub's closing words) is "fixes" (fix...) or "closes" (close... and resolve...).
// References to other repositories, and an item's reference to itself, are dropped. A "#" glued to
// a word ("page#12", "&#39;") is not a reference.
var refRE = regexp.MustCompile(`(?i)(?:\b(fix(?:e[sd])?|close[sd]?|resolve[sd]?)[: ]+)?` +
	`(?:https?://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:pull|issues)/(\d+)|([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)|#(\d+))`)

var rank = map[string]int{"mentions": 0, "fixes": 1, "closes": 2}

// Links reads the references to other items of repo ("owner/repo") out of text written on item self.
func Links(repo string, self int, text string) []store.ItemLink {
	best := map[int]string{}
	for _, m := range refRE.FindAllStringSubmatchIndex(text, -1) {
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[m[2*i]:m[2*i+1]]
		}
		var n int
		var other string
		switch {
		case group(3) != "":
			n, other = atoi(group(3)), group(2)
		case group(5) != "":
			n, other = atoi(group(5)), group(4)
		default:
			n = atoi(group(6))
			if prev := text[max(m[0]-1, 0)]; m[0] > 0 && group(1) == "" && (isWord(prev) || strings.IndexByte("_&/#", prev) >= 0) {
				continue
			}
		}
		if n <= 0 || n == self || (other != "" && !strings.EqualFold(other, repo)) {
			continue
		}
		how := "mentions"
		switch w := strings.ToLower(group(1)); {
		case strings.HasPrefix(w, "fix"):
			how = "fixes"
		case w != "":
			how = "closes"
		}
		if old, ok := best[n]; !ok || rank[how] > rank[old] {
			best[n] = how
		}
	}
	var out []store.ItemLink
	for n, how := range best {
		out = append(out, store.ItemLink{To: n, How: how})
	}
	return out
}

func isWord(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
