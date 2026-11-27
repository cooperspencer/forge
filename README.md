# Preface

After I saw that [soft-serve](https://github.com/charmbracelet/soft-serve) was archived, I got kinda sad because I really liked the "everything in the terminal" approach. So I booted up Claude and let it go to work.
Needless to say, this project was done by Claude with my input. Usually I wouldn't upload it, but I think it is a cool project and I'd be happy if someone used it.

# forge

A lightweight, self-hosted git forge with integrated CI/CD, where the **terminal is the primary interface**.

```
ssh -p 23231 git.example.com
```

That one command gives you repositories, code browsing, commits and diffs, issues, pull
requests with reviews, releases, pipelines with live build logs, runners, users, permissions
and secrets — without a browser.

- **TUI-first** — a Bubble Tea app served over SSH. Keyboard-driven, split panes, filterable lists, adapts to the terminal size.
- **Scriptable** — every TUI action is also an SSH command (`ssh host pr merge acme/api 12`) and a JSON HTTP API.
- **Git-native** — plain bare repositories on disk, standard git over SSH and smart HTTP. No proprietary format.
- **CI/CD built in** — YAML pipelines in the repo, matrix builds, `needs` graphs, Docker/Podman runners, streaming logs, artifacts, secrets.
- **Package registry** — container images (`docker push`) and generic files, with repository permissions.
- **Git LFS** — per repository, over HTTP and SSH, fetched automatically in CI.
- **Wiki** — Markdown pages per repository, rendered in the terminal, stored in a cloneable git repo.
- **Code search** — across all your repositories or one, with path/repo filters and regexes.
- **Mirrors** — pull mirrors of GitHub & co., push mirrors to them, over https or ssh.
- **Branch protection** — no force pushes or deletion, pull-request-only branches, required approvals and checks.
- **Small** — one Go binary, one SQLite file. No Kubernetes, no Redis, no message queue.

## Quick start

```sh
go build -o forge ./cmd/forge        # needs Go ≥ 1.26 and git ≥ 2.38 on the server

./forge serve                        # SSH :23231, HTTP :23232, data in ./data
ssh -p 23231 localhost               # first key to connect registers and becomes admin
```

Or bootstrap the admin non-interactively:

```sh
FORGE_INITIAL_ADMIN_KEYS="$(cat ~/.ssh/id_ed25519.pub)" ./forge serve
# or, against the data dir directly:
./forge admin user create --admin alice "$(cat ~/.ssh/id_ed25519.pub)"
```

Push an existing project (pushing to a repo that doesn't exist creates it, private by default):

```sh
git remote add forge ssh://localhost:23231/alice/myproject.git
git push forge main
```

Start a CI runner on any machine with Docker or Podman:

```sh
ssh -p 23231 localhost runner create builder-1                # prints a token (admin only)
./forge runner --server http://localhost:23232 --token fgr_…   # add --concurrency 4 for parallel jobs
```

## Using the TUI

| Key | Action |
| --- | --- |
| `↑↓` / `j k` | move · `ctrl+u/d` half page |
| `enter` | open |
| `tab` / `shift+tab`, `1`–`9`, `0` | switch tabs |
| `/` | filter the current list (space-separated terms) |
| `esc` | clear filter / go back · `H` home · `q` back/quit |
| `r` | refresh · `?` help for the current screen |

Screens and their main actions:

- **Home** — Repositories (`n` new, `m` new mirror), Search (code in all repositories, `S` from anywhere), Activity (your contribution graph of the last year above the feed; `ssh host activity [user]` prints anyone's, `GET /api/v1/users/{name}/heatmap` returns it as JSON), Packages, Inbox (`m` mark read), Orgs (members, org-wide CI secrets), Account (SSH keys, API tokens, `s` CI secrets for all your repos, `e` profile, `R` rename), and for admins Runners and Users (`R` rename).
- **Repository** — Code (browse tree with syntax highlighting; a directory's README is rendered below its files, `v` reads all of it; `b` switch branch/tag, open files with line numbers, `L` file history), Commits (diff view, `R` revert a commit, `P` cherry-pick it onto another branch; both straight onto the branch or as a pull request), Branches (`n` new branch from the selected one, `p` open PR from branch, `d` delete; ★ default, 🔒 protected), Tags (`n` new tag, with a message for an annotated tag; ● has a release, `d` delete), Issues, Board (kanban: `←→↑↓` select, `H`/`L` move a card between columns, `K`/`J` reorder, `n` new issue in a column, `a`/`E`/`D`/`<`/`>` add, rename, delete, move columns; moving a card into the last column closes the issue, moving it out reopens it), Pull requests (`s` cycle open/closed/all; in a PR the Revisions tab shows what changed between pushes, incl. jj rewrites; in a merged PR `R` reverts it, `P` cherry-picks it onto another branch (backports)), Wiki (`n` new page, `enter` read; on a page `e` edit, `o` follow a link, `h` history), Pipelines (`t` trigger, `A` approve, `c` cancel, `R` rerun), Releases, Packages, Activity, Settings (Git LFS usage; admins: `e` edit incl. the Git LFS checkbox, `p` protect a branch (`enter` edits a rule), `M` add a push mirror, `y` sync a mirror, `K` its deploy key, `c` collaborators, `s` secrets, `R` rename, `X` delete). `w` watch, `f` fork, `S` search the repository's code anywhere in a repo.
- **Issue / PR** — conversation with `c` comment, `e` edit, `L` labels, `x` close/reopen; PRs add `A` approve, `C` request changes, `m` merge (merge / squash / fast-forward), plus Commits, Files (`v` line review comment) and Checks tabs.
- **Pipeline run** — job list beside the live log (stacked on narrow terminals). `tab` switches focus, `f` toggles follow, `A` approve a held run, `c` cancel, `R` rerun all, `F` rerun failed, `a` artifacts, `s` resolved job spec.

Light terminal? `ssh -o SetEnv=FORGE_THEME=light …` (also honours `COLORFGBG`).

## SSH command line

```sh
ssh -p 23231 host help
ssh -p 23231 host repo create --private -d "API server" acme/api
ssh -p 23231 host 'issue create -t "Crash on start" -b "Steps: …" acme/api'
ssh -p 23231 host 'issue move acme/api 12 "In progress"'
ssh -p 23231 host 'board view acme/api'
ssh -p 23231 host repo branch acme/api hotfix v1.2.0              # new branch from a tag (or branch/commit)
ssh -p 23231 host 'repo tag -m "Stable" acme/api v1.3.0 main'    # annotated tag; without -m lightweight
ssh -p 23231 host repo revert acme/api 3f2a9c1                   # undo a commit on the default branch (-branch b, -pr)
ssh -p 23231 host pr revert --pr acme/api 4                     # undo a merged PR through a new PR
ssh -p 23231 host repo pick --branch release/1.x acme/api 3f2a9c1   # cherry-pick a commit (keeps its author)
ssh -p 23231 host pr pick --pr --branch release/1.x acme/api 4      # backport a merged PR through a new PR
ssh -p 23231 host pr create acme/api my-branch           # from a fork: targets the parent
ssh -p 23231 host pr view acme/api 4
ssh -p 23231 host pr revisions acme/api 4                       # every push, incl. force pushes / jj rewrites
ssh -p 23231 host pr interdiff -p acme/api 4 2 3                # what changed from revision 2 to 3
ssh -p 23231 host pr merge --method squash acme/api 4
ssh -p 23231 host ci runs acme/api
ssh -p 23231 host ci logs -f acme/api 17                 # stream logs of all jobs of run #17
ssh -p 23231 host ci approve acme/api 18                 # run a held PR from a fork
ssh -p 23231 host ci trigger -p deploy acme/api v1.2.0
echo -n "$TOKEN" | ssh -p 23231 host secret set acme/api DEPLOY_TOKEN
echo -n "$KEY"   | ssh -p 23231 host secret set acme REGISTRY_KEY   # org-wide: every acme/* repo
ssh -p 23231 host ci artifact acme/api 9 > build.tar.gz
ssh -p 23231 host user rename andy                       # rename yourself (or: user rename acme acme-corp)
```

OpenSSH joins remote arguments with spaces, so quote multi-word values twice as shown.
`-b -` reads a body from stdin.

## Runners on other machines

The server never runs jobs itself; it only schedules them. Runners are separate `forge runner`
processes that can live anywhere that can reach the server's HTTP port — a build box, your
laptop, an ARM board, a cloud VM. They connect **out** to the server, so they work behind NAT
and firewalls with no inbound ports.

```sh
# as an admin: register the runner and note the token
ssh -p 23231 git.example.com runner create build-box

# on the build machine: copy the forge binary over, then
forge runner --server https://git.example.com --token fgr_… --labels gpu --concurrency 4
```

- The runner fetches jobs, clones and uploads artifacts all through `--server`, so the address you
  give it is the only one that has to work from that machine.
- **Labels** route jobs: a job with `runs-on: [gpu]` only goes to runners with the `gpu` label.
  A runner's labels are its executor, OS and architecture (e.g. `docker`, `linux`, `arm64`) plus
  anything in `--labels`; they are reported when it connects, replacing any given to
  `runner create`. Jobs without `runs-on` go to any runner.
- **Executors**: `--executor docker` (default) runs each job in a container, using Docker or Podman
  (detected automatically; force one with `--docker podman`). `--executor shell` runs on the host
  with no isolation, for trusted machines only.
- Run as many runners as you like; each job goes to exactly one. `ssh host runner list` shows which
  are online, `runner disable NAME` stops new jobs going to one.
- Use `https://` for `--server` when the runner is outside a trusted network: the runner token,
  clone tokens and secrets travel over this connection.
- To run it as a service, see [`deploy/forge-runner.service`](deploy/forge-runner.service).

## Pipelines

Put a pipeline in `.forge.yml` or any number in `.forge/pipelines/*.yml`:

```yaml
name: ci

on:                    # omit `on` to run on every push, PR and manual trigger
  push:
    branches: [main, "release/*"]
    tags: ["v*"]
    paths: ["src/**", "go.mod"]   # optional
  pull_request:
    branches: [main]              # matches the PR's target branch
  manual: true                    # allow `t` in the TUI / `ci trigger`
  schedule:                       # on the default branch, at cron times
    - cron: "0 18 * * *"          # minute hour day month weekday; @daily, @hourly … work too
      timezone: Europe/Vienna     # optional, default UTC

env:
  CGO_ENABLED: "0"

jobs:
  lint:
    image: golang:1.23
    steps:
      - go vet ./...              # a bare string is a step

  test:
    image: golang:${{ matrix.go }}
    needs: lint
    matrix:
      go: ["1.22", "1.23"]       # cartesian product of all keys
    steps:
      - name: unit tests
        run: go test ./...

  build:
    image: golang:1.23
    needs: [test]
    runs-on: [docker, amd64]     # only runners with all these labels
    timeout: 20m
    secrets: [DEPLOY_TOKEN]      # exposed as env vars, masked in logs
    steps:
      - go build -o dist/app ./cmd/app
    artifacts: [dist/*]          # files, or directories (uploaded as .tar.gz)

  notify:
    image: alpine
    needs: build
    if: always                   # run even if dependencies failed
    steps: ["echo done"]
```

Each job runs in one container (state carries over between steps) with the repository checked out
at `/workspace`. Steps run with `sh -e`. Jobs get `CI=true` and `FORGE_REPO`, `FORGE_REF`,
`FORGE_BRANCH`, `FORGE_TAG`, `FORGE_SHA`, `FORGE_TRIGGER`, `FORGE_PIPELINE`, `FORGE_RUN_NUMBER`,
`FORGE_JOB`, `FORGE_PR_NUMBER`, `FORGE_ACTOR`, `FORGE_SERVER_URL`, `FORGE_REGISTRY`, `FORGE_TOKEN`
(a job-scoped token, see [Packages](#packages)), plus `MATRIX_<KEY>` for matrix
values. `${{ matrix.key }}` is interpolated in `image`, names, `env` and `run`.

Scheduled runs use the pipeline as it is on the default branch, with `FORGE_TRIGGER=schedule`.
They are not caught up after downtime (like cron), and forks don't run their parent's schedules.

**Building images.** Jobs normally have no Docker. Start a runner with
`--docker-socket /var/run/docker.sock` (`FORGE_RUNNER_DOCKER_SOCKET`) to mount the host's Docker
into its jobs; it gets the label `docker-socket`, so such jobs say `runs-on: docker-socket` and use
e.g. `image: docker:27-cli` for `docker build` / `docker buildx`. This gives jobs root on that
machine — only enable it on runners for code you trust. Jobs from fork pull requests never get it.

Validate without pushing a commit to `main`: `ssh host ci lint acme/api my-branch`.
A broken pipeline file shows up as an errored run with the parse error.

### Secrets

- **Scopes**: repository secrets (repo admins), and user/organization secrets that every
  repository of that owner inherits (the user, or the org's owners). A repository secret overrides
  an inherited one with the same name. Manage them in the TUI (repo Settings, Orgs › Secrets,
  Account), with `ssh host secret set|list|rm owner/name|owner …`, or the API.
- **Names** are upper-cased: letters, digits and `_`, not starting with a digit. `CI`, `FORGE_*` and
  `MATRIX_*` are reserved.
- **Least exposure**: a job receives only the secrets it lists under `secrets:`, as environment
  variables, masked as `***` in logs. Values can be written and deleted, never read back.
  A listed secret that isn't defined anywhere produces a warning in the job log.
- **Fork PRs** run the pipeline **without secrets**, and their job token can't push packages.

### Mirrors

**Pull mirrors** keep a forge repository as a copy of a remote one: Home › Repositories › `m`
(or `ssh host mirror create --interval 1h https://github.com/owner/repo.git`). forge fetches every
interval (10m to 1d), copies branches and tags exactly (deleted ones disappear too) and follows the
remote's default branch. A pull mirror is read-only; pipelines don't run for mirrored commits
unless you tick that. To stop mirroring, delete the mirror in Settings: the repository keeps its
contents and becomes a normal one.

**Push mirrors** copy a repository to remotes after every push (and optionally on an interval):
Settings › `M`, or `ssh host mirror push acme/api https://github.com/acme/api.git --user me --token -`.
They push branches and tags and delete remote branches forge doesn't have.

- **https remotes**: give a username and a password or token (GitHub/GitLab/Gitea/Codeberg tokens
  work); it's stored encrypted like CI secrets and sent only to that host.
- **ssh remotes** (`git@github.com:acme/api.git`): forge creates a deploy key for the mirror; `K`
  (or `mirror key`) shows it. Add it to the remote (with write access for push mirrors), then `y`.
- Status and errors show in Settings and, for pull mirrors, in the repository header;
  `ssh host mirror list acme/api` and `mirror sync` do the same from scripts.
- Remotes on this machine (localhost) are refused unless `mirror.allow_loopback` is set, and so
  are link-local addresses (cloud metadata), local paths and `file://`.
- Not mirrored: LFS objects, the wiki, issues and pull requests (git data only).

### Branch protection

Protect branches in **Settings › `p`** (repository admins), or with
`ssh host protect set --push none --approvals 1 --checks ci acme/api main`. A rule covers a branch
name or a pattern (`release/*`) and decides:

| Setting | Default | |
|---|---|---|
| Direct pushes | everyone with write access | or *admins only*, or *nobody* (changes only through pull requests) |
| Required approvals | 0 | approvals needed to merge a pull request; the author's own doesn't count |
| Required checks | none | pipelines that must have passed on the pull request's latest commit (`*` = all) |
| Block on changes | on | no merging while a reviewer's latest review requests changes |
| Force pushes | not allowed | rewriting the branch's history |
| Deletion | not allowed | deleting the branch (push `:branch` or `d` in Branches) |
| Admins bypass | off | repository admins aren't bound by the rule |

Pushes over SSH and HTTP are checked before anything is written: forge runs the push with a
pre-receive hook (the forge binary itself), and a rejected push changes nothing and tells you why:

```
remote: forge: ✗ main is protected: changes must go through a pull request
remote: forge:   push to another branch and open a pull request: ssh host pr create acme/api <branch>
```

Merges (TUI `m`, `pr merge`, API) check the approvals and checks; the pull request shows what's
still missing. Protected branches have a 🔒 in the Branches tab. Repositories without rules are pushed
to without any hook. If several rules match a branch, the strictest setting of each wins.

### Pull requests from forks need approval

When a pull request comes from a fork and the person who pushed the code has no write access to
the target repository, its runs start as **needs approval**: no job is queued and no runner sees
them until someone with write access approves — `A` on the run (or in the Pipelines tab), `ssh host
ci approve owner/name N`, or `POST …/runs/N/approve`. Review the change first: approving runs that
code on your runners (still without secrets). Every new push to the PR needs a new approval; it
replaces the run still waiting. Collaborators with write access are trusted and run right away,
as do pushes to a fork's own pipelines in the fork.

- **Encrypted at rest** with AES-256-GCM, each value bound to its repository/owner and name.
  See [Security notes](#security-notes) for where the key lives.

## Jujutsu (jj)

forge is a plain git server, so [jj](https://jj-vcs.dev) works with it out of the box. It also
understands how jj works:

```sh
jj git clone ssh://git@host:23231/acme/api.git
jj git push -c @-                       # push the change as bookmark push-<change id>
ssh -p 23231 host pr create acme/api push-ztlmmotoltnk
# …amend, squash, rebase, then just push again:
jj git push
ssh -p 23231 host pr interdiff acme/api 4      # what changed since the last push
```

- **Change IDs**: commits jj made show their change ID (in purple) in the Commits tab, the PR
  Commits tab, the commit view and the API (`change_id`).
- **Revisions**: every head a pull request has had is kept (`refs/pull/N/rev/K`, safe from gc).
  The PR's Revisions tab (`enter`: since the previous push, `a`: since the first) and
  `pr interdiff` / `GET …/pulls/{n}/interdiff` show what changed between two pushes, per change:
  `unchanged`, `rebased`, `reworded`, `rewritten` (with the diff of just that change), `new` or
  `dropped`, plus the whole PR with the old revision replayed onto the new base, so a rebase
  doesn't drown the real changes. Changes are matched by change ID; plain git commits by identical
  patch.
- **Stacked pull requests**: open a PR whose base is another PR's branch (e.g. `push-b` onto
  `push-a`). The PR shows the stack (`#3 ← #4 ← #5`), and when the lower one is merged the PRs
  stacked on it are retargeted to its base. Rebase the rest of the stack with jj and push.
- Rewriting history is a force push to git; branch protection rules about force pushes apply as
  usual (bookmarks for PRs are normally unprotected).
- `jj abandon` deletes bookmarks pointing at the abandoned change, so pushing afterwards deletes
  the PR's branch; use `jj abandon --retain-bookmarks` to keep it.

## API tokens

Tokens authenticate git over HTTPS (as the password), the API, LFS and the package registry.
Create them in the TUI (Home › Account › `t`) or over SSH. Each token has:

- **scopes**: `code` (git, files, releases, wiki, search), `issues` (issues, pull requests,
  reviews, board, notifications), `ci` (runs, logs, artifacts) and `packages`, each `:read` or
  `:write`, plus `admin` (repository and owner settings, secrets, mirrors, protection, creating
  and deleting repositories) and, for site admins, `site-admin`. `*` is full access: everything
  you can do, including scopes added in later versions.
- **repositories** (optional): `owner/name` or `owner/*`. Outside them a token gets only what an
  anonymous client gets.
- **an expiry** (optional): a lifetime, a date, or never. Expired tokens are refused with
  "token expired" and shown in red in the Account tab.

```sh
ssh -p 23231 host token create --scopes code:read,packages:write --repos acme/api --expires 90d ci-deploy
ssh -p 23231 host token create --scopes '*' --expires 1y laptop        # the super token
ssh -p 23231 host token list
ssh -p 23231 host token rm 3
```

A token can never do more than its user. Tokens created before scopes existed keep full access
and never expire. Tokens can't create tokens.

## Packages

forge includes a package registry: **container images** (anything that speaks the OCI/Docker
registry API — docker, podman, buildx, skopeo, crane) and **generic files** (any file, by name and
version, over plain HTTP). Content is deduplicated in one blob store under `data/packages/`.

**Ownership and permissions.** Packages belong to a user or organization. A package named after
one of the owner's repositories — `acme/api`, or `acme/api/worker` — is *linked* to it and uses its
permissions: pull needs read (so public repos give public packages), push needs write, and the
repository's CI jobs can push. A package without such a repository is private to its owner (the
user, or the org's members).

```sh
# containers: log in with your username and an API token (Account tab → t, or `ssh host token create`)
docker login git.example.com -u alice
docker push git.example.com/acme/api:1.4.0
docker pull git.example.com/acme/api:1.4.0

# generic files: PUT to upload (immutable: delete first to replace), GET to download
curl -u alice:$TOKEN -T app.tar.gz https://git.example.com/api/packages/acme/generic/api/1.4.0/app.tar.gz
curl -LO https://git.example.com/api/packages/acme/generic/api/1.4.0/app.tar.gz
```

Browse them in the TUI (Home › Packages, or a repository's Packages tab: `d` delete a tag/file,
`D` a version, `X` the package) or with `ssh host package list|show|rm`.

**From CI**, every job gets `FORGE_REGISTRY` (host:port) and `FORGE_TOKEN` (valid only while the job
runs, masked in logs). It can push packages linked to the job's own repository — not to others, and
not at all in pull requests from forks:

```yaml
jobs:
  image:
    image: docker:cli
    runs-on: [shell, docker-host]      # building images needs a Docker daemon: a shell runner
    steps:
      - |
        export DOCKER_CONFIG="$FORGE_WORKSPACE/.docker"
        echo "$FORGE_TOKEN" | docker login "$FORGE_REGISTRY" -u forge-job --password-stdin
        IMAGE="$FORGE_REGISTRY/$(echo $FORGE_REPO | tr A-Z a-z)"
        docker build -t "$IMAGE:$FORGE_SHA" . && docker push "$IMAGE:$FORGE_SHA"
      - curl -fsS -u "forge-job:$FORGE_TOKEN" -T dist/app "$FORGE_SERVER_URL/api/packages/acme/generic/app/$FORGE_TAG/app"
```

**Notes**

- Docker only talks plain HTTP to `localhost`/`127.0.0.1`. Elsewhere, put the HTTP port behind a TLS
  proxy (it must be served at the root of a host name, not under a path), or list it under
  `insecure-registries` in the Docker daemon config of each client.
- Image names must be lower case (`acme/api`, even if the repository is `Acme/API`).
- Deleting a tag or package frees space at the next garbage collection (hourly, or
  `ssh host package gc` as admin). Untagged manifests stay pullable by digest until deleted.
- Not yet: other formats (npm, PyPI, Maven, Cargo, Go modules), quotas, retention policies.

## Code search

Search the code of every repository you can read: **Home › Search** (or `S` anywhere on the
home screen), or just one repository with `S` inside it. Type the query, `enter` to search,
`enter` on a match opens the file at that line; `/` edits the query again.

| Query | Finds |
|---|---|
| `listen port` | the phrase "listen port", any case |
| `ListenAndServe` | case-sensitive as soon as the query has capitals |
| `"a  b"` | a phrase with exact spacing |
| `/Listen(And)?Serve\(/` | a regular expression (extended syntax) |
| `path:*.go` `path:docs/` | only matching files / folders (repeatable) |
| `repo:api` `owner:acme` | only some repositories |
| `case:yes` `fork:yes` | force case-sensitivity / include forks (skipped otherwise) |

It searches each repository's default branch with `git grep`, so there is no index to build
and results are always current. Results stop at 100 matches per repository and 300 in total,
so narrow it down with `path:` or `repo:` when the limit is reached. From scripts:
`ssh host search code -n 50 'TODO path:*.go'` (grep-like output) or `GET /api/v1/search/code?q=`.

## Wiki

Every repository has a wiki: Markdown pages, rendered in the TUI (headings, lists, task lists,
tables, quotes, code blocks with syntax highlighting). Open the **Wiki** tab, press `n` to write
the Home page. On a page: `e` edit, `o` follow a link, `h` history (and older versions), `d` delete.

- Link pages with `[[Page Name]]`, `[[Page Name|link text]]` or `[text](Page-Name)`. A link to a
  page that doesn't exist yet opens the editor for it.
- Page names may contain spaces and folders: "Getting Started" is `Getting-Started.md`,
  "guides/Setup" is `guides/Setup.md`.
- **It's a git repository.** Clone it, edit pages in your editor, push:

  ```sh
  git clone ssh://host:23231/acme/api.wiki.git
  ```

  Pushes to the wiki never start pipelines.
- Readers of the repository can read the wiki, writers can edit it. Each save is a commit by you;
  if someone else saved the wiki while you were editing, your save is refused instead of
  overwriting their change.
- From scripts: `ssh host wiki edit acme/api "Getting Started" < page.md`, `wiki view`,
  `wiki list`, `wiki log`, `wiki rm`, or the `/wiki` API endpoints.

Issue and pull request descriptions and comments are rendered as Markdown too.

## Git LFS

Large files can be stored with [Git LFS](https://git-lfs.com). It is switched on per repository:
tick **Git LFS** when creating the repository (TUI `n`, or `repo create -lfs`), or later in
Settings › `e` (or `ssh host repo edit --lfs true acme/media`, or `PATCH .../repos/{owner}/{repo}
{"lfs": true}`). Then use git-lfs as usual:

```sh
git lfs install                 # once per machine
git lfs track "*.psd" "*.mp4"   # writes .gitattributes; commit it
git add .gitattributes art.psd && git commit -m "add art" && git push
```

- **Both remotes work.** Over SSH, forge answers `git-lfs-authenticate` with a short-lived token,
  so your SSH key is all you need. Over HTTPS, the same token you use for git works.
- **Permissions** follow the repository: readers download, writers upload. Anonymous downloads of
  public repositories follow `anonymous_read`.
- **Storage.** Objects are stored once under `data/lfs/` by content hash. A repository serves only
  objects uploaded to it, so knowing a file's hash isn't enough to read it from another
  repository. Forks share their parent's objects. Merging a pull request from a fork makes its LFS
  files available in the target repository. Objects no repository uses any more are deleted
  together with the last repository that had them.
- **Turning it off** refuses new uploads; files already stored stay downloadable, so existing
  history keeps working. The Settings tab shows how many objects and bytes the repository holds.
- **CI.** Runners fetch LFS files right after checkout (the runner image includes git-lfs; for
  the `shell` executor install git-lfs on the host).
- **TUI.** The code view shows the real content of an LFS file, marked with "◆ Git LFS", instead
  of the pointer file.
- Limits: objects up to `lfs.max_object_mb` (default 5120). File locking (`git lfs lock`) is not
  supported yet.

## Architecture

```
                         ┌──────────────────────── forge serve ──────────────────────────┐
  ssh ── TUI / CLI ─────▶│ sshd ──┐                                                       │
  git (ssh)  ───────────▶│        ├─▶ forge (service layer: permissions, PRs, merges,     │
  git (https) ──────────▶│ web  ──┘     events, push handling) ──▶ store (SQLite)         │
  scripts (REST) ───────▶│                    │                     gitx  (git CLI, bare  │
                         │                    ▼                            repos on disk) │
                         │            ci.Orchestrator  ──▶ logs (files + live pub/sub)   │
                         └───────────────────▲───────────────────────────────────────────┘
                                             │ HTTP: claim (long-poll), log, heartbeat,
                                             │       finish, artifacts
                              forge runner × N  (docker/podman or shell executor)
```

- **Orchestration vs. execution**: the server decides what runs and when (trigger matching,
  matrix expansion, dependency graph, cancellation, stale-runner reaping). Runners are stateless
  pull-based agents: they claim a job, clone with a short-lived job-scoped token, run the steps,
  stream logs, heartbeat (which is also how cancellation reaches them), upload artifacts, report.
- **Git** is handled by the real `git` binary (`upload-pack`, `receive-pack`, `http-backend`,
  `merge-tree`), so repositories are byte-for-byte standard. Refs are diffed around each push to
  record activity, update PRs and trigger pipelines — no hook scripts to install.
  PR heads are mirrored to `refs/pull/N/head`, which clients cannot push to.
- **Permissions**: site admin › owner / org owner › collaborator role (read, write, admin) ›
  org member (write) › public read. Private repositories are invisible to people without access.

Code layout: `cmd/forge` (binary), `internal/{store,gitx,forge,ci,runner,sshd,web,tui,config}`.
API reference: [`/api/v1/docs`](internal/web/docs.go).

## Configuration

`forge config > forge.yaml` prints all options. Everything can also be set through the
environment, which overrides the file, so a config file is optional (e.g. in containers):

| Variable | Config key |
|---|---|
| `FORGE_CONFIG` | config file path (instead of `-c`) |
| `FORGE_NAME`, `FORGE_DATA_DIR` | `name`, `data_dir` |
| `FORGE_SSH_LISTEN`, `FORGE_HTTP_LISTEN` | `ssh.listen`, `http.listen` — where the server binds, e.g. `:23231` |
| `FORGE_SSH_PUBLIC_ADDR`, `FORGE_HTTP_PUBLIC_URL` | `ssh.public_addr`, `http.public_url` — what users see in clone URLs |
| `FORGE_SSH_IDLE_TIMEOUT` | `ssh.idle_timeout` (seconds) |
| `FORGE_ALLOW_REGISTRATION`, `FORGE_ANONYMOUS_READ` | `allow_registration`, `anonymous_read` |
| `FORGE_INITIAL_ADMIN_KEYS`, `FORGE_INITIAL_ADMIN_NAME` | `initial_admin_keys` (one key per line), `initial_admin_name` |
| `FORGE_SECRET_KEY_FILE`, `FORGE_SECRET_KEY` | `secret_key_file`, or the key itself |
| `FORGE_PACKAGES_ENABLED` | `packages.enabled` |
| `FORGE_CI_STALE_SECONDS`, `FORGE_CI_MAX_ARTIFACT_MB` | `ci.stale_seconds`, `ci.max_artifact_mb` |
| `FORGE_LFS_MAX_OBJECT_MB` | `lfs.max_object_mb` |
| `FORGE_MIRROR_ALLOW_LOOPBACK` | `mirror.allow_loopback` |
| `FORGE_LOG_FORMAT`, `FORGE_LOG_LEVEL` | `log.format`, `log.level` |

Invalid values (`FORGE_ANONYMOUS_READ=nope`) stop startup with an error instead of being ignored.
The runner has its own variables, one per flag: `FORGE_RUNNER_SERVER`, `FORGE_RUNNER_TOKEN`,
`FORGE_RUNNER_LABELS`, `FORGE_RUNNER_EXECUTOR`, `FORGE_RUNNER_CONCURRENCY`, `FORGE_RUNNER_WORKDIR`,
`FORGE_RUNNER_DOCKER`, `FORGE_RUNNER_NETWORK` (Docker network for job containers) and
`FORGE_RUNNER_REGISTRY` (registry host jobs push to, if not the `--server` host).

**Logging** goes to stderr, either human-readable or as JSON lines (for journald, Docker, Loki, …):

```yaml
log:
  format: pretty   # or json
  level: info      # debug, info, warn, error  (-debug / FORGE_DEBUG=1 force debug)
```

The runner reads no config file; use `forge runner --log-format json --log-level warn` or the same
`FORGE_LOG_FORMAT` / `FORGE_LOG_LEVEL` variables. Pretty output is colored only on a terminal. Set `public_addr`/`public_url` to what users and runners can reach.

**Private instance.** To allow only known people in, set:

```yaml
anonymous_read: false      # FORGE_ANONYMOUS_READ=false
allow_registration: false  # the default
```

The SSH server then refuses unregistered keys at login ("Permission denied (publickey)"), and HTTP
(git, API, registry, packages) requires a token for everything. A fresh install with no users still
accepts the first key so the admin can register; or create the admin up front with
`FORGE_INITIAL_ADMIN_KEYS` / `forge admin user create`. Add people with `ssh host user create`.

**Renaming.** Users can rename themselves (`ssh host user rename NEW`, Account tab → `R`, or
`PATCH /api/v1/users/{name}`); site admins can rename anyone and org owners their org. Repositories,
packages, secrets and permissions move along, but old clone URLs, git remotes and image references
stop working — there are no redirects. The first admin is called `admin` unless you set
`initial_admin_name` (`FORGE_INITIAL_ADMIN_NAME`) before the first start.

### Docker Compose

[`deploy/docker-compose.yml`](deploy/docker-compose.yml) runs the server and a runner, configured
entirely from `deploy/.env`:

```sh
cp deploy/.env.example deploy/.env        # set ports, public addresses, your SSH key
docker compose -f deploy/docker-compose.yml up -d forge
docker compose -f deploy/docker-compose.yml exec forge forge admin runner create compose
# put the token into FORGE_RUNNER_TOKEN in deploy/.env, then
docker compose -f deploy/docker-compose.yml --profile runner up -d
```

- `FORGE_SSH_PORT` / `FORGE_HTTP_PORT` are the host ports; inside the container the server always
  listens on 23231/23232. Keep `FORGE_SSH_PUBLIC_ADDR` / `FORGE_HTTP_PUBLIC_URL` in sync with them
  (or with your reverse proxy).
- The runner starts job containers on the host's Docker daemon through the mounted socket. Jobs
  join the `forge-ci` network, so they reach the server as `forge:23232` (`FORGE_REGISTRY` is
  `forge:23232` — plain HTTP, so registry tools need their insecure flag, e.g. `crane --insecure`).
- Workspaces live in `/var/lib/forge-runner`, mounted at the same path inside the runner, because
  job containers bind-mount them from the host.
- Mounting the Docker socket gives the runner root on the host; put it on a separate machine if
  that matters to you (point `FORGE_RUNNER_SERVER` at the public URL).
- Images run as non-root may not be able to write to the workspace, which belongs to root.

Other deployment examples: [`deploy/`](deploy) (systemd units, Dockerfile). Back up by copying the data
directory (stop the server, or use `sqlite3 data/forge.db .backup` for a hot copy of the DB).

## Security notes

- Terminate TLS for the HTTP port with a reverse proxy (Caddy, nginx) in production; tokens travel in headers.
- CI secrets are encrypted in the database. The key comes from `FORGE_SECRET_KEY`, else
  `secret_key_file`, else `data/secret.key` (created on first start). With the default, the key sits
  next to the database, so encryption only helps against a leaked copy of `forge.db` on its own.
  For real separation keep the key elsewhere and out of data-dir backups:

  ```sh
  openssl rand -base64 32 > /etc/forge/secret.key && chmod 600 /etc/forge/secret.key
  # forge.yaml: secret_key_file: /etc/forge/secret.key
  forge admin rekey --old-key-file data/secret.key   # re-encrypt existing secrets under the new key
  ```

  Losing the key means re-entering the secrets. On startup the server encrypts any plaintext
  secrets left from older versions, and logs an error if values were encrypted with a different key.
  `data/secret.key` also signs job clone tokens, so keep it either way.
- The Docker executor gives jobs a container, not a VM; jobs can use whatever the runner's Docker daemon allows.
  The `shell` executor has **no isolation** — use it only on dedicated, trusted machines.

## Status / not yet implemented

Web UI (intentionally optional), webhooks, email notifications,
service containers for jobs, caches between runs, LFS file locking, LFS in mirrors, OIDC/LDAP login.

## Development

```sh
make build    # bin/forge for this machine (server and runner are the same binary)
make dist     # cross-compile into dist/: linux, macOS (amd64/arm64), windows/amd64
make test     # unit + end-to-end tests (real git, SSH, HTTP and a shell-executor runner)
make run      # serve with ./data
```
