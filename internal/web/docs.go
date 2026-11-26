package web

// APIDocs is served at /api/v1/docs.
const APIDocs = `# Forge HTTP API v1

Authenticate with a personal access token (create one in the TUI under
Home › Account, or with ` + "`ssh <host> token create <name>`" + `):

    Authorization: Bearer fgp_xxxxxxxx

Tokens have scopes (code, issues, ci, packages as :read or :write; admin;
site-admin; or * for everything), an optional list of repositories and an
optional expiry. Requests outside them get 403 "this token lacks the … scope",
or for other repositories what an anonymous client gets. Expired tokens get 401.

Anonymous requests can read public repositories when anonymous_read is enabled.
Lists that accept ?page= (from 1) and ?limit= (1…1000) return everything
without them and always set X-Total-Count. All bodies are JSON. Errors are {"error": "..."} with a 4xx/5xx status.

## Users
    GET    /api/v1/user                         current user
    PATCH  /api/v1/users/{name}                 rename a user or org: {"name": "new"}
    GET    /api/v1/users/{name}
    GET    /api/v1/users/{name}/heatmap         contributions of the last year in 15-minute buckets:
                                                [{timestamp, contributions}] (readable repos only)
    GET    /api/v1/notifications
    GET    /api/v1/user/orgs                    organizations you belong to, with your role
                                                (site admins: ?all=true for every organization)
    POST   /api/v1/orgs                         {name, full_name?}  you become its owner

## Code search
    GET    /api/v1/search/code?q=&repo=owner/name&limit=
                                                matches on each repository's default branch,
                                                in all repositories you can read (or just repo)
    q: words are one phrase (case-insensitive unless it has capitals); "quoted phrase";
       /regex/; filters path:*.go path:docs/ repo:name owner:acme case:yes fork:yes

## Repositories
    GET    /api/v1/repos                        repositories visible to you; ?owner=user-or-org, ?q=text
                                                (name or description), ?page=&limit= (X-Total-Count)
                                                each has owner_org, ssh_url, http_url, has_wiki and,
                                                with a wiki, wiki_ssh_url / wiki_http_url
    POST   /api/v1/repos                        {owner?, name, description?, private?, lfs?}
                                                with "mirror": {url, username?, password?, interval?,
                                                run_ci?} it creates a read-only pull mirror
    GET    /api/v1/repos/{owner}/{repo}
    PATCH  /api/v1/repos/{owner}/{repo}         {description?, private?, default_branch?, name?, lfs?}
    DELETE /api/v1/repos/{owner}/{repo}
    POST   /api/v1/repos/{owner}/{repo}/forks   {owner?}
    GET    .../branches | .../tags
    POST   .../branches                         {name, from?}   from: branch, tag or commit (default branch)
    POST   .../tags                             {name, target?, message?}   message: annotated tag
    GET    .../commits?ref=&path=&limit=&skip=
    GET    .../commits/{sha}                    commit + per-file patches
    POST   .../commits/{sha}/revert             {branch?, pull_request?}  undo a commit with a new one
                                                (pull_request: on a new branch, with a PR)
    POST   .../commits/{sha}/cherry-pick        {branch, pull_request?}  apply a commit to branch
    GET    .../tree?ref=&path=
    GET    .../raw?ref=&path=                   file content
    GET    .../events                           activity
    GET    .../collaborators
    PUT    .../collaborators/{user}             {role: read|write|admin}
    DELETE .../collaborators/{user}

## Mirrors (repo admin)
    GET    .../mirrors                          pull mirror and push mirrors with their state
    POST   .../mirrors                          {url, username?, password?, interval?}  add a push mirror
    PATCH  .../mirrors/{id}                     {url?, username?, password?, interval?, run_ci?}
    DELETE .../mirrors/{id}                     removing the pull mirror makes the repo writable
    POST   .../mirrors/{id}/sync                sync now (202; the result shows up in GET)

    Remotes: https/http (credentials stored encrypted) or ssh (git@host:x/y.git; forge
    generates a deploy key, returned as deploy_key). interval: 10m … 30d; push mirrors
    default to 0 = only after pushes. Mirrors copy refs/heads and refs/tags exactly.

## Branch protection
    GET    .../protections                      rules (readers)
    PUT    .../protections/{pattern}            {push: write|admin|none, required_approvals,
                                                 required_checks: ["ci"] or ["*"], allow_force_push,
                                                 allow_delete, block_on_requested_changes,
                                                 admins_bypass}   (repo admin)
    DELETE .../protections/{pattern}            (repo admin)

    {pattern} is a branch name or glob (main, release/*). Pushes over SSH and HTTP,
    pull request merges and branch deletion all follow the rules.

## Issues
    GET    .../issues?state=open|closed|all&q=&page=&limit=
    POST   .../issues                           {title, body}
    GET    .../issues/{n}
    PATCH  .../issues/{n}                       {title?, body?, state?, labels?}
    GET    .../issues/{n}/comments
    POST   .../issues/{n}/comments              {body}
    POST   .../issues/{n}/move                  {column: "name" | "2", index?}  move on the board

## Wiki
    GET    .../wiki                             pages (name, path, last author, updated_at)
    GET    .../wiki/{page}?rev=                 one page with its Markdown; rev = an older commit
    PUT    .../wiki/{page}                      {content, message?, name? (rename), expect?}
                                                creates (201) or replaces (200) the page; expect =
                                                wiki_head from the GET, refused if someone saved since
    DELETE .../wiki/{page}

    Page names may contain spaces and folders: "Getting Started", "guides/Setup".
    The wiki is also a git repository: clone {owner}/{repo}.wiki.git over SSH or HTTP.

## Board (kanban)
    GET    .../board                            columns with their issues; the last
                                                column ("done": true) holds closed ones
    POST   .../board/columns                    {name}  added before the last column
    PATCH  .../board/columns/{id}               {name?, position?}  (1-based)
    DELETE .../board/columns/{id}               its cards go back to the first column

    Moving an issue into the last column closes it, moving it out reopens it.
    Columns need write access; issue authors may move their own issues.

## Pull requests
    GET    .../pulls?state=
    POST   .../pulls                            {head: "branch" | "owner/fork:branch", base?, title?, body?}
    GET    .../pulls/{n}                        includes mergeability, files, reviews, checks
    PATCH  .../pulls/{n}                        {title?, body?, state?, labels?}
    POST   .../pulls/{n}/comments               {body, path?, line?}
    POST   .../pulls/{n}/reviews                {event: approve|changes|comment, body}
    POST   .../pulls/{n}/merge                  {method: merge|squash|ff}
    GET    .../pulls/{n}/revisions              every head the PR has had (pushes, force pushes, jj rewrites)
    GET    .../pulls/{n}/interdiff?from=&to=    what changed between two revisions (default: the last push):
                                                per change (matched by jj change ID or identical patch):
                                                unchanged|rebased|reworded|rewritten|new|dropped, plus the
                                                whole-PR diff with the old revision replayed onto the new base
    POST   .../pulls/{n}/revert                 {branch?, pull_request?}  undo a merged PR
    POST   .../pulls/{n}/cherry-pick            {branch, pull_request?}  backport a merged PR

## Git LFS
    Standard Git LFS batch API at <repo>.git/info/lfs (basic transfer). Enable it per
    repository ({"lfs": true}); see the README. Authenticate like git over HTTP.

## Releases
    GET    .../releases
    POST   .../releases                         {tag, target?, title?, notes?}

## CI/CD
    GET    .../runs?limit=
    POST   .../runs                             {ref?, pipeline?}   manual trigger
    GET    .../runs/{n}                         run + jobs
    POST   .../runs/{n}/approve                 run a held fork PR run (write access)
    POST   .../runs/{n}/cancel
    POST   .../runs/{n}/rerun?failed=true
    GET    .../runs/{n}/artifacts
    GET    .../artifacts/{id}                   download
    GET    .../jobs/{id}/log?follow=true        plain text, streams while running
    GET    .../secrets                          names only, incl. inherited (repo admin)
    PUT    .../secrets/{name}                   {value} (repo admin)
    DELETE .../secrets/{name}                   (repo admin)
    GET    /api/v1/users/{owner}/secrets        user/org secrets inherited by all its repos
    PUT    /api/v1/users/{owner}/secrets/{name} {value}  (that user, org owners, site admin)
    DELETE /api/v1/users/{owner}/secrets/{name}

    Secret names are upper-cased; letters, digits and _ (not leading digit);
    CI, FORGE_* and MATRIX_* are reserved. Values are encrypted at rest and
    never returned. A repository secret overrides an owner secret of the same name.
    GET    /api/v1/runners                      (site admin)
    POST   /api/v1/runners                      {name, labels} -> {token} (site admin)

## Packages
Container images are served by the OCI distribution API at /v2/ (docker
login with username + API token; CI jobs use forge-job + $FORGE_TOKEN).

    PUT    /api/packages/{owner}/generic/{name}/{version}/{file}   upload (body); 409 if it exists
    GET    /api/packages/{owner}/generic/{name}/{version}/{file}   download (Range supported)
    DELETE /api/packages/{owner}/generic/{name}/{version}[/{file}]
    GET    /api/v1/packages[/{owner}]                              packages you can read
    GET    /api/v1/packages/{owner}/{container|generic}/{name}     tags or files

Packages named like one of the owner's repositories use its permissions;
others are private to the owner.

## Runner protocol
Runners authenticate with their own token (Bearer fgr_...).

    POST /api/v1/runner/claim                 {labels, version, wait} -> 200 job | 204
    POST /api/v1/runner/jobs/{id}/log         raw bytes
    POST /api/v1/runner/jobs/{id}/heartbeat   -> {cancel}
    POST /api/v1/runner/jobs/{id}/finish      {status, exit_code}
    POST /api/v1/runner/jobs/{id}/artifacts?name=

## Git
    git clone http(s)://<host>/<owner>/<repo>.git
Use any username and an access token as the password for private repositories and pushes.
`
