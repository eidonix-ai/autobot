package workflow

import "testing"

// {pr:link} and {issue:link} are the item's link under the name that says what it is: the same value as
// {pr} and {issue}, built in, filled in by Autobot, and listed under their parents.
func TestItemLinkTags(t *testing.T) {
	values := map[string]string{"pr": "https://github.com/o/r/pull/7", "issue": "https://github.com/o/r/issues/9"}
	if got := Render("{pr:link} | {pr} | {issue:link} | [[case]]", values); got != "https://github.com/o/r/pull/7 | https://github.com/o/r/pull/7 | https://github.com/o/r/issues/9 | [[case]]" {
		t.Errorf("rendered %q", got)
	}
	if got := Render("{pr:link}", map[string]string{"pr:link": "own"}); got != "own" {
		t.Errorf("a value of its own: %q", got)
	}
	for kind, tag := range map[string]string{PR: "pr", Issue: "issue"} {
		if err := CheckTemplates(kind, nil, "{"+tag+":link}"); err != nil {
			t.Errorf("%s: {%s:link}: %v", kind, tag, err)
		}
		list := Tags(kind)
		at := -1
		for i, listed := range list {
			if listed.Name == tag+":link" {
				at = i
				if !listed.Auto || !listed.Field() {
					t.Errorf("%s: {%s:link} is not an automatic field: %+v", kind, tag, listed)
				}
			}
		}
		if at < 1 || list[at-1].Name != tag {
			t.Errorf("%s: {%s:link} is not listed right after {%s}: %v", kind, tag, tag, list)
		}
	}
	// Each kind has its own.
	if err := CheckTemplates(Issue, nil, "{pr:link}"); err == nil {
		t.Error("{pr:link} in an issue workflow")
	}
}
