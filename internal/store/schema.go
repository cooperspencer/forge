package store

// migrations are applied in order; each entry is run once and recorded in
// schema_migrations. Never edit an existing entry — append a new one.
var migrations = []string{
	`
CREATE TABLE users (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
	full_name   TEXT NOT NULL DEFAULT '',
	email       TEXT NOT NULL DEFAULT '',
	is_admin    INTEGER NOT NULL DEFAULT 0,
	is_org      INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);

CREATE TABLE ssh_keys (
	id          INTEGER PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title       TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL UNIQUE,
	public_key  TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE access_tokens (
	id          INTEGER PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	token_hash  TEXT NOT NULL UNIQUE,
	created_at  INTEGER NOT NULL,
	last_used   INTEGER NOT NULL DEFAULT 0
);

-- org membership: org_id references a users row with is_org=1
CREATE TABLE org_members (
	org_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role        TEXT NOT NULL DEFAULT 'member', -- owner | member
	PRIMARY KEY (org_id, user_id)
);

CREATE TABLE repos (
	id             INTEGER PRIMARY KEY,
	owner_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name           TEXT NOT NULL COLLATE NOCASE,
	description    TEXT NOT NULL DEFAULT '',
	private        INTEGER NOT NULL DEFAULT 0,
	default_branch TEXT NOT NULL DEFAULT 'main',
	fork_of        INTEGER REFERENCES repos(id) ON DELETE SET NULL,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL,
	UNIQUE (owner_id, name)
);

CREATE TABLE collaborators (
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role        TEXT NOT NULL, -- read | write | admin
	PRIMARY KEY (repo_id, user_id)
);

-- issues and pull requests share a per-repo number sequence
CREATE TABLE issues (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	number      INTEGER NOT NULL,
	author_id   INTEGER NOT NULL REFERENCES users(id),
	title       TEXT NOT NULL,
	body        TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL DEFAULT 'open', -- open | closed | merged
	is_pr       INTEGER NOT NULL DEFAULT 0,
	labels      TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	UNIQUE (repo_id, number)
);

CREATE TABLE pull_requests (
	issue_id     INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
	head_repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	head_branch  TEXT NOT NULL,
	base_branch  TEXT NOT NULL,
	head_sha     TEXT NOT NULL DEFAULT '',
	merge_sha    TEXT NOT NULL DEFAULT '',
	merged_by    INTEGER REFERENCES users(id)
);

CREATE TABLE comments (
	id          INTEGER PRIMARY KEY,
	issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	author_id   INTEGER NOT NULL REFERENCES users(id),
	body        TEXT NOT NULL,
	-- review comments (PRs only)
	review      TEXT NOT NULL DEFAULT '', -- '' | approve | changes | comment
	path        TEXT NOT NULL DEFAULT '',
	line        INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);

CREATE TABLE releases (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	tag         TEXT NOT NULL,
	title       TEXT NOT NULL,
	notes       TEXT NOT NULL DEFAULT '',
	author_id   INTEGER NOT NULL REFERENCES users(id),
	created_at  INTEGER NOT NULL,
	UNIQUE (repo_id, tag)
);

CREATE TABLE events (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER REFERENCES repos(id) ON DELETE CASCADE,
	actor_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	kind        TEXT NOT NULL,
	summary     TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX events_repo ON events(repo_id, id DESC);

CREATE TABLE watches (
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, repo_id)
);

CREATE TABLE notifications (
	id          INTEGER PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	event_id    INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	read        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX notifications_user ON notifications(user_id, read, id DESC);

-- ---------------------------------------------------------------- CI/CD

CREATE TABLE secrets (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	value       TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	UNIQUE (repo_id, name)
);

CREATE TABLE runners (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	token_hash  TEXT NOT NULL UNIQUE,
	labels      TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	last_seen   INTEGER NOT NULL DEFAULT 0,
	version     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);

CREATE TABLE pipeline_runs (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	number      INTEGER NOT NULL,
	pipeline    TEXT NOT NULL,          -- pipeline name
	file        TEXT NOT NULL,          -- path of definition
	trigger     TEXT NOT NULL,          -- push | pull_request | tag | manual
	ref         TEXT NOT NULL,
	sha         TEXT NOT NULL,
	pr_number   INTEGER NOT NULL DEFAULT 0,
	actor_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	status      TEXT NOT NULL,          -- pending | running | success | failure | cancelled | error
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE (repo_id, number)
);
CREATE INDEX pipeline_runs_repo ON pipeline_runs(repo_id, id DESC);

CREATE TABLE jobs (
	id          INTEGER PRIMARY KEY,
	run_id      INTEGER NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
	key         TEXT NOT NULL,          -- job key from yaml
	name        TEXT NOT NULL,          -- display name incl. matrix values
	spec        TEXT NOT NULL,          -- JSON-encoded resolved ci.JobSpec
	needs       TEXT NOT NULL DEFAULT '',-- comma separated job keys
	labels      TEXT NOT NULL DEFAULT '',-- comma separated runner labels required
	status      TEXT NOT NULL,          -- blocked | queued | running | success | failure | cancelled | skipped | error
	runner_id   INTEGER REFERENCES runners(id) ON DELETE SET NULL,
	exit_code   INTEGER NOT NULL DEFAULT 0,
	cancel_req  INTEGER NOT NULL DEFAULT 0,
	attempt     INTEGER NOT NULL DEFAULT 1,
	heartbeat   INTEGER NOT NULL DEFAULT 0,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX jobs_run ON jobs(run_id);
CREATE INDEX jobs_status ON jobs(status);

CREATE TABLE artifacts (
	id          INTEGER PRIMARY KEY,
	job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	size        INTEGER NOT NULL,
	path        TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
`,

	// 2: encrypted secrets, and secrets owned by a user or organization that
	// all of its repositories inherit.
	`
ALTER TABLE secrets ADD COLUMN encrypted INTEGER NOT NULL DEFAULT 0;

CREATE TABLE owner_secrets (
	id          INTEGER PRIMARY KEY,
	owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	value       TEXT NOT NULL,
	encrypted   INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	UNIQUE (owner_id, name)
);
`,
	// 3: package registry (container images and generic files). Package
	// content lives in a content-addressed blob store on disk.
	`
CREATE TABLE pkg_blobs (
	digest      TEXT PRIMARY KEY,         -- sha256:<hex>
	size        INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE packages (
	id          INTEGER PRIMARY KEY,
	owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	type        TEXT NOT NULL,            -- container | generic
	name        TEXT NOT NULL COLLATE NOCASE,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	UNIQUE (owner_id, type, name)
);

CREATE TABLE pkg_files (
	id          INTEGER PRIMARY KEY,
	package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
	version     TEXT NOT NULL,
	filename    TEXT NOT NULL,
	digest      TEXT NOT NULL,
	size        INTEGER NOT NULL,
	creator     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	UNIQUE (package_id, version, filename)
);

CREATE TABLE oci_manifests (
	id          INTEGER PRIMARY KEY,
	package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
	digest      TEXT NOT NULL,
	media_type  TEXT NOT NULL,
	size        INTEGER NOT NULL,
	refs        TEXT NOT NULL DEFAULT '', -- space-separated digests this manifest references
	created_at  INTEGER NOT NULL,
	UNIQUE (package_id, digest)
);

CREATE TABLE oci_tags (
	package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
	tag         TEXT NOT NULL,
	digest      TEXT NOT NULL,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (package_id, tag)
);

-- A blob is readable through a package only once it was uploaded (or
-- mounted) into that package, so knowing a digest grants nothing.
CREATE TABLE oci_blob_links (
	package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
	digest      TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (package_id, digest)
);

CREATE TABLE oci_uploads (
	id          TEXT PRIMARY KEY,
	package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
	size        INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);
`,
	// 4: kanban board. Each repository has ordered board columns; an issue
	// remembers its column and its position inside it. Closed issues always
	// show in the last column, so board_column only matters for open ones.
	`
CREATE TABLE board_columns (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	name        TEXT NOT NULL COLLATE NOCASE,
	position    INTEGER NOT NULL,
	UNIQUE (repo_id, name)
);

ALTER TABLE issues ADD COLUMN board_column INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN board_pos INTEGER NOT NULL DEFAULT 0;
`,
	// 5: Git LFS. Objects are stored once on disk by content hash; a repo can
	// serve an object only once it was uploaded to (or inherited by) that
	// repo, so knowing a hash grants nothing.
	`
ALTER TABLE repos ADD COLUMN lfs INTEGER NOT NULL DEFAULT 0;

CREATE TABLE lfs_objects (
	oid         TEXT PRIMARY KEY,         -- sha256 hex
	size        INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE lfs_repo_objects (
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	oid         TEXT NOT NULL REFERENCES lfs_objects(oid),
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (repo_id, oid)
);
`,
	// 6: branch protection rules (see package protect).
	`
CREATE TABLE branch_protections (
	id             INTEGER PRIMARY KEY,
	repo_id        INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	pattern        TEXT NOT NULL,
	push           TEXT NOT NULL DEFAULT 'write', -- write | admin | none
	allow_force    INTEGER NOT NULL DEFAULT 0,
	allow_delete   INTEGER NOT NULL DEFAULT 0,
	approvals      INTEGER NOT NULL DEFAULT 0,
	checks         TEXT NOT NULL DEFAULT '',      -- comma-separated pipeline names, or *
	block_changes  INTEGER NOT NULL DEFAULT 1,
	admin_bypass   INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	UNIQUE (repo_id, pattern)
);
`,
	// 7: repository mirrors. A pull mirror (at most one per repository) makes
	// the repository a read-only copy of a remote; push mirrors copy it to
	// remotes. Passwords/tokens and SSH keys are encrypted like CI secrets.
	`
CREATE TABLE mirrors (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	direction   TEXT NOT NULL,                -- pull | push
	url         TEXT NOT NULL,                -- without credentials
	username    TEXT NOT NULL DEFAULT '',
	secret      TEXT NOT NULL DEFAULT '',     -- encrypted password / token
	ssh_key     TEXT NOT NULL DEFAULT '',     -- encrypted private key (SSH remotes)
	ssh_pub     TEXT NOT NULL DEFAULT '',
	interval    INTEGER NOT NULL DEFAULT 0,   -- seconds between syncs (push: 0 = only after pushes)
	run_ci      INTEGER NOT NULL DEFAULT 0,   -- pull: run pipelines for synced commits
	last_sync   INTEGER NOT NULL DEFAULT 0,   -- last successful sync
	last_try    INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT NOT NULL DEFAULT '',
	next_sync   INTEGER NOT NULL DEFAULT 0,   -- 0 = nothing scheduled
	created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX mirrors_one_pull ON mirrors(repo_id) WHERE direction = 'pull';
CREATE INDEX mirrors_due ON mirrors(next_sync);
`,
	// 8: scoped, expiring access tokens. Existing tokens keep full access
	// ("*") and never expire.
	`
ALTER TABLE access_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT '*';
ALTER TABLE access_tokens ADD COLUMN repos TEXT NOT NULL DEFAULT '';
ALTER TABLE access_tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;
`,
	// 9: pull request revisions: every head a PR had (kept reachable as
	// refs/pull/N/rev/K), so rewritten history (jj, rebases, force pushes)
	// can be compared ("interdiff").
	`
CREATE TABLE pr_revisions (
	issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	n           INTEGER NOT NULL,
	head_sha    TEXT NOT NULL,
	base_sha    TEXT NOT NULL,   -- merge base with the base branch at the time
	pusher_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (issue_id, n)
);
`,
}
