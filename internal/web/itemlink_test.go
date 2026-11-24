package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// {pr:link} is the same as {pr}: it renders as the pull request's link and is listed under {pr}.
func TestPRLinkTagRendersAndIsListed(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("a", "Link", "L=[{pr:link}] P=[{pr}]")), [2]string{"ev1", "a"})
	steps := c.run("ev1")
	out := steps["a"].Output
	link := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(out, "]", 2)[0], "L=["), "]")
	if !strings.HasPrefix(link, "http") || out != "L=["+link+"] P=["+link+"]" {
		t.Fatalf("rendered %q", out)
	}
	_, form := c.b.get("/w/o/r/workflows/pr/nodes/a")
	i, j := strings.Index(form, "<dt><code>{pr}</code>"), strings.Index(form, `<dt class="field"><code>{pr:link}</code>`)
	k := strings.Index(form, `<dt class="field"><code>{pr:number}</code>`)
	if i < 0 || j < i || k < j || !strings.Contains(form[j:k], `class="auto"`) {
		t.Errorf("{pr:link} is not listed under {pr} as an automatic field:\n%s", form)
	}
}
