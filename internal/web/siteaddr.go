package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The site's public address (https://autobot.eidonix.com) is what links to Autobot in a run are
// built from, such as {case:link}. A background run has no request to learn it from, so it is kept
// as a setting. It is filled once, from the first signed-in request's scheme and Host as the proxy
// passes them on, and from then on only an admin changes it: a link is never built from another
// request's Host.

// publicAddressKey is the setting holding the site's public address.
const publicAddressKey = "public_address"

// addressRE is a site address: http or https, a host name or address and perhaps a port, no path.
var addressRE = regexp.MustCompile(`^https?://(?:[a-z0-9]([a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// checkAddress cleans up an address typed in, or says why it is not one. Empty clears it.
func checkAddress(v string) (string, error) {
	v = strings.TrimRight(strings.ToLower(strings.TrimSpace(v)), "/")
	if v != "" && !addressRE.MatchString(v) {
		return "", errors.New("give the address as https://host, with a port if it has one and no path, such as https://autobot.example.com")
	}
	return v, nil
}

// siteAddress is the site's public address, or "" when it is not known yet.
func (s *Server) siteAddress() string {
	v, _ := s.st.Setting(publicAddressKey)
	return v
}

// noteSiteAddress fills an empty public address from a signed-in request, once. It is called for
// every signed-in request and costs nothing once the address is known.
func (s *Server) noteSiteAddress(r *http.Request) {
	if s.addressKnown.Load() {
		return
	}
	if s.siteAddress() != "" {
		s.addressKnown.Store(true)
		return
	}
	addr, err := checkAddress(publicURL(r))
	if err != nil || addr == "" {
		return // an odd Host header is not an address: wait for a request with a proper one
	}
	if _, err := s.st.SetSettingIfEmpty(publicAddressKey, addr); err == nil {
		s.addressKnown.Store(true)
	}
}

// saveSiteAddress sets the public address (admins only).
func (s *Server) saveSiteAddress(w http.ResponseWriter, r *http.Request) {
	addr, err := checkAddress(r.PostForm.Get("address"))
	if err != nil {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{"Error": err.Error() + ".", "Address": r.PostForm.Get("address")})
		return
	}
	if err := s.st.SetSetting(publicAddressKey, addr); err != nil {
		s.fail(w, err)
		return
	}
	s.addressKnown.Store(addr != "")
	http.Redirect(w, r, "/admin/users?address=1", http.StatusSeeOther)
}

// caseLink is the address of a case file as a page to read, which follows the file as it grows (its
// raw Markdown, for agents, is the same address without the .html).
func (s *Server) caseLink(c *store.Case) string {
	base := s.siteAddress()
	if base == "" || c == nil {
		return ""
	}
	return base + "/api/v1/cases/" + url.PathEscape(c.UUID) + ".html"
}

// caseTags are the built-in tags that come from an item's case file: {pr:case_file} or
// {issue:case_file}, {case} (the same text) and {case:link}.
func (s *Server) caseTags(kind string, c *store.Case) map[string]string {
	return map[string]string{
		workflow.ItemTag(kind) + ":case_file": c.Contents,
		"case":                                c.Contents,
		"case:link":                           s.caseLink(c),
	}
}
