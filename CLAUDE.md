# Autobot: working rules

Autobot is a fresh start (2026-11-22) after an earlier attempt, the "triage harness", grew too
complex to fix. Build it piece by piece in the original vision; each piece is small, works end to end,
and is shown to Derek before the next begins.

## Principles

- A Go application backed by one SQLite file for both settings and data. No Node unless it is truly
  needed. Go's standard library first (net/http routing, html/template); add a dependency only when it
  removes real code.
- Server-rendered pages and plain forms. Simple panels: free text is one big text box, a fixed set is
  a checklist, a choice is a dropdown. No raw YAML, no per-entry forms with up/down/remove buttons.
- Keep it simple. Complexity has to earn its place.
- Workspaces are named exactly as their GitHub repository (`owner/repo`).

## The old code is for harvesting only

The old harness lives at `/Users/derek/src/triage-harness` (main) and its branch checkouts
`triage-harness-*`; the donor connector prototype is in `/Users/derek/src/triage-harness-donor`.
Lift a piece when it fits (for example the hardware connectors), read it first, and bring over
only what the current piece needs. Never copy its architecture, its config system or its flow engine.

## Hosting

Autobot runs on 192.168.5.123 (ssh alias `triage`, user `agent`, passwordless sudo) as
`autobot.service`, user `autobot`, listening on 192.168.5.123:8080, database
`/var/lib/autobot/autobot.db`. Deploy or upgrade with `deploy/install.sh`.

