package workflow

// The code review presets (Derek, 2026-11-23): a first reviewer over all five lenses, and one preset per
// lens. Every one reads a pull request through the orientation packet and the project's own rules, and
// reports each finding with its severity, place, reason and fix. They are shared presets like the others:
// put in with the rest on a new database, and in once on an older one (PresetsV2, see the store).

// PresetRename is the name a seeded preset had, and the name it was given when the review presets came.
// An older database renames it only if it still has the old name: an admin's own rename stays.
type PresetRename struct{ Key, From, To string }

// PresetRenames are the three seeded presets that became code review presets. Their keys never change.
var PresetRenames = []PresetRename{
	{"code_quality", "Code Quality", "Code Review - Quality"},
	{"security", "Security Reviewer", "Code Review - Security"},
	{"test_engineer", "Test Engineer", "Code Review - Testing"},
}

// reviewRules follows each lens's text.
const reviewRules = `How to review:
- Read the project's own rules first: its charter (read_charter), and the contributor, design-review and writing guidance in the repository (AGENTS.md, CONTRIBUTING, docs on design and review). Judge against those, and cite them.
- Stay inside your lens: other reviewers cover the other lenses.
- Read each changed function in its surrounding code, not only the diff, and follow its callers when behaviour changes.
- For every finding give: its severity (blocking, should fix, or nit), the file and line, what is wrong, why it matters, and the fix.
- Before you report a finding, check it in the code. Drop anything you cannot show from the code; say what would settle it if only a test can.
- For GPU kernel code (CUDA and other low-level GPU paths), report defects only; do not propose designs for it.
- Record each finding in your scratchpad as you go, with its severity at the start of the title.
- If you find nothing in your lens, say so plainly.`

const reviewGoal = "Every change has been read in context through this review's lens, and the report lists each finding with its severity, file and line, why it matters and the fix, or says plainly that there are none."

const reviewSummary = `**Verdict:** <blocking issues / should-fix issues only / clean>
**Blocking:**
- <file:line: the issue, in one line>   (up to 5; "None.")
**Should fix:**
- <file:line: the issue, in one line>   (up to 5; "None.")
**Nits:** <how many, or "None.">
**Link:** {pr}`

const reviewIssueInstruction = "This preset reviews pull requests; use it on a pull-request workflow."
const reviewIssueGoal = "Not applicable to issues."

// review is a code review preset for a lens text.
func review(key, name, description, lens string) Persona {
	return Persona{Key: key, Name: name, Description: description,
		Instruction: gatherPRHead + lens + "\n\n" + reviewRules, Goal: reviewGoal, Summary: reviewSummary,
		IssueInstruction: reviewIssueInstruction, Issue: reviewIssueGoal, Closing: ClosingReport}
}

// PreTestPreset gathers the lens reviewers' reports into one checked report for the decision on
// testing on real hardware (Derek, 2026-11-24). It reads the reviews from {input.report}: link the lens
// blocks into it, or into a Node in front of it.
var PreTestPreset = Persona{Key: "review_pretest", Name: "Code Review - Pre-Test Report",
	Description: "Checks and combines the lens reviewers' findings into one report that says whether to go on to testing on real hardware.",
	Instruction: gatherPRHead + "## The reviews\n\n{input.report}\n\n" +
		`You are assembling the Pre-Test Report: the reviewers' findings above, checked and combined, for the decision on whether this pull request goes on to testing on real hardware. You do not review it again from scratch.

1. Check every blocking and should-fix finding against the code yourself. Drop each one you cannot show from the code, and say which you dropped and why. Merge findings that several reviewers made.
2. For each finding that stands, say whether it would make a hardware test unsafe, meaningless or wasted (it would not build, would crash or corrupt state, or could not give a valid result), or whether it can wait until after testing.
3. Say what a hardware test should cover: which configurations, models and hardware targets, and what result would show the change works, following the project's own test and benchmark guidance (read_charter, and the repository's docs).
4. Give a clear recommendation: proceed to hardware testing, fix first, or do not test.

Do not repeat the reviews in full: give the checked result, with file and line for every finding. Record each checked finding in your scratchpad as you go, with "kept" or "dropped" at the start of its title. For GPU kernel code, report defects only.`,
	Goal: "Every blocking and should-fix finding has been checked in the code and kept or dropped with a reason, and the report gives a clear recommendation on hardware testing, with its reasons and what the test should cover.",
	Summary: `**Recommendation:** <proceed to hardware testing / fix first / do not test>
**Why:** <two sentences>
**Blocking before testing:**
- <file:line: the issue, in one line>   (up to 5; "None.")
**Can wait until after testing:**
- <file:line: the issue, in one line>   (up to 5; "None.")
**Dropped on checking:** <how many, and up to 3 in one line each, or "None.">
**Test plan:**
- <configuration or hardware target: the result that would show it works>   (up to 5)
**Link:** {pr}`,
	IssueInstruction: reviewIssueInstruction, Issue: reviewIssueGoal, Closing: ClosingReport}

// ReviewPresets are the six new code review presets.
var ReviewPresets = []Persona{
	review("review_all", "Code Review - All Lenses", "The first reviewer: reads a pull request through all five lenses and groups the findings by lens.",
		"You are the first reviewer of this pull request, covering all five review lenses: A. Correctness and consequences; B. Architecture and scope; C. Conventions and duplication; D. Simplicity; E. Prose. (Each lens is described in the project's other Code Review presets: correctness everywhere the change reaches; whether it belongs and the approach is right; consistency with precedent and no duplicated helpers; what can be deleted; and the quality of the description, commits, docs and comments.) Give the findings grouped by lens."),
	review("review_correctness", "Code Review - Correctness", "Lens A: does the change work everywhere it reaches, in every supported configuration.",
		"Your lens is A, correctness and consequences: does it work everywhere it reaches? Check every caller, and every configuration the project supports: each model, cache scheme, parallel layout, multi-host setup and hardware target its documentation names. Check resource lifetimes, inactive and edge-case paths, state and graph transitions, and whether existing data, artifacts and exported cores still load."),
	review("review_architecture", "Code Review - Architecture", "Lens B: does the change belong in the repository, and is the approach right.",
		"Your lens is B, architecture and scope: does this belong in the repository, and is the approach right? Judge it against the project's rejection criteria and design-review principles as its documents state them: for example keeping the core simple, keeping each layer in its role, no per-model special cases or flags, compatibility with what exists, and measured evidence for performance claims."),
	review("review_conventions", "Code Review - Conventions", "Lens C: consistency with the closest precedent, and no duplicated helpers.",
		"Your lens is C, conventions and duplication: compare each new construct with its closest precedent in the repository and flag where it departs from the established way, citing the precedent. Flag any existing helper or mechanism it duplicates, citing it."),
	review("review_simplicity", "Code Review - Simplicity", "Lens D: what can be deleted, with the smaller version shown.",
		"Your lens is D, simplicity: what can be deleted? Look for unneeded code, options, fallbacks and parallel paths, tests that do not prove a behaviour, and bloated comments. For each, show the smaller version."),
	review("review_prose", "Code Review - Prose", "Lens E: the pull request's description, commit messages, docs and comments against the project's writing rules.",
		"Your lens is E, prose: check the pull request's description, its commit messages, and the docs and comments it changes against the repository's writing rules. Check that the description has the four parts: why, what changed, the claimed result, and the evidence."),
}
