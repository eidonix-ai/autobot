# Autobot

Automated GitHub code review, built piece by piece. Autobot organizes its work into **workspaces**. Each
workspace is one GitHub repository plus the test hardware donors lend it. Each workspace has a project
charter, resources, case files and a history.

This is the proof of concept. Piece 1 is done: users, workspaces and the project charter.

## What works now

- **Users** are admins or regular users. Admins create workspaces, add users, change roles, assign
  workspaces and reset passwords; admins can open every workspace. Regular users see only the
  workspaces they are assigned to (any other workspace does not exist for them).
- **Workspaces** are named exactly as their GitHub repository, `owner/repo`, and live at the matching
  URL: `/w/NVIDIA/hyperflare-inference`.
- **The project charter** is one Markdown document per workspace, on its own page, edited in a single
  text box. New charters start with Vision, Philosophy, Goals & Non-Goals, Applicable Standards,
  Competitors and Roadmap. Every save is kept as a revision; if two people edit at once, the second
  save is refused with their text kept, so nobody's edit is silently lost.
- **History** lists what happened in a workspace: creation, charter edits, assignments.
- **Resources** and **case files** are placeholders until their pieces are built.

## Run it

Requires Go 1.27. There is no Node and no C toolchain; SQLite is the pure-Go `modernc.org/sqlite`.

```sh
go build -o autobot ./cmd/autobot
./autobot useradd -db autobot.db -admin -name "Your Name" yourname   # prompts for a password
./autobot serve -db autobot.db -addr 127.0.0.1:8080
```

Then sign in at http://127.0.0.1:8080 and create a workspace. `autobot passwd USERNAME` resets a
password from the command line (for a locked-out admin).

Build for the Linux host with `GOOS=linux GOARCH=amd64 go build -o autobot ./cmd/autobot`.

## Layout

| Path | What it holds |
| --- | --- |
| `cmd/autobot` | the binary: `serve`, `useradd`, `passwd` |
| `internal/store` | the SQLite database: schema migrations, users and sessions, workspaces, charter, history |
| `internal/web` | the web interface: handlers, embedded templates and CSS (no JavaScript) |

All settings and data live in the one SQLite file. Back it up by copying it while the service is
stopped, or with `sqlite3 autobot.db ".backup copy.db"` while it runs.

## Security notes

- Passwords are bcrypt hashes; session cookies are HttpOnly and SameSite=Lax, and the database stores
  only a hash of each session token. Every form post carries a per-session token.
- The charter renders Markdown with raw HTML escaped and `javascript:` links dropped.
- The first admin is created on the command line, so there is no "first visitor becomes admin" window.
- There is no sign-in rate limit yet. Keep the service on the LAN, behind the nginx allow list,
  until there is one.

## Test

```sh
go test -p 1 ./...
```
