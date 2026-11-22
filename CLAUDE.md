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
5. Next: to be decided with Derek.

## Testing

`go test -p 1 ./...`. Every rule that protects data or access has a test, and a test is only
trusted once it has been seen to fail when the rule is broken.
