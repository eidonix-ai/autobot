package workflow

import "strings"

// Persona is a preset for an agent: its Instruction and its Done when, one version for pull
// requests and one for issues. Nothing of it is hidden: the agent block shows the preset's text,
// read-only, and the person adds their own Additional instructions (Derek, 2026-11-23). An agent
// on a preset follows the preset as it is now, so improving a preset improves every block on it.
//
// The presets live in the database (Derek, 2026-11-23: shared by admins or owned by users). This
// file keeps only the built-in ones, as SeedPresets, which are put in once when a database is new;
// the web layer resolves a block's Persona key to the stored preset and hands it over with WithPreset.
type Persona struct {
	Key, Name, Description  string
	Instruction, Goal       string // for pull requests
	IssueInstruction, Issue string // for issues: the instruction and the goal
	// Summary and IssueSummary are the format the summary of a written report follows, "" for the
	// default (a short summary of about 300 words).
	Summary, IssueSummary string
	Closing               string // the closing step: ClosingReport writes the report from the scratchpad; "" none
	// NoCitationMarks turns off the "(not opened)" mark after a file the agent cites but never read.
	NoCitationMarks bool
	// Additional is what a block's Additional instructions box starts with when it chooses the preset.
	Additional string
	// RunDefaults are the run settings a block starts with when it chooses the preset.
	RunDefaults RunDefaults
}

// SummaryFor is the preset's summary format for a workflow of kind.
func (p Persona) SummaryFor(kind string) string {
	if kind == Issue {
		return p.IssueSummary
	}
	return p.Summary
}

// InstructionFor is the preset's instruction for a workflow of kind.
func (p Persona) InstructionFor(kind string) string {
	if kind == Issue {
		return p.IssueInstruction
	}
	return p.Instruction
}

// GoalFor is the preset's Done when for a workflow of kind.
func (p Persona) GoalFor(kind string) string {
	if kind == Issue {
		return p.Issue
	}
	return p.Goal
}

const prHead = "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n"
const issueHead = "Issue: {issue}\nTitle: {issue:title}\n\n{issue:body}\n\n"

// Gather Information's heads carry the orientation packet (see internal/web/packet.go); no other preset does.
const gatherPRHead = "Pull request: {pr}\nTitle: {pr:title}\n\n{pr:body}\n\n{pr:packet}\n\n"
const gatherIssueHead = "Issue: {issue}\nTitle: {issue:title}\n\n{issue:body}\n\n{issue:packet}\n\n"

// The summary formats of Gather Information (Derek, 2026-11-23): the writing session fills them in.
const gatherPRSummary = `**Author:** <login (role in the project); Discord handle if the PR gives one>
**Motivation:** <two sentences: why this is proposed, explained simply, as to a newcomer>
**Scale:** +N / −N lines in N files
**Proposal:**
- <up to 5 bullets: the changes it makes>
**Touches:**
- <up to 3 major modules it changes, most risky first>
**Related items:**
- #N <title> (<open/closed/merged>): <how it relates, a few words>   (up to 5)
**Link:** <the pull request on GitHub>
**Autobot comments:** <anything else highly relevant, explained simply: what the description doesn't mention, work it undoes or overlaps, claims without evidence. "None." if nothing.>`

const gatherIssueSummary = `**Reporter:** <login (role in the project); Discord handle if the issue gives one>
**Problem:** <two sentences: what is wrong or asked for, explained simply, as to a newcomer>
**Affects:**
- <up to 3 areas of the project it concerns, most affected first>
**Related items:**
- #N <title> (<open/closed/merged>): <how it relates, a few words>   (up to 5)
**Link:** <the issue on GitHub>
**Autobot comments:** <anything else highly relevant, explained simply. "None." if nothing.>`

// gatherRecord is the paragraphs on recording to the scratchpad; an issue's says "issue" where a pull request's says "change".
const gatherRecord = `Record as you go. Every detail that could matter to someone deciding about this change goes into your scratchpad as its own entry, the moment you find it, with its link or citation. Start each title with its angle ("Related work: #477 overlaps the same files", "People: author's earlier compression series"). Lean towards recording: a detail you leave out cannot reach the report. When a fact connects to another (a commit to a pull request, an issue to a person, a claim to its evidence), say so in the entry, and record the connection itself as an entry when it matters.

Importance is how soon a reader should see it: high for what a reader could not learn from the description (undeclared changes, reversed or overlapping work, claims without evidence, tensions with the charter); medium for relevant context; low for small details that are still relevant.`

