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
  URL: `/w/NVIDIA/hyperflare-inference`. The name is checked on GitHub when a workspace is created or
  renamed and saved as GitHub spells it; a repository GitHub does not know is refused, and so is any
  name when GitHub cannot be reached.
- **GitHub identity** (per workspace): the GitHub user Autobot acts as there, such as
  flare-autobot, and that user's token. Required for a private repository, optional for a public
  one. Before saving, Autobot checks that the token belongs to that user and that the user can see
  the repository; the settings page re-checks it each time it opens and shows the user's access and
  the token's expiry. Tokens are encrypted and never shown again.
- **Workspace settings** (admins): rename the repository, choose the members from a checklist, or
  delete the workspace by typing its name.
- **The project charter** is one Markdown document per workspace, on its own page, edited in a single
  text box. New charters start with Vision, Philosophy, Goals & Non-Goals, Applicable Standards,
  Competitors and Roadmap. Every save is kept as a revision; if two people edit at once, the second
  save is refused with their text kept, so nobody's edit is silently lost.
- **History** lists what happened in a workspace: creation, charter edits, assignments.
- **Resources**: donated test machines. An admin adds a machine and gets its token once; the
  machine's owner starts the connector (below). Each machine's page shows its live state, GPU
  readings and the models in its models folder, and admins can run a test on it: a bash script,
  optionally on a branch, tag or commit of the repository. Output streams to the run's page.
- **Roles**: an admin is a super-admin who creates, renames and deletes workspaces, chooses their
  members and manages users, and can see everything. A user runs the workspaces they are assigned
  to: everything inside them (settings, GitHub identity, machines, test runs, the charter) is theirs
  to configure.
- **Providers** belong to the user who pays for them: an endpoint, an API key (encrypted, never
  shown again, not even to admins), and the models it may use, which are opt-in. The owner lends a
  provider to the workspaces they are assigned to; a workspace's Models card lists what is lent to it
  and by whom. Only the owner changes or lends a provider; an admin can look but not change. A
  disabled owner's providers stop working everywhere. Presets: OpenAI, Anthropic, OpenRouter,
  Together AI, Groq, Fireworks, DeepInfra, Mistral, xAI, Google Gemini, DeepSeek, System One, or a
  custom endpoint. Decision providers speak `/v1/decisions` or System One's `/v1/systemone`.
- **Case files** are a placeholder until that piece is built.

## The connector on a donated machine

`cmd/autobot-connector` runs on each donated machine as one Docker container
(`deploy/connector/compose.yml`). It checks in with Autobot every two seconds over HTTPS with its
machine token. For a run, it downloads the repository at the exact commit from GitHub and starts a
fresh job container from the work image:

- no network, so the job cannot reach the machine, its local network or the internet;
- a read-only root filesystem, uid 1000, no capabilities, no privilege gain;
- capped CPU, memory (no swap) and processes, with scratch only in a size-capped in-memory `/work`;
- only the GPUs the run asked for, and the code and models folder mounted read-only.

The container is removed when the run ends. `docker compose run --rm connector check` starts a job
container with these settings and confirms each one from inside. The connector holds the Docker
socket, as any CI runner does, so a donated machine should be used only for testing.

Setting up a machine, once an admin has added it in Autobot and has its token:

```sh
cd deploy/connector
cp example.env .env && chmod 600 .env     # fill in the token, work image and models folder
docker compose up -d --build
docker compose run --rm connector check
```

## Run it

Requires Go 1.27. There is no Node and no C toolchain; SQLite is the pure-Go `modernc.org/sqlite`.

```sh
go build -o autobot ./cmd/autobot
./autobot serve -db autobot.db -addr 127.0.0.1:8080
```

Open http://127.0.0.1:8080. While there are no users, the first visitor is taken to a setup page
and creates the first admin account; after that the page is closed for good. For recovery,
`autobot useradd -admin USERNAME` adds an admin and `autobot passwd USERNAME` resets a password from
the command line.

Build for the Linux host with `GOOS=linux GOARCH=amd64 go build -o autobot ./cmd/autobot`.

## Layout

| Path | What it holds |
| --- | --- |
| `cmd/autobot` | the binary: `serve`, `useradd`, `passwd` |
| `internal/store` | the SQLite database: schema migrations, users and sessions, workspaces, charter, history |
| `internal/providers` | asking an inference provider which models it offers |
| `internal/web` | the web interface: handlers, embedded templates and CSS (no JavaScript) |

All settings and data live in the one SQLite file. Back it up by copying it while the service is
stopped, or with `sqlite3 autobot.db ".backup copy.db"` while it runs.

GitHub tokens and provider API keys in the database are encrypted (AES-256-GCM) with a key kept outside it: on the host,
`/etc/autobot/secret.key` (root only, handed to the service by systemd); when run by hand, the key
is `DB.key` next to the database, created on first start. Back the key up separately. Without it the
saved tokens cannot be read and must be entered again; nothing else is lost.

## Security notes

- Passwords are bcrypt hashes; session cookies are HttpOnly and SameSite=Lax, and the database stores
  only a hash of each session token. Every form post carries a per-session token.
- The charter renders Markdown with raw HTML escaped and `javascript:` links dropped.
- Until setup is done, anyone who can reach the site can create the first admin, so finish setup
  as soon as the service is first started. Only one admin can come out of setup, however many
  people submit at once.
- Form posts from other sites are refused (Go's `http.CrossOriginProtection`), so a web page cannot
  submit the setup or sign-in form through a visitor's browser.
- There is no sign-in rate limit yet. Keep the service on the LAN, behind the nginx allow list,
  until there is one.

## Test

```sh
go test -p 1 ./...
```
