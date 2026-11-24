package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Post is the block that says something: the same text, as a comment on the item on GitHub and in
// the case's forum post on Discord, whichever are ticked. It is an endpoint, so it keeps no output
// tag.
//
// It replaced two blocks, GitHub Comment and Post to Discord, which are still the types of graphs
// saved before: they load as Post nodes (see Graph.UnmarshalJSON), each with the destination it had.
const (
	Post          = "post"
	githubComment = "github_comment" // the old types
	discordPost   = "discord_post"
)

// PostSettings are a Post node's settings.
type PostSettings struct {
	Message string `json:"message"` // a template
	GitHub  bool   `json:"github"`  // a comment on the item
	// Trial: write what the comment would be into the case file, and post nothing on GitHub.
	Trial   bool `json:"trial"`
	Discord bool `json:"discord"` // a message in the case's forum post
	// Summary: replace the forum post's first message (its summary) instead of adding a message.
	Summary bool `json:"summary"`
}

// DefaultPost is a new Post node's settings: a comment on GitHub, as a trial until someone decides,
// with no text yet.
func DefaultPost() PostSettings { return PostSettings{GitHub: true, Trial: true} }

// Check confirms a Post node's settings are complete.
func (s PostSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Message) == "":
		return errors.New("write the message")
	case len(s.Message) > maxTemplate:
		return fmt.Errorf("the message is limited to %d characters", maxTemplate)
	case !s.GitHub && !s.Discord:
		return errors.New("tick where to post: GitHub, Discord or both")
	}
	return nil
}

// PostFromForm reads a Post node's settings from its form.
func PostFromForm(get func(string) string) PostSettings {
	return PostSettings{Message: strings.TrimSpace(get("message")), GitHub: get("github") == "on", Trial: get("trial") == "on",
		Discord: get("discord") == "on", Summary: get("summary") == "on"}
}

// Where says where the node posts, as its card shows it: "GitHub (trial) · Discord".
func (s PostSettings) Where() string {
	var parts []string
	if s.GitHub {
		if s.Trial {
			parts = append(parts, "GitHub (trial)")
		} else {
			parts = append(parts, "GitHub")
		}
	}
	if s.Discord {
		if s.Summary {
			parts = append(parts, "Discord (summary)")
		} else {
			parts = append(parts, "Discord")
		}
	}
	if len(parts) == 0 {
		return "Tick where to post"
	}
	return strings.Join(parts, " · ")
}

// upgrade turns a node of one of the two old posting types into a Post node with the same name and
// settings, each with the destination it had. Any other node is returned as it is.
func upgrade(n Node) Node {
	switch n.Type {
	case githubComment:
		old := struct {
			Message string `json:"message"`
			Trial   bool   `json:"trial"`
		}{"{input.report}", true} // what a block never saved had
		if len(n.Settings) > 0 {
			json.Unmarshal(n.Settings, &old)
		}
		n.Type, n.Name = Post, cmpName(n.Name, "GitHub Comment")
		n.Settings, _ = json.Marshal(PostSettings{Message: old.Message, GitHub: true, Trial: old.Trial})
	case discordPost:
		old := struct {
			Message string `json:"message"`
			Summary bool   `json:"summary"`
		}{Message: "{input.report}"}
		if len(n.Settings) > 0 {
			json.Unmarshal(n.Settings, &old)
		}
		n.Type, n.Name = Post, cmpName(n.Name, "Post to Discord")
		n.Settings, _ = json.Marshal(PostSettings{Message: old.Message, Discord: true, Summary: old.Summary})
	}
	return n
}

// cmpName keeps a node's name, or gives it the old type's label, so a block does not change name when it changes type.
func cmpName(name, old string) string {
	if name != "" {
		return name
	}
	return old
}
