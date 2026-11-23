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
`dmz`, no systemd: reload with `nginx -s reload`). The server block allows only 192.168.0.0/16 until
the security audit passes (Derek, 2026-11-22). LAN clients on other subnets reach nginx as
192.168.5.1; outside clients keep their public address, so the allow list holds. The pre-change config
is `/etc/nginx/nginx.conf.pre-autobot-internal-20261122`.

The old harness's `triage.service` and `donor-hub.service` were stopped and disabled on 2026-11-22
(Derek's OK); their data is still in `/var/lib/triage`. Don't delete it without asking.

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
   Event blocks (PR Ready, PR Draft, PR Activity, Issue Opened) with summon phrase and stop tag
   (case-insensitive, ? and * wildcards, a stop is never also a summons); step blocks Agent (our
   own lightweight harness, no shell), Decision (one output per choice) and Case Entry. Links may
   fan out and merge; loops are refused for now. Templates use {tags}; a step's "Output to" tag is
   kept per case (one PR or issue) and replaced by the next write. Shift-click tests one block with
   values for its tags and writes nothing. The harness (internal/agent) is goal-driven: it ends only
   through finish, refuses "met" with open plan steps or with files cited but never read, warns
   near its turn budget, and trims old tool output when sending; transcripts are stored per
   message, gzipped. Built so far: PR and web tools, personas, and machine sessions: an agent whose
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
   Next: the Discord bot, Human Approval, questions in case posts, sub-agents, the poller.
6. Discord (decided by Derek, 2026-11-23; not built yet): one bot for the instance (discordgo, one
   outbound Gateway connection, no public endpoint). Everything lives in ONE PRIVATE forum,
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

## Testing

`go test -p 1 ./...`. Every rule that protects data or access has a test, and a test is only
trusted once it has been seen to fail when the rule is broken.
