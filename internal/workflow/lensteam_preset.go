package workflow

// LensTeamPreset is one agent that leads a team review (Derek, 2026-11-24): it hands each lens to a
// sub-agent of its own, all in one turn so they work at the same time, then checks and combines what they
// bring back. The lens texts are the constants of the single-lens presets. Its starting settings give it
// the turns, time and sub-agents a team needs.
var LensTeamPreset = Persona{Key: "review_lens_team", Name: "Code Review - Lens Team",
	Description: "One agent that hands each review lens to a sub-agent of its own, at the same time, then checks and combines their findings.",
	Instruction: gatherPRHead + lensTeamInstruction + "\n\n" + reviewRules,
	Goal:        "Every lens has been reviewed by a sub-agent (or by you, where one failed), every blocking and should-fix finding has been checked in the code and kept or dropped with a reason, and the report gives each lens's findings with severity, file and line, why it matters and the fix.",
	Summary: `**Verdict:** <blocking issues / should-fix issues only / clean>
**Blocking:**
- <lens, file:line: the issue, in one line>   (up to 5; "None.")
**Should fix:**
- <lens, file:line: the issue, in one line>   (up to 5; "None.")
**By lens:** Correctness <n> · Architecture <n> · Conventions <n> · Subtraction <n> · Prose <n> · Consequences <n>
**Dropped on checking:** <how many, or "None.">
**Link:** {pr}`,
	IssueInstruction: reviewIssueInstruction, Issue: reviewIssueGoal, Closing: ClosingReport,
	RunDefaults: NewRunDefaults(60, 60, 20, 6, 0)}

const lensTeamInstruction = `You lead a team review of this pull request. You do not review it alone: you hand each lens to a sub-agent of its own with spawn_agent, all in the same turn so they work at the same time, then check and combine what they bring back.

1. Read the orientation packet above, and the pull request's description and diff (pr_diff), enough to brief the team. Do not review it yourself yet.
2. In one turn, call spawn_agent once for each lens below, with the lens's name as the title. A sub-agent sees nothing but its task, so each task must stand on its own: the pull request's number, title and link; the lens's text below, word for word; the "How to review" rules below, word for word; and the parts of the orientation packet that matter to that lens (the changed files and areas, related items, recent commits, the people, the charter lines), copied in. Ask each to bring back every finding with its severity, file and line, what is wrong, why it matters and the fix, or to say plainly that it found none.
3. As the reports come back, check every blocking and should-fix finding against the code yourself. Drop each one you cannot show from the code, with the reason. Merge findings that several lenses made, keeping each lens's name. Nits may stand unchecked.
4. Record each finding you keep in your scratchpad as you go: the title starts with its lens and severity ("Correctness, blocking: …"), and the body has the file and line, what is wrong, why, the fix, and which sub-agent found it. Record each dropped finding too, titled "Dropped: …", with the reason.
5. If a sub-agent failed or ran out of time, say so; review that lens yourself only if time allows.

The lenses:

A. Correctness. ` + correctnessLens + `

B. Architecture. ` + architectureLens + `

C. Conventions. ` + conventionsLens + `

D. Subtraction. ` + subtractionLens + `

E. Prose. ` + proseLens + `

F. Unintended consequences. ` + consequencesLens
