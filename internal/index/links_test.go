package index

import (
	"slices"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
)

func TestLinks(t *testing.T) {
	const repo = "NVIDIA/hyperflare-inference"
	cases := []struct {
		name string
		text string
		self int
		want []store.ItemLink
	}{
		{"mention", "see #12 for context", 1, []store.ItemLink{{To: 12, How: "mentions"}}},
		{"fixes", "Fixes #12", 1, []store.ItemLink{{To: 12, How: "fixes"}}},
		{"fixed with colon", "fixed: #12", 1, []store.ItemLink{{To: 12, How: "fixes"}}},
		{"closes", "This closes #12 and resolves #13", 1, []store.ItemLink{{To: 12, How: "closes"}, {To: 13, How: "closes"}}},
		{"strongest way wins", "#12 is related. Fixes #12.", 1, []store.ItemLink{{To: 12, How: "fixes"}}},
		{"same repo url", "https://github.com/nvidia/Hyperflare-Inference/pull/40 and https://github.com/NVIDIA/hyperflare-inference/issues/41", 1,
			[]store.ItemLink{{To: 40, How: "mentions"}, {To: 41, How: "mentions"}}},
		{"fixes a url", "closes https://github.com/NVIDIA/hyperflare-inference/issues/41", 1, []store.ItemLink{{To: 41, How: "closes"}}},
		{"owner/repo#n of this repo", "fixes NVIDIA/hyperflare-inference#44", 1, []store.ItemLink{{To: 44, How: "fixes"}}},
		{"another repository is dropped", "fixes other/repo#5, see https://github.com/other/repo/pull/6 and https://github.com/NVIDIA/other/issues/7", 1, nil},
		{"itself is dropped", "this is #1", 1, nil},
		{"glued to a word is not a reference", "page#12, &#39; and abc#13 and a/b/#14", 1, nil},
		{"a heading or list is", "- #15\n(#16)\n#17", 1, []store.ItemLink{{To: 15, How: "mentions"}, {To: 16, How: "mentions"}, {To: 17, How: "mentions"}}},
		{"no number", "fixes # and #abc", 1, nil},
	}
	for _, c := range cases {
		got := Links(repo, c.self, c.text)
		slices.SortFunc(got, func(a, b store.ItemLink) int { return a.To - b.To })
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q gave %v, want %v", c.name, c.text, got, c.want)
		}
	}
}
