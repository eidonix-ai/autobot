# Autobot: Working Rules & Architecture

Autobot is a fresh start (2026-11-22) after an earlier attempt, the "triage harness", grew too complex to fix. Build it piece by piece in the original vision; each piece is small, works end to end, and is shown to Derek before the next begins.

## Principles

- **Go standard library first** (`net/http`, `html/template`), backed by **one SQLite file** for both settings and data. No Node unless truly needed; add a dependency only when it removes real code.
- **Server-rendered pages and plain forms**, with minimal JS (`workflow.js`, `live.js`, Drawflow) only for the workflow canvas and live updates. Simple panels: free text is one big text box, a fixed set is a checklist, a choice is a dropdown. No raw YAML, no per-entry forms with up/down/remove buttons.
- **Keep it simple.** Complexity has to earn its place.
- **Workspaces** are named exactly as their GitHub repository (`owner/repo`).
- **The old code is for harvesting only:** `/Users/derek/src/triage-harness` (main + `triage-harness-*`) and `/Users/derek/src/triage-harness-donor`. Read first and lift only what fits; never copy its architecture, config system, or flow engine.

## Hosting & Operations

- **App host:** `192.168.5.123` (SSH alias `triage`, user `agent`, passwordless sudo) running `autobot.service` as user `autobot`, listening on `192.168.5.123:8080`, SQLite DB `/var/lib/autobot/autobot.db`, gzipped run logs/conversations in `/var/lib/autobot/runs`. Deploy or upgrade with `deploy/install.sh`.
- **DMZ proxy:** Published at `https://autobot.eidonix.com` via nginx on `192.168.5.2` (SSH alias `dmz`, no systemd: reload with `nginx -s reload`). Public since **2026-11-23** (Derek's OK, after the security audit: two independent reviews, Go 1.27.4, security fixes); nginx still limits sign-ins per IP, connections, and request sizes. Pre-public config backup: `/etc/nginx/nginx.conf.pre-autobot-public-20261123`.
- **Old services:** `triage.service` and `donor-hub.service` were stopped and disabled on **2026-11-22** (Derek's OK); their data remains in `/var/lib/triage` — **do not delete it without asking**.

## Package Map

| Package | Responsibility |
| --- | --- |
| `cmd/autobot` | Main server binary (`serve`, `useradd`, `passwd`, `machine`, `run`, `case`, `charter`). |
| `cmd/autobot-connector` | Sandboxed test-runner agent on donated hardware (`deploy/connector`). |
| `internal/store` | Single SQLite DB (`modernc.org/sqlite`, `MaxOpenConns(1)`), append-only schema migrations (`migrations` in `store.go`), sealed secrets, presets, threads/scratchpads, cases, workflow/agent runs, item index. |
| `internal/workflow` | Graph model, block types, template tags (`{tag}`, `[[tag\|Label]]`), validation, retagging, built-in presets (`personas.go`, `review_presets.go`, `rundefaults.go`). |
| `internal/agent` | Goal-driven LLM harness: OpenAI-compatible client, cache-friendly append-only conversations, scratchpad/prune/compact, closing report writer (`closing.go`), sub-agents (`subagents.go`), citation annotation. |
| `internal/web` | HTTP handlers, auth/CSRF, workflow engine (`engine.go`), agent runner (`agentrun.go`), run queue (`queue.go`), stop/cancellation (`stop.go`), orientation packet (`packet*.go`), GitHub poller (`poller.go`), Discord bot/interactions (`discord*.go`), templates & static assets. |
| `internal/repo` | Per-workspace lean bare git mirror (`<data dir>/repos/<ws>.git`) synced every 10 min (2–60 min backoff; freshly re-cloned if >2× initial size or >14 days old). GitHub token reaches `git` only via env, never CLI args or git config. |
| `internal/index` | Local copy of open PRs/issues + 30-day closed items (180-day slim PR records with files) for search, cross-references, and orientation packets. |
| `internal/github`, `internal/discord`, `internal/decisions`, `internal/search`, `internal/netguard`, `internal/protocol`, `internal/providers` | External API clients, SSRF guard (`netguard.PublicOnly`), connector protocol. |

## Decisions & System Rules

### 1. Users, Roles & Workspaces
- **Roles (Derek, 2026-11-22):** Admin is a super-admin (creates/renames/deletes workspaces, manages members, users, public site address, trusted LAN hosts, and shared presets; sees all). Regular users configure everything inside their assigned workspaces.
- **Public address:** Admin setting on the Users page, captured once from the first signed-in request's scheme and Host behind the proxy and changed only by an admin; links are never built from later request Hosts.

### 2. Resources (Donated Test Machines)
- Connectors check in over HTTPS with a machine token and run each test (and agent shell sessions) in a fresh job container with no network, read-only root, non-root user, and capped resources. The old runner/link/hub design was replaced, not reused (Derek, 2026-11-22: *"We value simple, robust designs"*).

### 3. Providers & Models
- **Ownership & lending:** Providers are owned by the user who pays for them and lent by the owner to workspaces they are assigned to; admins can view but never modify another user's providers.
- **API keys only** (no subscription logins: Derek, 2026-11-22, after checking vendor terms). Keys and GitHub tokens are sealed with the key file.
- **Models & routes:** Opt-in permitted models with model discovery and context window (`max_model_len` / `context_length` / `context_window`, default 128,000). Decision routes (`/v1/decisions` and System One's `/v1/systemone`) are auto-detected per provider; web search comes from Tavily or Brave providers, lent like inference.

### 4. Case Files & Published Pages
- One Markdown log per PR or issue; contributions are drafted then finalized (immutable, with line ranges); human edits record line notes.
- Served at `/api/v1/cases/{uuid}` (raw Markdown) and `.html` (readable page with "Raw Markdown for agents: …"), and published `[[tag|Label]]` snapshots at `/p/{uuid}.html` / `.md` (retained 30 days after item close).
- **Access rule:** Public repos by UUID alone; private repos also require `.github/autobot-secret.txt` committed in the repo (read live by Autobot).

### 5. Workflows
- **Polling only, no webhooks** (one canvas per kind: `pr`, `issue`; Live off by default, baselines open items when switched on).
- **Opt-out & summons (Derek, 2026-11-23):** One workspace-level opt-out tag (in Settings, `{repo:opt_out}`), honored in item body and comments by the item author or collaborators with write access — never Autobot's own comments or others. "Trusted only" applies to summons only. A summon phrase may never match the opt-out tag.
- **Tags & block naming (Derek, 2026-11-23):** Colon for Autobot built-in fields (`{pr:title}`, `{pr:packet}`, `{case:link}`, `{charter}`, `{machines}`); dot for step output parts (`{step.summary}`, `{step.report}`, `{step.scratchpad}`; Decision `.confidence`, `.reason`). Every step's tag is its unique snake-case block name; renaming rewrites downstream templates and moves stored case values atomically. Reference blocks (`ContinueOf`) have no name/tag of their own and write to their source's tag.
- **Blocks:** Event blocks (`PR First Ready`, `PR Draft`, `PR Activity`, `Issue Opened`), `Agent`, `Decision`, `Condition`, `Test Run`, `Human Approval` (required bold Question above buttons + optional Details: Derek, 2026-11-24), `Case Entry`, `Post` (replaced GitHub Comment & Post to Discord: Derek, 2026-11-23; Trial run on by default for GitHub), and `Node` (`junction` wire connector: Derek, 2026-11-23).
- **Auto case entries (Derek, 2026-11-23):** Agent and Decision steps append finalized results to the case file by default (`no_case_entry` inverse flag).
- **Stop, Dry runs, Concurrency, Restarts (Derek, 2026-11-24):**
  - **Stop:** Cancels the run's context (`agent.StopError`), ends agents immediately with no report, edits waiting Discord approval cards, and guards all subsequent tag/case/post writes.
  - **Dry runs:** Get an isolated dry-run case (`dry_run_of`, negative `number` in `cases`), never read/write the real case or post externally; deleted 30 days after ending.
  - **Concurrency:** Per-workspace max runs at once (1–20, default 3) with FIFO queue (`queued`); runs waiting only for approvals release their slot and jump ahead of queued runs once answered.
  - **Deploy restarts:** `Agent`, `Decision`, `Condition`, and `Junction` steps cut off by a restart re-queue and run again keeping finished steps (`workflow.Restartable`); `Post`, `CaseEntry`, `TestRun`, and `Approval` never run twice; dry runs fail as before.

### 6. Agent Harness & Presets
- **Goal-driven & cache-friendly (Derek, 2026-11-23):** Ends only through `finish`; cited files never opened are marked `(not opened)` (skipping URLs) rather than refusing finish. Conversations are append-only with a fixed `tools` array every turn (narrowed turns enforce allowed tools via message + error); `Conversation.request()` coalesces adjacent `user` messages before sending. Context is agent-managed (numbered `[result N]`, `prune_result` only past 50% when `[context: ...]` appears, `compact` at ~70%, forced save+compact at 85%).
- **Budgets & overtime (Derek, 2026-11-24):** Warned at 5 turns left and at 20% time left (default); time-limit cutoff gets one finish-only overtime turn (5 min); an Agent step leaving no report fails so downstream steps never run on empty output. Agent workflow step timeout is at least 4 hours and scales for long `MaxMinutes` + thread wait + closing writer.
- **Threads, Closing Step & Sub-agents:** Every run starts a fresh session in its thread `(ws, kind, block, item)` sharing one append-only scratchpad. With `Closing: "report"`, research ends with a short summary + 1 save turn (`scratchpad_write` only), then a fresh read-only writer session writes `{summary, report}`. Sub-agents (`spawn_agent`) run concurrently up to `SubAgents`, with private scratchpads (`s1`, `s2`…) and `include_context: true` to pass item details + orientation packet without copying.
- **Orientation packet (Derek, 2026-11-23):** Computed deterministically (`internal/web/packet*.go`, max 12,000 chars) only when a block uses `{pr:packet}` / `{issue:packet}`: PR facts/CI, changed areas (noting ones omitted from description), recent default-branch commits, related items, author standing, top area committers, matching charter lines.
- **Presets (Derek, 2026-11-23/24):** Stored in SQLite (`presets` table), split like providers (shared: admin-only; personal: owner-only, readable/copyable by all). Seeded once on a fresh DB; one-time upgrades for existing DBs go into `presetSeedSteps()` (`internal/web/presets.go`) via `Store.RunPresetSteps` (using SHA-256 `Upgrades` so admin edits are never overwritten). Includes `Gather Information`, `Value Analyst`, `Editor - Prose Cleanup` (`NoTools: true`, `NoCitationMarks: true`), `Code Review - Quality / Security / Testing / All Lenses / Correctness / Architecture / Conventions / Subtraction / Prose / Unintended Consequences / Pre-Test Report / Lens Team`, each carrying `RunDefaults`.
- **Prompt hygiene:** Keep shared presets repository-agnostic (direct agents to `read_charter` and repo docs rather than hardcoding repo-specific rules); keep `closing.go` and `finishWriterSpec` generic (preset-specific structure belongs in that preset's instruction); align research `Goal` with `Closing: "report"`.

### 7. Discord (Derek, 2026-11-23)
- Each workspace has its own bot on its project's server (token sealed per workspace; button clicks hit `/discord/interactions/{owner}/{repo}`, verified with that workspace's Ed25519 public key and answered only for that workspace).
- **One private forum** (`flare-autobot`) on the project server. Its access list is the trusted set — **anyone who can see it may approve** (no approver role, no separate approvals channel). Bots' clicks are ignored, stale buttons refused, first click wins, and who/when is recorded in the case file.
- One forum post per case (first message holds the summary). **A private repository's cases never reach Discord.**
- **The bot never pings `@everyone` or roles**, and suppresses link previews (`suppressEmbeds`). Webhook channels were tried and dropped.
- **Next:** Gateway reader for Q&A / `@mention` workflows and Discord community context in orientation packets.

## Pinned (Derek, 2026-11-23: come back to these)

- **Prompt-injection exfiltration (private repos):** Untrusted PR/page/vendored text can try to exfiltrate data via `fetch_url` (including via DNS lookup of an attacker hostname before `netguard` runs). Planned: domain allowlist for `fetch_url` checked before DNS, refuse arbitrary URLs on private repos, Human Approval before posting agent-written text from a private repo, and canary-PR testing on GLM and DeepSeek.
- **Before automatic poller PR runs:** Cap and evict the connector's dependency cache (a PR lockfile can make a donor download up to 20 GB, kept forever), and add per-user run limits.

## Testing

Two sizes (Derek, 2026-11-24: tests must not slow iteration down):
- **Quick, while iterating:** `scripts/test.sh quick [package…]` — `go vet` and tests of changed packages only, no browser tests.
- **Full, for releases:** `scripts/test.sh full` — `go vet` (with and without `-tags browser`), all tests, headless Chrome browser tests, and `-race` on `internal/agent`, `internal/store`, `internal/workflow`. `deploy/install.sh` runs `full` first (`AUTOBOT_SKIP_TESTS=1` only in an emergency).
- Every rule that protects data or access, or controls what gets posted or run, must have a test verified to fail when the rule is broken. Test behavior and invariants rather than freezing prompt prose with brittle exact-string/SHA-256 snapshot assertions.
- **Schema migrations:** `store.migrations` in `internal/store/store.go` is strictly append-only; never edit a released entry.
