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

The old harness ran on 192.168.5.123 (ssh alias `triage`, user `agent`) on port 8080, published as
autobot.eidonix.com through nginx on the DMZ host (192.168.5.2), LAN only. Its services
(`triage.service`, `donor-hub.service`) were still running there on 2026-11-22. Don't stop them or
take the port without Derek's OK.

## Pieces

1. Users and workspaces, the project charter (done).
2. Next: to be decided with Derek.

## Testing

`go test -p 1 ./...`. Every rule that protects data or access has a test, and a test is only
trusted once it has been seen to fail when the rule is broken.