It is published as https://autobot.eidonix.com through nginx on the DMZ host (192.168.5.2, ssh alias
`dmz`, no systemd: reload with `nginx -s reload`). It is public since 2026-11-23 (Derek's OK, after the
security audit: two independent reviews, Go 1.27.4, the fixes in that day's commits); nginx still
limits sign-ins per address and connections and sizes. The config from before it went public is
`/etc/nginx/nginx.conf.pre-autobot-public-20261123`.

The old harness's `triage.service` and `donor-hub.service` were stopped and disabled on 2026-11-22
(Derek's OK); their data is still in `/var/lib/triage`. Don't delete it without asking.

## Local copy of the code

`internal/repo` keeps one lean bare git mirror per workspace in `<data dir>/repos/<workspace id>.git`:
the default branch and the heads of the open pull requests only (files over 1 MB are fetched only when
read). `autobot serve` syncs it every 10 minutes (failures back off 2 to 60 minutes), asking GitHub
which pull requests are open (an ETag list); a closed pull request loses its ref. Plain `git gc` cannot free the
objects only closed ones used (a partial clone keeps everything the server sent), so a mirror that has
grown past twice its size at the last fresh clone, or is 14 days old, is cloned afresh beside the old
one, checked (`fsck`, default branch resolves) and swapped in; if that fails the old mirror stays. A run on a pull request fetches just that head. The workspace's Settings page shows the
last fetch, size and any error. Agents get `list_files`, `read_file`, `search_code`, `search_history`,
`repo_overview` and `pr_diff` over it instead of GitHub API calls; with no mirror yet they read through
GitHub as before. It runs the system `git` (the host needs it installed); the workspace's token reaches
git only through its environment, never a command line or the mirror's config.

## Pieces

1. Users and workspaces, the project charter, first-time admin setup in the browser, workspace
   settings with names checked on GitHub, a per-workspace GitHub identity (user and encrypted
   token) (done).
2. Resources: donated test machines (done). A connector on each machine (cmd/autobot-connector,
   deployed with deploy/connector) checks in over HTTPS with a machine token and runs each test in a
   fresh job container with no network, a read-only root, no root user and capped resources. The
   old autobot-connector repository's runner/link/hub design was replaced, not reused (Derek,
   2026-11-22: "We value simple, robust designs").
3. Providers (done): owned by the user who pays for them and lent by the owner to workspaces they
   are assigned to; admins can view but not change others'. API keys only (no subscription
   logins: Derek, 2026-11-22, after the vendors' terms were checked). Opt-in permitted models,
   model discovery, decision providers for /v1/decisions and System One's /v1/systemone.
   Roles (Derek, 2026-11-22): admin is a super-admin (workspaces, members, users, sees all); a user
   configures everything inside their assigned workspaces but cannot create, rename or delete them.
4. Case files (done): one per issue or PR, a Markdown log opened by intake (workflows, not built
   yet; `autobot case open` meanwhile). Writers add contributions: a draft they may revise until
   finalized, then appended and immutable, with its line range. Human edits are recorded with a
   line note. GET /api/v1/cases/{uuid} serves the Markdown to contributors' agents: public repos by
   uuid alone; private repos also need the secret in .github/autobot-secret.txt (committed by the
   repo's people, read live by Autobot), and a request without it gets instructions to set it up.
5. Workflows (in progress): a canvas per kind (pull requests, issues), polling only, no webhooks.
   Event blocks (PR First Ready, PR Draft, PR Activity, Issue Opened) with summon phrase and stop tag
   (case-insensitive, ? and * wildcards, a stop is never also a summons); step blocks Agent (our
   own lightweight harness, no shell), Decision (one output per choice) and Case Entry. Links may
   fan out and merge; loops are refused for now. Templates use {tags}: built-in ones are a parent and
   its fields ({pr} is the link, {pr:title}, {pr:diff}… are filled from it; {repo}, {charter}),
   so the Test box asks only for the parent; old names such as {pr_title}, {project_charter} and {repo:charter} still work.
   The colon is for fields Autobot fills from a built-in parent; a dot names a part of a step's output
   (Derek, 2026-11-23). A step's "Output to" tag (say `gather`) is kept per case (one PR or issue) and
   replaced by the next write, with its parts: `{gather}` is the whole (an Agent's "## Summary", "## Report"
   and, when it has notes, "## Notes"), `{gather.summary}`, `{gather.report}` and `{gather.scratchpad}`
   (the thread's notes in full, high importance first, then medium, then low, each "#N [importance] Title"
   and its body; the main agent's only) the parts; a block with no closing step has one report, which is its
   summary too, so `{gather}` is the report plus the Notes. A Decision's output has no parts. `CheckTemplates`
   refuses an unknown part, a colon on an output, a dot on a built-in or a decision (typo checks, not limits).
   `{input}` is what the step before produced, in the same shape (`{input.summary}`, `{input.report}`,
   `{input.scratchpad}`): after an Agent its whole and parts; after any other step its output text, as both
   summary and report, with no notes; with several links in, each under its step's name. The old
   `{previous_output}` still means `{input.report}` (the report after an agent) and is not listed. A new GitHub
   Comment, Discord Post or Case Entry block starts with `{input.report}`; saved blocks keep their text.
   Shift-click tests one block with
   values for its tags and writes nothing. The harness (internal/agent) is goal-driven: it ends only
   through finish, which always takes the report (Derek, 2026-11-23): a cited file the agent never
   opened is marked "(not opened)" where it is cited; the plan is the agent's own notes. Only an
   optional check model (Verify) can send it back, and that report is kept meanwhile. It is told
   once, at 5 turns left, and on its last turn only finish may be called, so a run always ends with a report.
   The tools array is the same on every turn (a changed one makes the provider start over on the whole
   request): a turn that allows less (the last, the forced compact, the save turn) says which tools are
   allowed in its message, and a call to any other is answered with an error and not run.
   The context is the agent's to manage and nothing is ever trimmed automatically (Derek, 2026-11-23):
   conversations are append-only so the provider's cache hits, and what is sent is rewritten only when
   the agent prunes or compacts. Every tool result starts with its number, `[result N]`;
   `prune_result` (only once the context line shows the context is past half its window: before that it costs a cache miss for nothing) replaces results by `[result N pruned: first line; n characters]` (recorded as a
   `prune` event, so a replay rebuilds the same messages); once the previous request passed 50% of the
   model's window, the last result of a turn ends with `[context: 412k of 1,000k tokens]` (appended,
   never rewriting). `compact` replaces the whole conversation by the run's task and one message with
   what the agent carries forward, its scratchpad index and its plan (a `compact` event holds those
   messages). Past 85% of the window (the last request's tokens plus what was added at 3.5 characters a
   token) the turn is told to save and compact and may call only scratchpad_write, update_plan and
   compact; an agent that does not is compacted by the harness, which also compacts and retries once when
   the provider refuses a request as too long. Each permitted model has a context window (tokens), set
   in one box on the provider's page, prefilled from the provider's /models (vLLM's `max_model_len`,
   or `context_length` or `context_window`); the Eidonix gateway reports `context_window`, and 128,000 is
   assumed when none is known. The scratchpad holds the agent's notes outside its context: entries
   (title, importance low/medium/high, one-line description, body, author "main" for now) are only ever
   added, with tools scratchpad_write, scratchpad_index, scratchpad_read and scratchpad_search.
   Every run is a new session, starting fresh, in its **thread**: (workspace, kind, the block that
   started it, item). A thread has one scratchpad, shared by all its runs, and every run's final report
   is also kept in it, as a high-importance entry "Report of <date>, run <id>" (the full report as its body,
   the run's summary kept with it and cut to one line as its description). Re-run "resume" and a
   reference block ("New reference", dashed, "Continue the conversation of") open a session with the
   block's own Instruction and Done when, "Your last summary on this item:" and the thread's newest
   summary (not the full report: one line says the report is that entry, readable with scratchpad_read),
   the scratchpad index and what is new on the pull request since (the commits and comments after the head and time the previous run
   saw); their model and tools are the block's own (a reference block starts with the other block's
   model, an ordinary setting). With no earlier report the block skips or starts fresh; Re-run
   "fresh" starts a new thread with an empty scratchpad. A thread takes one run at a time (the others
   wait), and is kept, with its scratchpad, while its item is open plus 30 days. The closing step
   "Write the report from the scratchpad" (Gather Information's; Custom may choose it) splits a run in
   two: the research session ends as always, its finish report is saved as a high-importance entry
   "Research summary", it gets one more turn that may only write to the scratchpad, and a new session
   (its own conversation, the preamble, only read-only code and pull request tools, its own turns and
   10 minutes) writes the report from the scratchpad and finishes with it. The research's finish asks
   for a short summary only (the full report is written next). The writing session's finish takes two
   parts, both required: a summary (at most about 300 words: the 5 to 8 findings that matter most, one
   line each with its citation, then the open questions) and the full report (opening with a short "Key
   findings" section); both get the "(not opened)" marks. A finish with no summary is not refused: the
   summary is made from the report's Key findings section, else its first 300 words, and says so. Every
   run that has a report has a summary (a block with no closing step: its report; a failed writer, a
   crash, a run cut off by a restart or a notes report: made the same way), kept in the run's summary
   JSON as `summary` beside `report`; the run page has a Summary panel above the Report, both live
   (none for a run whose summary is its report). The run's report is the written one,
   else the research's own, else the high and medium notes in order; its outcome stays the research's,
   and "(not opened)" is judged against the files both sessions read. Conversations are stored with test-run
   logs as gzip files in /var/lib/autobot/runs: test logs 30 days, a conversation while its item is open
   plus 30 days; conversations from before threads are shown on the run page but never continued. The run
   page shows the scratchpad, the research transcript, the sub-agents and "Writing the report", all updating
   in place. A run that crashes keeps its last good transcript (the written report if any, else the
   research's, else the notes) and still saves it to the thread; a panic in a harness tool is an error
   answer; a run cut off by a restart that wrote notes and no report gets a notes report, kept in the
   thread. What the thread has seen of the item (head and time) moves only when a run came to a report.
   A thread or conversation with a run in progress is never dropped by retention, and nothing is written
   to a deleted thread.
   Gather Information (Derek, 2026-11-23) builds a broad case file: everything relevant to the pull request
   or issue, gathered, linked and cross-referenced, with no judgement. Its instruction is organized by
   angles (change, why, related work, recent activity, people, community, project fit, claims and evidence,
   reach, process; an issue's: the report, why and impact, and the same others), and tells the agent to record
   every detail to the scratchpad as its own entry the moment it finds it, the title starting with its angle
   ("Related work: #477 overlaps the same files"), with importance as how soon a reader should see it. Its Done
   when asks for entries under every angle and every packet lead followed. The closing step then groups the
   notes by angle prefix into one report section per angle (the writing prompt applies this only when the
   instruction names angles; an angle with no notes says "Nothing found"), and the summary gives each angle a
   line or two within about 300 words. The orientation packet (`{pr:packet}`, `{issue:packet}`: built-in, Auto,
   computed by Autobot with no model in internal/web/packet*.go when a block's text uses the tag, so only
   Gather Information's heads carry it) opens the first message after the head, under "## Orientation
   packet (computed by Autobot: a map of where to look, not conclusions)", about 3,000 tokens, never over
   12,000 characters (a section over 3,000 is cut with "…more"). Every line is a pointer. For a pull request:
   its facts (state, draft, age, base, head, size, labels, reviews from the index, CI of the head from GitHub
   with a 5 second limit); the changed files grouped by area (the first two segments of the folder) with
   whether the description names each area, its folders or its files, whole words in any case; the last 8
   default-branch commits in each of up to 8 areas within 60 days and the commits that landed since the
   merge-base on the changed files (git log by path on the mirror, no blobs); possibly related items from
   the index, found by explicit links, names (changed files' base names and the functions, types, flags and
   metric-like names on the diff's added and removed lines, 30 terms, longest first, any term that finds 20
   or more items dropped), the title's words, the author's open items, shared labels and pull requests
   touching the same files, ranked by how many directions found each then recency, top 15; the author's
   standing and last 180 days (counts, 10 most recent, areas of merged pull requests); the top 3 committers
   per area over 180 days; the charter's lines that name an area or a title word (15 at most, with line
   numbers). An issue's packet is the same without areas, activity and people, unless its text names
   paths that exist on the default branch. A source that is missing (no code copy or index yet, GitHub
   down) says "(not available: why)" in its section and never fails the run. A "Community (Discord)" section
   is left for when the Discord gateway reads exist.
   Sub-agents: with "Max sub-agents at a time" above 0 the main agent has `spawn_agent` (title, task),
   which waits for the sub-agent and returns its report, its outcome, an index (no bodies) of its notes
   and the run's totals ("Sub-agents so far: 3 started, 112 turns used."); a call made while that many are
   working is refused, and an optional per-run limit ("Limit sub-agents per run", off by default) refuses
   past the total. A sub-agent is a fresh session on the same harness (same model, the block's turns,
   numbered results, prune, compact, window, warnings) inside the parent's context and deadline, with the
   read-only code and pull request tools (the web ones only if the block allows the internet), no shell,
   no spawn_agent, and a private scratchpad (entries by s1, s2…, kept with the run and deleted with the
   thread); the main agent reads them with scratchpad_index, scratchpad_read and scratchpad_search, agent
   "s1", and copies nothing automatically. The main agent's compaction message and the writing session's
   first message list the sub-agents. The run's totals include theirs; the run page shows each as
   "Sub-agent s2: title". Time warning ("Warn before the time limit", 10% by default, 0 for none): when
   that share of the time is left every session still working (the main one, each sub-agent, a late one
   on its first turn, the writing session of its own 10 minutes) is told once, at its next turn,
   "About N minutes left: finish with your report soon." Built so far: PR and web tools, personas, and machine sessions: an agent whose
   settings list machines gets a shell in a long-lived job container on the first free one (a test run
   with the session flag: same isolation, all the machine's GPUs, the PR head's code). The connector
   takes the agent's commands over a long poll (GET /connector/v1/runs/{id}/commands, answered at
   .../commands/{cid}/result) and runs each with Docker exec; files go in through a read-only inbox
   folder the connector writes itself. Queues live in memory in the web Server, so a restart ends
   sessions (StopOrphanRuns); a session idle for 10 minutes, or whose agent ended, is closed. Tools:
   bash (background with bash_output/bash_kill), read_file, write_file, edit_file, grep, glob.
   The engine (internal/web/engine.go) runs a workflow on one PR or issue from an event block
   (shift-click it: number, dry run): steps run when all their inputs have finished and at least
   one link into them fired, so decisions route and forks rejoin; output tags are kept per case;
   runs keep a snapshot of the workflow and each step's state (workflow_runs, workflow_steps).
   More blocks: GitHub Comment (trial run on by default: writes what it would post to the case
   file; keep one comment edits its earlier one), Condition (rules on files, size, author's
   standing, labels, text, tags; no model), Test Run (a script on a machine; passed or failed),
   Post to Discord (the case's forum post, opened when needed; or its summary) and Human Approval
   (Discord buttons and the run page; first answer wins; waits survive restarts). Discord is
   reached over its plain HTTP API (no gateway yet).
   The poller (internal/web/poller.go, internal/workflow/poll.go) starts runs for workflows
   switched Live on the canvas (off by default; switching on baselines what is already open).
   Next: questions in case posts (needs the gateway), sub-agents.
6. Discord (decided by Derek, 2026-11-23): each workspace has its own bot, installed on its
   project's own server, set up in the workspace's Settings by any member (token sealed per
   workspace; clicks come to /discord/interactions/{owner}/{repo}, checked with that workspace's
   public key, and answer only that workspace's requests). Everything lives in ONE PRIVATE forum,
   flare-autobot, on the project server; its access list (server admins, moderators, Contributor,
   flare-contrib, the owner) is the set of people trusted to make the calls, so anyone who can see
   it may approve (no approver role, no separate approvals channel; bots' clicks ignored, stale
   buttons refused, first click wins, who and when recorded in the case file). One post per case,
   created by the bot; its first message carries the case's summary findings; people ask
   questions by mentioning the bot inside a case's post (read-only agent, rate-limited). Human
   Approval is a workflow block (outputs approved, denied, timed out) in front of frontier agents
   and hardware runs, usable to gate any fork. A private repository's cases stay out of Discord.
   The bot never pings @everyone or roles. Webhook channels were tried and dropped. Order: the
   bot with case posts, then Human Approval, then questions. Web search comes from Tavily or Brave providers,
   lent like inference; decision routes are detected per provider with its key.

## Pinned (Derek, 2026-11-23: come back to these)

- Prompt-injection exfiltration. An agent that reads untrusted text (a PR, a fetched page, a
  vendored file) can be told to send what it has read out through fetch_url, including through the
  DNS lookup of an attacker's host name, which happens before the address guard. The danger is
  private repositories. Proposed, not built: fetch_url limited to a list of documentation and
  package sites (checked on the name before any lookup), "any website" refused for private repos,
  link previews off for Discord posts, Human Approval before agent-written text is posted from a
  private repo, and a canary-PR measurement of how readily GLM and DeepSeek obey such text.
- Before the poller runs PRs automatically: cap and evict the connector's dependency cache (a PR's
  lockfile can make a donor download up to 20 GB, kept forever), and per-user run limits.

## Testing

`go test -p 1 ./...`. Every rule that protects data or access has a test, and a test is only
trusted once it has been seen to fail when the rule is broken.