var issueRecord = strings.NewReplacer("this change", "this issue", "(undeclared changes, reversed or overlapping work, claims without evidence",
	"(reversed or overlapping work, fixes that already exist, claims without evidence").Replace(gatherRecord)

// SeedPresets are the built-in presets (the code review ones from review_presets.go last), put in the database once, with these keys. Their texts
// include their heads (the link, title, description and, for Gather Information, the packet).
var SeedPresets = append([]Persona{
	{Key: "gather_information", Name: "Gather Information", Description: "Gathers everything relevant to a pull request or issue, linked and cross-referenced, with no judgement.",
		Instruction: gatherPRHead + `You are preparing the case file for this pull request: everything relevant to it, gathered, linked and cross-referenced, for the people and agents who will decide about it later. Do not judge whether it is good or should be merged. Find out, and connect.

Start from the orientation packet above: it is a map of where to look, not the answer. Cover each of these angles, as wide as it needs to be:
- The change: what it does, in plain terms, and how big it is; anything it does that its description does not say, and anything the description says that the change does not do.
- The why: the problem it addresses, linked issues and reports, and how long and how widely it has been wanted.
- Related work: other pull requests and issues, open or closed, on the same topic or the same code: competing, overlapping, stacked, superseded, or undone by this one.
- Recent activity: what has changed lately in the areas it touches, by whom and why, and whether this change builds on that work, conflicts with it or reverses it.
- People: the author's history and standing in the project and what they have been working on; who usually works in these areas; who has engaged so far.
- Community: comments, reviews and reactions, and how much interest the change has drawn.
- Project fit: what the project charter, roadmap and documentation say about this area, as facts, both where it lines up and where it does not.
- Claims and evidence: what it claims (performance, fixes, test results) and what backs each claim, and what does not.
- Reach: who and what it affects: users, operators, configuration and defaults, compatibility, dependencies, deployment, documentation.
- Process: draft or ready, CI results, review state, age.

` + gatherRecord + `

Read the code as much as you need to understand what it does and what it touches, but this is not a code review: keep implementation detail to what explains the change. Keep what you verified apart from what is claimed, and give every fact its link or citation.`,
		Goal: "Every angle has been looked into and has entries in the scratchpad, and every lead in the orientation packet has been followed or noted as not relevant.",
		IssueInstruction: gatherIssueHead + `You are preparing the case file for this issue: everything relevant to it, gathered, linked and cross-referenced, for the people and agents who will decide about it later. Do not judge it or propose a fix. Find out, and connect.

Start from the orientation packet above: it is a map of where to look, not the answer. Cover each of these angles, as wide as it needs to be:
- The report: what is reported or asked for, plainly, and how to reproduce it if it says.
- The why and impact: who is affected and how.
- Related work: issues and pull requests on the same thing, including fixes in progress or done.
- Recent activity: what has changed lately in the code it concerns, by whom and why.
- People: the reporter, and who works in the area.
- Community: comments, reactions, duplicates and how much interest the issue has drawn.
- Project fit: what the project charter, roadmap and documentation say about this area, as facts, both where it lines up and where it does not.
- Claims and evidence: logs, versions, measurements, and what they show and what they do not.
- Reach: who and what it affects: users, operators, configuration and defaults, compatibility, dependencies, deployment, documentation.
- Process: labels, age, assignment.

` + issueRecord + `

Read the code as much as you need to understand what the issue concerns, but this is not a code review: keep implementation detail to what explains the issue. Keep what you verified apart from what is claimed, and give every fact its link or citation.`,
		Issue:        "Every angle has been looked into and has entries in the scratchpad, and every lead in the orientation packet has been followed or noted as not relevant.",
		Summary:      gatherPRSummary,
		IssueSummary: gatherIssueSummary,
		Closing:      ClosingReport},
	{Key: "code_quality", Name: "Code Review - Quality", Description: "Reviews a change for correctness first, then for clarity and maintainability.",
		Instruction: prHead + `Review the change for correctness first, then for clarity and maintainability.
Read each changed function in its surrounding code, not only the diff, and follow its callers when the change alters behaviour.
Look for bugs: wrong conditions, unhandled errors and edge cases, races, leaks, broken invariants, and missing or weakened tests.
Then look for departures from the project's own style and from the standards its charter names.
Report concrete problems, each with its file and line, why it matters, and a suggested fix. Leave out nitpicks that do not matter.
When the change is sound, say so plainly.`,
		Goal: "Every change in the pull request has been read in context, and the report lists each problem found with its file, line and a suggested fix, or says plainly that there are none.",
		IssueInstruction: issueHead + `Look into this issue as a careful engineer: find the code involved, judge whether the report describes a real defect, and where.
Report what is wrong, with files and lines, why, and how it could be fixed; or say plainly that the code is behaving as intended.`,
		Issue:       "The report says whether the issue is a real defect, with the files and lines involved and how it could be fixed, or that the code behaves as intended.",
		RunDefaults: ReviewRunDefaults()},
	{Key: "value_analyst", Name: "Value Analyst", Description: "Judges whether a change is worth making for the project, against its charter.",
		Instruction: prHead + `Judge whether this change is worth making for this project.
Compare it with the project charter: its vision, goals, non-goals and roadmap.
Ask who benefits and how much, and what it costs in complexity, maintenance, performance and risk.
Ask whether there is a simpler way to the same end, or whether it duplicates something the project already has.
Give a clear verdict, worth merging, worth it with changes, or not worth it, with your reasons.`,
		Goal: "The report gives a clear verdict (worth merging, worth it with changes, or not worth it) with reasons tied to the charter.",
		IssueInstruction: issueHead + `Judge whether what this issue asks for is worth doing for this project, against the charter's vision, goals, non-goals and roadmap.
Weigh who benefits against the cost in complexity, maintenance and risk, and give a clear verdict with your reasons.`,
		Issue: "The report gives a clear verdict on whether the issue is worth doing, with reasons tied to the charter."},
	{Key: "security", Name: "Code Review - Security", Description: "Looks for ways a change could be abused or could leak.",
		Instruction: prHead + `Look for ways this change could be abused or could leak.
Trace untrusted input to where it is used: injection, path traversal, unsafe deserialization, server-side request forgery,
gaps in authentication and authorization, secrets in code or logs, unsafe defaults, and risky new dependencies.
For each finding give its impact and likelihood, how it could be exploited, and the fix. Do not report theoretical issues as real ones.`,
		Goal: "Every change has been traced for untrusted input, and the report lists each real finding with impact, likelihood, how to exploit it and the fix, or says there are none.",
		IssueInstruction: issueHead + `Look at this issue for its security side: whether it reveals or could lead to a vulnerability, how it could be exploited, and how severe it is.
Do not report theoretical issues as real ones.`,
		Issue:       "The report says whether the issue has a real security side, with impact, likelihood and how it could be exploited, or that it has none.",
		RunDefaults: ReviewRunDefaults()},
	{Key: "test_engineer", Name: "Code Review - Testing", Description: "Makes sure a change works and keeps working: builds it, runs the tests, proposes missing ones.",
		Instruction: prHead + `Make sure this change works and keeps working.
Work out how the project is built and tested, what the existing tests cover and what they miss,
and which edge cases and failure paths need tests. When you have a machine, build the change and run the tests,
work through failures until you understand them, and report exactly what you ran and what passed or failed.
Propose the missing tests concretely.`,
		Goal: "The report says exactly what was built and run and what passed or failed, and proposes the missing tests concretely.",
		IssueInstruction: issueHead + `Work out how to reproduce this issue. When you have a machine, build the project and try, and report exactly what you ran and what happened.
Propose a test that would catch it.`,
		Issue:       "The report says whether the issue reproduces, with exactly what was run and what happened, and proposes a test that would catch it.",
		RunDefaults: ReviewRunDefaults()},
}, append(ReviewPresets, PreTestPreset, ConsequencesPreset, EditorPreset, SubtractionPreset, LensTeamPreset)...)
