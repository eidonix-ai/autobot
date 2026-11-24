package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A template's [[tag]] (or [[tag|Label]]) becomes a Markdown link to a page that shows the tag's value as
// it is when the template is filled in. Autobot keeps that page (store.Publish) and serves it at
// /p/<uuid>.html, and the raw text at /p/<uuid>.md, to whoever may read the case: the same rule as
// the case file's own address. [[case]] is the exception: the case file keeps growing, so it links to
// the live case file, and publishes nothing.

// publisher keeps a tag's value as a page and returns its uuid; nil where nothing may be published
// (a test of one block from the canvas).
type publisher func(tag, content string) (string, error)

// links are what a template's [[links]] need: where to publish, and the case [[case]] points to.
type links struct {
	publish publisher                   // nil where nothing is published
	caseOf  func() (*store.Case, error) // the case file [[case]] links to, if it is open
}

// runLinks are a run's: its pages are published into its case file (the item's, opened if it has none;
// a dry run's own, which it deletes with them), and [[case]] is that file.
func (s *Server) runLinks(rs *runState) links {
	return links{
		publish: func(tag, content string) (string, error) {
			c, err := s.runCase(rs)
			if errors.Is(err, store.ErrNotFound) {
				c, err = s.caseForRun(rs)
			}
			if err != nil {
				return "", err
			}
			return s.st.Publish(c, tag, content)
		},
		caseOf: func() (*store.Case, error) { return s.runCase(rs) },
	}
}

// testLinks are a test of one block's: nothing is published, and [[case]] is the item's case file.
func (s *Server) testLinks(ws *store.Workspace, number int) links {
	return links{caseOf: func() (*store.Case, error) { return s.st.CaseByNumber(ws.ID, number) }}
}

// pageLink is the address of a published page, or "" before the site's public address is known.
func (s *Server) pageLink(uuid string) string {
	if base := s.siteAddress(); base != "" {
		return base + "/p/" + uuid + ".html"
	}
	return ""
}

// fillLinks adds to values the Markdown link each [[tag]] of the templates stands for, under the
// text as written (which is where workflow.Render looks). A tag's value is taken from values, so call
// it once they are all there. With no publisher the link shows its label and says nothing was
// published.
func (s *Server) fillLinks(lk links, templates []string, values map[string]string) {
	for _, t := range templates {
		for _, l := range workflow.TemplateLinks(t) {
			if _, done := values[l.Text]; done {
				continue
			}
			values[l.Text] = s.linkFor(lk, l, values)
		}
	}
}

func (s *Server) linkFor(lk links, l workflow.LinkRef, values map[string]string) string {
	if l.Name == "case" {
		if c, err := lk.caseOf(); err == nil {
			if link := s.caseLink(c); link != "" {
				return "[" + l.Label + "](" + link + ")"
			}
		}
		return l.Label + " (not opened yet)"
	}
	publish := lk.publish
	switch {
	case publish == nil:
		return l.Label + " (not published in a test)"
	case s.siteAddress() == "":
		return l.Label + " (not published: the site's public address is not set)"
	}
	uuid, err := publish(l.Name, workflow.Value(values, l.Name))
	if err != nil {
		return l.Label + " (not published: " + err.Error() + ")"
	}
	return "[" + l.Label + "](" + s.pageLink(uuid) + ")"
}

// publishedPage serves a published page: /p/<uuid>.html as a page, /p/<uuid>.md as the raw text. An
// unknown uuid is a bare 404; otherwise the case API's rule of access applies, with its instructions.
func (s *Server) publishedPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id, md := strings.CutSuffix(name, ".md")
	if !md {
		var html bool
		if id, html = strings.CutSuffix(name, ".html"); !html {
			http.Error(w, "No such page.", http.StatusNotFound)
			return
		}
	}
	p, err := s.st.PublishedPage(id)
	if err != nil {
		http.Error(w, "No such page.", http.StatusNotFound)
		return
	}
	ws, err := s.st.WorkspaceByID(p.WorkspaceID)
	if err != nil {
		http.Error(w, "No such page.", http.StatusNotFound)
		return
	}
	if !s.mayRead(w, r, ws) {
		return
	}
	if md {
		h := w.Header()
		h.Set("Content-Type", "text/markdown; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex")
		w.Write([]byte(p.Content))
		return
	}
	text := p.Content
	if strings.TrimSpace(text) == "" {
		text = "_This is empty._"
	}
	s.publicPage(w, r, workflow.LinkLabel(p.Tag)+" · "+ws.Name+" #"+strconv.Itoa(p.Item), text, "Raw Markdown for agents: "+s.addressOf(r)+"/p/"+p.UUID+".md")
}
