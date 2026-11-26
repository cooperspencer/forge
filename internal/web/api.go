package web

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/heatmap"
	"github.com/cooperspencer/forge/internal/store"
)

// ---------------------------------------------------------------- DTOs

type userDTO struct {
	Name     string    `json:"name"`
	FullName string    `json:"full_name"`
	Email    string    `json:"email,omitempty"`
	Admin    bool      `json:"admin"`
	Org      bool      `json:"org"`
	Created  time.Time `json:"created_at"`
}

func toUser(u *store.User, self bool) userDTO {
	d := userDTO{Name: u.Name, FullName: u.FullName, Admin: u.IsAdmin, Org: u.IsOrg, Created: u.CreatedAt}
	if self {
		d.Email = u.Email
	}
	return d
}

type repoDTO struct {
	FullName      string    `json:"full_name"`
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	ForkOf        string    `json:"fork_of,omitempty"`
	LFS           bool      `json:"lfs"`
	Mirror        bool      `json:"mirror"`
	OwnerOrg      bool      `json:"owner_org"`
	SSHURL        string    `json:"ssh_url"`
	HTTPURL       string    `json:"http_url"`
	HasWiki       bool      `json:"has_wiki"`
	WikiSSHURL    string    `json:"wiki_ssh_url,omitempty"`
	WikiHTTPURL   string    `json:"wiki_http_url,omitempty"`
	Created       time.Time `json:"created_at"`
	Updated       time.Time `json:"updated_at"`
}

func (s *Server) toRepo(r *store.Repo) repoDTO {
	d := repoDTO{FullName: r.FullName(), Owner: r.Owner, Name: r.Name, Description: r.Description, Private: r.Private,
		DefaultBranch: r.DefaultBranch, ForkOf: r.ForkOfName, LFS: r.LFS, Mirror: r.Mirror, OwnerOrg: r.OwnerIsOrg,
		SSHURL: s.F.Cfg.SSHCloneURL(r.FullName()), HTTPURL: s.F.Cfg.HTTPCloneURL(r.FullName()), Created: r.CreatedAt, Updated: r.UpdatedAt}
	if s.F.HasWiki(r) {
		d.HasWiki, d.WikiSSHURL, d.WikiHTTPURL = true, s.F.Cfg.SSHCloneURL(r.FullName()+".wiki"), s.F.Cfg.HTTPCloneURL(r.FullName()+".wiki")
	}
	return d
}

// pageBounds applies ?page= (from 1) and ?limit= to n items: it returns the
// slice bounds and sets X-Total-Count. Without limit everything is returned.
func pageBounds(w http.ResponseWriter, r *http.Request, n int) (from, to int, err error) {
	w.Header().Set("X-Total-Count", strconv.Itoa(n))
	q := r.URL.Query()
	if q.Get("limit") == "" && q.Get("page") == "" {
		return 0, n, nil
	}
	limit, page := 50, 1
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return 0, 0, fmt.Errorf("limit must be 1…1000")
		}
	}
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			return 0, 0, fmt.Errorf("page must be 1 or more")
		}
	}
	from = min((page-1)*limit, n)
	return from, min(from+limit, n), nil
}

type issueDTO struct {
	Number   int64     `json:"number"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	State    string    `json:"state"`
	Author   string    `json:"author"`
	Labels   []string  `json:"labels"`
	Comments int       `json:"comments"`
	Created  time.Time `json:"created_at"`
	Updated  time.Time `json:"updated_at"`
	PR       *prDTO    `json:"pull_request,omitempty"`
}

type prDTO struct {
	HeadRepo   string `json:"head_repo"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	HeadSHA    string `json:"head_sha"`
	MergeSHA   string `json:"merge_sha,omitempty"`
	MergedBy   string `json:"merged_by,omitempty"`
}

func toIssue(i *store.Issue) issueDTO {
	d := issueDTO{Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Author: i.Author, Labels: i.Labels,
		Comments: i.Comments, Created: i.CreatedAt, Updated: i.UpdatedAt}
	if d.Labels == nil {
		d.Labels = []string{}
	}
	if i.PR != nil {
		d.PR = &prDTO{i.PR.HeadRepo, i.PR.HeadBranch, i.PR.BaseBranch, i.PR.HeadSHA, i.PR.MergeSHA, i.PR.MergedBy}
	}
	return d
}

type runDTO struct {
	Number   int64      `json:"number"`
	Pipeline string     `json:"pipeline"`
	File     string     `json:"file"`
	Trigger  string     `json:"trigger"`
	Ref      string     `json:"ref"`
	SHA      string     `json:"sha"`
	PR       int64      `json:"pr_number,omitempty"`
	Actor    string     `json:"actor,omitempty"`
	Status   string     `json:"status"`
	Error    string     `json:"error,omitempty"`
	Created  time.Time  `json:"created_at"`
	Started  *time.Time `json:"started_at,omitempty"`
	Finished *time.Time `json:"finished_at,omitempty"`
	Jobs     []jobDTO   `json:"jobs,omitempty"`
}

type jobDTO struct {
	ID       int64      `json:"id"`
	Key      string     `json:"key"`
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Runner   string     `json:"runner,omitempty"`
	ExitCode int        `json:"exit_code"`
	Attempt  int        `json:"attempt"`
	Needs    []string   `json:"needs,omitempty"`
	Started  *time.Time `json:"started_at,omitempty"`
	Finished *time.Time `json:"finished_at,omitempty"`
}

func optTime(t time.Time) *time.Time {
	if t.Unix() <= 0 {
		return nil
	}
	return &t
}

func toRun(r *store.Run) runDTO {
	return runDTO{Number: r.Number, Pipeline: r.Pipeline, File: r.File, Trigger: r.Trigger, Ref: r.Ref, SHA: r.SHA, PR: r.PRNumber,
		Actor: r.Actor, Status: r.Status, Error: r.Error, Created: r.CreatedAt, Started: optTime(r.StartedAt), Finished: optTime(r.FinishedAt)}
}

func toJob(j *store.Job) jobDTO {
	return jobDTO{ID: j.ID, Key: j.Key, Name: j.Name, Status: j.Status, Runner: j.Runner, ExitCode: j.ExitCode, Attempt: j.Attempt,
		Needs: j.Needs, Started: optTime(j.StartedAt), Finished: optTime(j.FinishedAt)}
}

// ---------------------------------------------------------------- routes

func (s *Server) apiRoutes(mux *http.ServeMux) {
	h := func(pattern, scopes string, fn apiFunc) { mux.HandleFunc(pattern, s.api(scopes, fn)) }
	const R = "/api/v1/repos/{owner}/{repo}"

	h("GET /api/v1/user", "", s.getSelf)
	h("GET /api/v1/users/{name}", "", s.getUser)
	h("PATCH /api/v1/users/{name}", "admin", s.patchUser)
	h("GET /api/v1/users/{name}/heatmap", "code:read", s.getHeatmap)
	h("GET /api/v1/users/{name}/secrets", "admin", s.listOwnerSecrets)
	h("PUT /api/v1/users/{name}/secrets/{secret}", "admin", s.putOwnerSecret)
	h("DELETE /api/v1/users/{name}/secrets/{secret}", "admin", s.deleteOwnerSecret)
	h("GET /api/v1/search/code", "code:read", s.searchCode)
	h("GET /api/v1/user/orgs", "", s.listOrgs)
	h("POST /api/v1/orgs", "admin", s.createOrg)
	h("GET /api/v1/repos", "", s.listRepos)
	h("POST /api/v1/repos", "admin", s.createRepo)
	h("GET "+R, "", s.getRepo)
	h("PATCH "+R, "admin", s.patchRepo)
	h("DELETE "+R, "admin", s.deleteRepo)
	h("POST "+R+"/forks", "admin code:read", s.forkRepo)
	h("GET "+R+"/branches", "code:read", s.listRefs(false))
	h("POST "+R+"/branches", "code:write", s.createRef(false))
	h("GET "+R+"/tags", "code:read", s.listRefs(true))
	h("POST "+R+"/tags", "code:write", s.createRef(true))
	h("GET "+R+"/commits", "code:read", s.listCommits)
	h("GET "+R+"/commits/{sha}", "code:read", s.getCommit)
	h("POST "+R+"/commits/{sha}/revert", "code:write", s.applyChange(false, false))
	h("POST "+R+"/commits/{sha}/cherry-pick", "code:write", s.applyChange(false, true))
	h("GET "+R+"/tree", "code:read", s.getTree)
	h("GET "+R+"/raw", "code:read", s.getRaw)
	h("GET "+R+"/collaborators", "admin", s.listCollabs)
	h("PUT "+R+"/collaborators/{user}", "admin", s.putCollab)
	h("DELETE "+R+"/collaborators/{user}", "admin", s.deleteCollab)
	h("GET "+R+"/events", "code:read", s.listEvents)

	h("GET "+R+"/issues", "issues:read", s.listIssues(false))
	h("POST "+R+"/issues", "issues:write", s.createIssue)
	h("GET "+R+"/issues/{n}", "issues:read", s.getIssue)
	h("PATCH "+R+"/issues/{n}", "issues:write", s.patchIssue)
	h("GET "+R+"/issues/{n}/comments", "issues:read", s.listComments)
	h("POST "+R+"/issues/{n}/comments", "issues:write", s.createComment)
	h("POST "+R+"/issues/{n}/move", "issues:write", s.moveIssue)

	h("GET "+R+"/wiki", "code:read", s.listWiki)
	h("GET "+R+"/wiki/{page...}", "code:read", s.getWikiPage)
	h("PUT "+R+"/wiki/{page...}", "code:write", s.putWikiPage)
	h("DELETE "+R+"/wiki/{page...}", "code:write", s.deleteWikiPage)

	h("GET "+R+"/board", "issues:read", s.getBoard)
	h("POST "+R+"/board/columns", "issues:write", s.createBoardColumn)
	h("PATCH "+R+"/board/columns/{id}", "issues:write", s.patchBoardColumn)
	h("DELETE "+R+"/board/columns/{id}", "issues:write", s.deleteBoardColumn)

	h("GET "+R+"/pulls", "issues:read", s.listIssues(true))
	h("POST "+R+"/pulls", "issues:write", s.createPR)
	h("GET "+R+"/pulls/{n}", "issues:read", s.getPR)
	h("PATCH "+R+"/pulls/{n}", "issues:write", s.patchIssue)
	h("GET "+R+"/pulls/{n}/comments", "issues:read", s.listComments)
	h("POST "+R+"/pulls/{n}/comments", "issues:write", s.createComment)
	h("POST "+R+"/pulls/{n}/reviews", "issues:write", s.createReview)
	h("POST "+R+"/pulls/{n}/merge", "issues:write code:write", s.mergePR)
	h("GET "+R+"/pulls/{n}/revisions", "issues:read code:read", s.listRevisions)
	h("GET "+R+"/pulls/{n}/interdiff", "issues:read code:read", s.getInterdiff)
	h("POST "+R+"/pulls/{n}/revert", "issues:read code:write", s.applyChange(true, false))
	h("POST "+R+"/pulls/{n}/cherry-pick", "issues:read code:write", s.applyChange(true, true))

	h("GET "+R+"/releases", "code:read", s.listReleases)
	h("POST "+R+"/releases", "code:write", s.createRelease)

	h("GET "+R+"/runs", "ci:read", s.listRuns)
	h("POST "+R+"/runs", "ci:write", s.triggerRun)
	h("GET "+R+"/runs/{n}", "ci:read", s.getRun)
	h("POST "+R+"/runs/{n}/cancel", "ci:write", s.cancelRun)
	h("POST "+R+"/runs/{n}/approve", "ci:write", s.approveRun)
	h("POST "+R+"/runs/{n}/rerun", "ci:write", s.rerunRun)
	h("GET "+R+"/jobs/{id}/log", "ci:read", s.getJobLog)
	h("GET "+R+"/runs/{n}/artifacts", "ci:read", s.listArtifacts)
	h("GET "+R+"/artifacts/{id}", "ci:read", s.getArtifact)

	h("GET "+R+"/mirrors", "admin", s.listMirrors)
	h("POST "+R+"/mirrors", "admin", s.createMirror)
	h("PATCH "+R+"/mirrors/{id}", "admin", s.patchMirror)
	h("DELETE "+R+"/mirrors/{id}", "admin", s.deleteMirror)
	h("POST "+R+"/mirrors/{id}/sync", "admin", s.syncMirror)

	h("GET "+R+"/protections", "admin", s.listProtections)
	h("PUT "+R+"/protections/{pattern...}", "admin", s.putProtection)
	h("DELETE "+R+"/protections/{pattern...}", "admin", s.deleteProtection)

	h("GET "+R+"/secrets", "admin", s.listSecrets)
	h("PUT "+R+"/secrets/{name}", "admin", s.putSecret)
	h("DELETE "+R+"/secrets/{name}", "admin", s.deleteSecret)

	h("GET /api/v1/runners", "site-admin", s.listRunners)
	h("POST /api/v1/runners", "site-admin", s.createRunner)
	h("GET /api/v1/notifications", "issues:read", s.listNotifications)
}

func (s *Server) repo(r *http.Request, u *store.User, need store.Access) (*store.Repo, error) {
	rp, _, err := s.F.ResolveRepo(r.Context(), u, r.PathValue("owner")+"/"+r.PathValue("repo"), need)
	return rp, err
}

func needUser(u *store.User) error {
	if u == nil {
		return errUnauthorized
	}
	return nil
}

func pathInt(r *http.Request, k string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(k), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", k)
	}
	return n, nil
}

// ---------------------------------------------------------------- users & repos

func (s *Server) getSelf(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := needUser(u); err != nil {
		return err
	}
	writeJSON(w, 200, toUser(u, true))
	return nil
}

// patchUser renames a user or organization: {"name": "new-name"}.
func (s *Server) patchUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	o, err := s.secretOwner(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Name *string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Name != nil {
		if err := s.F.RenameUser(r.Context(), u, o, *req.Name); err != nil {
			return err
		}
	}
	writeJSON(w, 200, toUser(o, o.ID == u.ID))
	return nil
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	x, err := s.F.Store.UserByName(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, toUser(x, u != nil && (u.ID == x.ID || u.SiteAdmin())))
	return nil
}

// getHeatmap returns a user's contributions of the last year in 15-minute
// buckets, as Gitea does: [{"timestamp": unix, "contributions": n}].
// Only repositories the caller can read count.
func (s *Server) getHeatmap(w http.ResponseWriter, r *http.Request, u *store.User) error {
	x, err := s.F.Store.UserByName(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	now := time.Now()
	cs, err := s.F.Store.Contributions(r.Context(), u, x.ID, heatmap.Since(now, time.UTC, heatmap.MaxWeeks))
	if err != nil {
		return err
	}
	type bucket struct {
		Timestamp     int64 `json:"timestamp"`
		Contributions int   `json:"contributions"`
	}
	out := []bucket{}
	for _, c := range cs {
		out = append(out, bucket{c.Timestamp, c.Count})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if u == nil && !s.F.Cfg.AnonymousRead {
		return errUnauthorized
	}
	rs, err := s.F.Store.VisibleRepos(r.Context(), u)
	if err != nil {
		return err
	}
	// filters: ?owner=name (a user or organization), ?q= (name or description)
	owner, query := r.URL.Query().Get("owner"), strings.ToLower(r.URL.Query().Get("q"))
	if owner != "" {
		if _, err := s.F.Store.UserByName(r.Context(), owner); err != nil {
			return fmt.Errorf("owner %q: %w", owner, err)
		}
	}
	var keep []*store.Repo
	for _, rp := range rs {
		if owner != "" && !strings.EqualFold(rp.Owner, owner) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(rp.Name), query) && !strings.Contains(strings.ToLower(rp.Description), query) {
			continue
		}
		keep = append(keep, rp)
	}
	from, to, err := pageBounds(w, r, len(keep))
	if err != nil {
		return err
	}
	out := []repoDTO{}
	for _, rp := range keep[from:to] {
		out = append(out, s.toRepo(rp))
	}
	writeJSON(w, 200, out)
	return nil
}

// listOrgs: the organizations the caller belongs to (site admins: all with ?all=true).
func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := needUser(u); err != nil {
		return err
	}
	type orgDTO struct {
		userDTO
		Role string `json:"role,omitempty"`
	}
	out := []orgDTO{}
	if r.URL.Query().Get("all") == "true" && u.SiteAdmin() {
		users, err := s.F.Store.ListUsers(r.Context())
		if err != nil {
			return err
		}
		for _, x := range users {
			if x.IsOrg {
				role, _ := s.F.Store.OrgRole(r.Context(), x.ID, u.ID)
				out = append(out, orgDTO{toUser(x, false), role})
			}
		}
	} else {
		ms, err := s.F.Store.UserOrgs(r.Context(), u.ID)
		if err != nil {
			return err
		}
		for _, m := range ms {
			out = append(out, orgDTO{toUser(m.Org, false), m.Role})
		}
	}
	writeJSON(w, 200, out)
	return nil
}

// createOrg: POST /api/v1/orgs {name, full_name?}; the caller becomes its owner.
func (s *Server) createOrg(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := needUser(u); err != nil {
		return err
	}
	var req struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	org, err := s.F.CreateOrg(r.Context(), u, req.Name)
	if err != nil {
		return err
	}
	if req.FullName != "" {
		org.FullName = req.FullName
		s.F.Store.UpdateUser(r.Context(), org)
	}
	writeJSON(w, 201, toUser(org, false))
	return nil
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request, u *store.User) error {
	var req struct {
		Owner, Name, Description string
		Private, LFS             bool
		Mirror                   *mirrorReq // create a pull mirror of this remote
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Mirror != nil {
		o, err := req.Mirror.options()
		if err != nil {
			return err
		}
		rp, err := s.F.CreatePullMirror(r.Context(), u, req.Owner, req.Name, req.Description, req.Private, o)
		if err != nil {
			return err
		}
		writeJSON(w, 201, s.toRepo(rp))
		return nil
	}
	rp, err := s.F.CreateRepoWith(r.Context(), u, req.Owner, req.Name, req.Description, forge.RepoOptions{Private: req.Private, LFS: req.LFS})
	if err != nil {
		return err
	}
	writeJSON(w, 201, s.toRepo(rp))
	return nil
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	writeJSON(w, 200, s.toRepo(rp))
	return nil
}

func (s *Server) patchRepo(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	var req struct {
		Description   *string `json:"description"`
		Private       *bool   `json:"private"`
		DefaultBranch *string `json:"default_branch"`
		Name          *string `json:"name"`
		LFS           *bool   `json:"lfs"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Description != nil {
		rp.Description = *req.Description
	}
	if req.Private != nil {
		rp.Private = *req.Private
	}
	if req.DefaultBranch != nil {
		rp.DefaultBranch = *req.DefaultBranch
	}
	if req.LFS != nil {
		rp.LFS = *req.LFS
	}
	if err := s.F.UpdateRepo(r.Context(), u, rp); err != nil {
		return err
	}
	if req.Name != nil && *req.Name != rp.Name {
		if err := s.F.RenameRepo(r.Context(), u, rp, *req.Name); err != nil {
			return err
		}
	}
	writeJSON(w, 200, s.toRepo(rp))
	return nil
}

func (s *Server) deleteRepo(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	if err := s.F.DeleteRepo(r.Context(), u, rp); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) forkRepo(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req struct{ Owner string }
	decode(r, &req)
	f, err := s.F.Fork(r.Context(), u, rp, req.Owner)
	if err != nil {
		return err
	}
	writeJSON(w, 201, s.toRepo(f))
	return nil
}

func (s *Server) listRefs(tags bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) error {
		rp, err := s.repo(r, u, store.AccessRead)
		if err != nil {
			return err
		}
		g := s.F.Git(rp)
		refs, err := g.Branches(r.Context())
		if tags {
			refs, err = g.Tags(r.Context())
		}
		if err != nil {
			return err
		}
		type refDTO struct {
			Name    string    `json:"name"`
			SHA     string    `json:"sha"`
			Subject string    `json:"subject"`
			Date    time.Time `json:"date"`
		}
		out := []refDTO{}
		for _, rf := range refs {
			out = append(out, refDTO{rf.Name, rf.SHA, rf.Subject, rf.When})
		}
		writeJSON(w, 200, out)
		return nil
	}
}

// createRef: POST /branches {name, from?}, POST /tags {name, target?, message?}.
func (s *Server) createRef(tag bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) error {
		if err := needUser(u); err != nil {
			return err
		}
		rp, err := s.repo(r, u, store.AccessWrite)
		if err != nil {
			return err
		}
		var req struct {
			Name    string `json:"name"`
			From    string `json:"from"`
			Target  string `json:"target"`
			Message string `json:"message"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		full := "refs/heads/" + req.Name
		if tag {
			full = "refs/tags/" + req.Name
			err = s.F.CreateTag(r.Context(), u, rp, req.Name, cmp.Or(req.Target, req.From), req.Message)
		} else {
			err = s.F.CreateBranch(r.Context(), u, rp, req.Name, cmp.Or(req.From, req.Target))
		}
		if err != nil {
			return err
		}
		sha, _ := s.F.Git(rp).ResolveRef(r.Context(), full)
		writeJSON(w, 201, map[string]string{"name": req.Name, "sha": sha})
		return nil
	}
}

type commitDTO struct {
	SHA      string    `json:"sha"`
	Parents  []string  `json:"parents"`
	Author   string    `json:"author"`
	Email    string    `json:"email"`
	Date     time.Time `json:"date"`
	Subject  string    `json:"subject"`
	Body     string    `json:"body"`
	ChangeID string    `json:"change_id,omitempty"` // jj change ID
}

func toCommit(c *gitx.Commit) commitDTO {
	return commitDTO{c.SHA, c.Parents, c.Author, c.AuthorEmail, c.When, c.Subject, c.Body, c.ChangeID}
}

func (s *Server) listCommits(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	if ref == "" {
		ref = rp.DefaultBranch
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	skip, _ := strconv.Atoi(q.Get("skip"))
	cs, err := s.F.Git(rp).Log(r.Context(), ref, skip, limit, q.Get("path"))
	if err != nil {
		return err
	}
	out := []commitDTO{}
	for _, c := range cs {
		out = append(out, toCommit(&c))
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getCommit(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	g := s.F.Git(rp)
	c, err := g.Commit(r.Context(), r.PathValue("sha"))
	if err != nil {
		return err
	}
	diffs, err := g.Diff(r.Context(), "", c.SHA)
	if err != nil {
		return err
	}
	type fileDTO struct {
		OldPath   string `json:"old_path"`
		Path      string `json:"path"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	files := []fileDTO{}
	for _, d := range diffs {
		files = append(files, fileDTO{d.OldPath, d.NewPath, d.Status, d.Additions, d.Deletions, d.Patch})
	}
	writeJSON(w, 200, map[string]any{"commit": toCommit(c), "files": files})
	return nil
}

func (s *Server) getTree(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = rp.DefaultBranch
	}
	es, err := s.F.Git(rp).Tree(r.Context(), ref, r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Mode string `json:"mode"`
		SHA  string `json:"sha"`
		Size int64  `json:"size"`
	}
	out := []entry{}
	for _, e := range es {
		out = append(out, entry{e.Name, e.Type, e.Mode, e.SHA, e.Size})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getRaw(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = rp.DefaultBranch
	}
	b, _, err := s.F.Git(rp).Blob(r.Context(), ref, r.URL.Query().Get("path"), 0)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
	return nil
}

func (s *Server) listCollabs(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	cs, err := s.F.Store.Collaborators(r.Context(), rp.ID)
	if err != nil {
		return err
	}
	out := []map[string]string{}
	for _, c := range cs {
		out = append(out, map[string]string{"user": c.User.Name, "role": c.Role})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) putCollab(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	var req struct{ Role string }
	if err := decode(r, &req); err != nil {
		return err
	}
	x, err := s.F.Store.UserByName(r.Context(), r.PathValue("user"))
	if err != nil {
		return err
	}
	if err := s.F.Store.SetCollaborator(r.Context(), rp.ID, x.ID, req.Role); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) deleteCollab(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	x, err := s.F.Store.UserByName(r.Context(), r.PathValue("user"))
	if err != nil {
		return err
	}
	if err := s.F.Store.RemoveCollaborator(r.Context(), rp.ID, x.ID); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

type eventDTO struct {
	Repo    string    `json:"repo"`
	Actor   string    `json:"actor"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	Created time.Time `json:"created_at"`
	Read    *bool     `json:"read,omitempty"`
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	evs, err := s.F.Store.RepoEvents(r.Context(), rp.ID, 100)
	if err != nil {
		return err
	}
	out := []eventDTO{}
	for _, e := range evs {
		out = append(out, eventDTO{e.Repo, e.Actor, e.Kind, e.Summary, e.CreatedAt, nil})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := needUser(u); err != nil {
		return err
	}
	evs, err := s.F.Store.Notifications(r.Context(), u.ID, 100)
	if err != nil {
		return err
	}
	out := []eventDTO{}
	for _, e := range evs {
		if o, n, _ := strings.Cut(e.Repo, "/"); !u.Grant.Covers(o, n) {
			continue // a token limited to other repositories
		}
		read := e.Read
		out = append(out, eventDTO{e.Repo, e.Actor, e.Kind, e.Summary, e.CreatedAt, &read})
	}
	writeJSON(w, 200, out)
	return nil
}

// ---------------------------------------------------------------- issues & PRs

func (s *Server) listIssues(prs bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) error {
		rp, err := s.repo(r, u, store.AccessRead)
		if err != nil {
			return err
		}
		st := r.URL.Query().Get("state")
		if st == "" {
			st = "open"
		}
		if st == "all" {
			st = ""
		}
		is, err := s.F.Store.ListIssues(r.Context(), rp.ID, store.IssueFilter{PRs: prs, State: st, Query: r.URL.Query().Get("q")})
		if err != nil {
			return err
		}
		from, to, err := pageBounds(w, r, len(is))
		if err != nil {
			return err
		}
		out := []issueDTO{}
		for _, i := range is[from:to] {
			out = append(out, toIssue(i))
		}
		writeJSON(w, 200, out)
		return nil
	}
}

func (s *Server) issue(r *http.Request, u *store.User) (*store.Repo, *store.Issue, error) {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return nil, nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, nil, err
	}
	i, err := s.F.Store.IssueByNumber(r.Context(), rp.ID, n)
	if err != nil {
		return nil, nil, err
	}
	wantPR := strings.Contains(r.URL.Path, "/pulls/")
	if wantPR && !i.IsPR {
		return nil, nil, store.ErrNotFound
	}
	return rp, i, nil
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req struct{ Title, Body string }
	if err := decode(r, &req); err != nil {
		return err
	}
	i, err := s.F.CreateIssue(r.Context(), u, rp, req.Title, req.Body)
	if err != nil {
		return err
	}
	writeJSON(w, 201, toIssue(i))
	return nil
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request, u *store.User) error {
	_, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	writeJSON(w, 200, toIssue(i))
	return nil
}

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Title  *string   `json:"title"`
		Body   *string   `json:"body"`
		State  *string   `json:"state"`
		Labels *[]string `json:"labels"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Title != nil || req.Body != nil || req.Labels != nil {
		if req.Title != nil {
			i.Title = *req.Title
		}
		if req.Body != nil {
			i.Body = *req.Body
		}
		if req.Labels != nil {
			i.Labels = *req.Labels
		}
		if err := s.F.EditIssue(r.Context(), u, rp, i); err != nil {
			return err
		}
	}
	if req.State != nil && *req.State != i.State {
		if *req.State != "open" && *req.State != "closed" {
			return fmt.Errorf("state must be open or closed")
		}
		if err := s.F.SetIssueState(r.Context(), u, rp, i, *req.State); err != nil {
			return err
		}
	}
	i, _ = s.F.Store.IssueByNumber(r.Context(), rp.ID, i.Number)
	writeJSON(w, 200, toIssue(i))
	return nil
}

type commentDTO struct {
	ID      int64     `json:"id"`
	Author  string    `json:"author"`
	Body    string    `json:"body"`
	Review  string    `json:"review,omitempty"`
	Path    string    `json:"path,omitempty"`
	Line    int       `json:"line,omitempty"`
	Created time.Time `json:"created_at"`
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, u *store.User) error {
	_, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	cs, err := s.F.Store.Comments(r.Context(), i.ID)
	if err != nil {
		return err
	}
	out := []commentDTO{}
	for _, c := range cs {
		out = append(out, commentDTO{c.ID, c.Author, c.Body, c.Review, c.Path, c.Line, c.CreatedAt})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Body string
		Path string
		Line int
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	c := &store.Comment{Body: req.Body, Path: req.Path, Line: req.Line}
	if req.Path != "" {
		c.Review = "comment"
	}
	if err := s.F.Comment(r.Context(), u, rp, i, c); err != nil {
		return err
	}
	writeJSON(w, 201, map[string]int64{"id": c.ID})
	return nil
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Event string `json:"event"` // approve | changes | comment
		Body  string `json:"body"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Event != "approve" && req.Event != "changes" && req.Event != "comment" {
		return fmt.Errorf("event must be approve, changes or comment")
	}
	c := &store.Comment{Body: req.Body, Review: req.Event}
	if err := s.F.Comment(r.Context(), u, rp, i, c); err != nil {
		return err
	}
	writeJSON(w, 201, map[string]int64{"id": c.ID})
	return nil
}

func (s *Server) createPR(w http.ResponseWriter, r *http.Request, u *store.User) error {
	base, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req struct {
		Title, Body string
		Head        string `json:"head"` // branch, or owner/repo:branch for forks
		Base        string `json:"base"` // base branch
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	head, branch := base, req.Head
	if hr, hb, ok := strings.Cut(req.Head, ":"); ok {
		if head, _, err = s.F.ResolveRepo(r.Context(), u, hr, store.AccessRead); err != nil {
			return err
		}
		branch = hb
	}
	i, err := s.F.OpenPR(r.Context(), u, base, req.Base, head, branch, req.Title, req.Body)
	if err != nil {
		return err
	}
	writeJSON(w, 201, toIssue(i))
	return nil
}

func (s *Server) getPR(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	st, err := s.F.PRStatus(r.Context(), rp, i)
	if err != nil {
		return err
	}
	checks := []runDTO{}
	for _, c := range st.Checks {
		checks = append(checks, toRun(c))
	}
	files := []map[string]any{}
	for _, f := range st.Files {
		files = append(files, map[string]any{"path": f.NewPath, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions})
	}
	below, above := s.F.PRStack(r.Context(), rp, i)
	nums := func(is []*store.Issue) []int64 {
		out := []int64{}
		for _, x := range is {
			out = append(out, x.Number)
		}
		return out
	}
	writeJSON(w, 200, map[string]any{
		"pull": toIssue(i), "mergeable": st.Mergeable, "conflicts": st.Conflicts, "commits": len(st.Commits), "files": files,
		"approvals": st.Approvals, "changes_requested": st.Changes, "checks": checks, "checks_passed": st.ChecksOK && st.ChecksDone,
		"stacked_on": nums(below), "stacked_below": nums(above),
	})
	return nil
}

type revisionDTO struct {
	N       int       `json:"n"`
	Head    string    `json:"head"`
	Base    string    `json:"base"`
	Pusher  string    `json:"pusher,omitempty"`
	Created time.Time `json:"created_at"`
}

func toRevision(v store.PRRevision) revisionDTO {
	return revisionDTO{v.N, v.HeadSHA, v.BaseSHA, v.Pusher, v.CreatedAt}
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	revs, err := s.F.PRRevisions(r.Context(), u, rp, i)
	if err != nil {
		return err
	}
	out := []revisionDTO{}
	for _, v := range revs {
		out = append(out, toRevision(v))
	}
	writeJSON(w, 200, out)
	return nil
}

// getInterdiff: GET .../pulls/{n}/interdiff?from=&to= (default: the last push).
func (s *Server) getInterdiff(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	to, _ := strconv.Atoi(r.URL.Query().Get("to"))
	d, err := s.F.PRInterdiff(r.Context(), u, rp, i, from, to)
	if err != nil {
		return err
	}
	files := func(fs []gitx.FileDiff) []map[string]any {
		out := []map[string]any{}
		for _, f := range fs {
			out = append(out, map[string]any{"path": f.NewPath, "old_path": f.OldPath, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions, "patch": f.Patch})
		}
		return out
	}
	changes := []map[string]any{}
	for _, c := range d.Changes {
		m := map[string]any{"status": c.Status, "change_id": c.ChangeID}
		if c.Old != nil {
			m["old"] = toCommit(c.Old)
		}
		if c.New != nil {
			m["new"] = toCommit(c.New)
		}
		if c.Status == forge.ChangeRewritten {
			m["files"] = files(c.Files)
		}
		if c.Note != "" {
			m["note"] = c.Note
		}
		changes = append(changes, m)
	}
	writeJSON(w, 200, map[string]any{"from": toRevision(d.From), "to": toRevision(d.To), "rebased": d.Rebased,
		"changes": changes, "files": files(d.Files), "note": d.Note})
	return nil
}

func (s *Server) mergePR(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	var req struct{ Method string }
	decode(r, &req)
	sha, err := s.F.MergePR(r.Context(), u, rp, i, req.Method)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]string{"sha": sha})
	return nil
}

// applyChange: POST .../commits/{sha}/revert|cherry-pick and
// .../pulls/{n}/revert|cherry-pick {branch?, pull_request?} (cherry-picks
// need a branch). Opening a pull request needs issues:write.
func (s *Server) applyChange(pr, pick bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) error {
		if err := needUser(u); err != nil {
			return err
		}
		var req struct {
			Branch      string `json:"branch"`
			PullRequest bool   `json:"pull_request"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.PullRequest {
			if err := u.Grant.Require(store.ScopeIssues, store.AccessWrite); err != nil {
				return err
			}
		}
		opt := forge.RevertOptions{Branch: req.Branch, PullRequest: req.PullRequest}
		var res *forge.RevertResult
		if pr {
			rp, i, err := s.issue(r, u)
			if err != nil {
				return err
			}
			if pick {
				res, err = s.F.CherryPickPR(r.Context(), u, rp, i, opt)
			} else {
				res, err = s.F.RevertPR(r.Context(), u, rp, i, opt)
			}
			if err != nil {
				return err
			}
		} else {
			rp, err := s.repo(r, u, store.AccessWrite)
			if err != nil {
				return err
			}
			if pick {
				res, err = s.F.CherryPick(r.Context(), u, rp, r.PathValue("sha"), opt)
			} else {
				res, err = s.F.RevertCommit(r.Context(), u, rp, r.PathValue("sha"), opt)
			}
			if err != nil {
				return err
			}
		}
		out := map[string]any{"sha": res.SHA, "branch": res.Branch}
		if res.PR != nil {
			out["pull_request"] = res.PR.Number
		}
		writeJSON(w, 201, out)
		return nil
	}
}

// ---------------------------------------------------------------- releases

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	rs, err := s.F.Store.Releases(r.Context(), rp.ID)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, rel := range rs {
		out = append(out, map[string]any{"tag": rel.Tag, "title": rel.Title, "notes": rel.Notes, "author": rel.Author, "created_at": rel.CreatedAt})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessWrite)
	if err != nil {
		return err
	}
	var req struct{ Tag, Target, Title, Notes string }
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Tag == "" {
		return fmt.Errorf("tag is required")
	}
	rel, err := s.F.CreateRelease(r.Context(), u, rp, req.Tag, req.Target, req.Title, req.Notes)
	if err != nil {
		return err
	}
	writeJSON(w, 201, map[string]any{"tag": rel.Tag, "title": rel.Title})
	return nil
}

// ---------------------------------------------------------------- CI

func (s *Server) run(r *http.Request, u *store.User) (*store.Repo, *store.Run, error) {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return nil, nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, nil, err
	}
	run, err := s.F.Store.RunByNumber(r.Context(), rp.ID, n)
	return rp, run, err
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	runs, err := s.F.Store.Runs(r.Context(), rp.ID, limit)
	if err != nil {
		return err
	}
	out := []runDTO{}
	for _, run := range runs {
		out = append(out, toRun(run))
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) triggerRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessWrite)
	if err != nil {
		return err
	}
	var req struct{ Ref, Pipeline string }
	if err := decode(r, &req); err != nil {
		return err
	}
	runs, err := s.F.TriggerManual(r.Context(), u, rp, req.Ref, req.Pipeline)
	if err != nil {
		return err
	}
	out := []runDTO{}
	for _, run := range runs {
		out = append(out, toRun(run))
	}
	writeJSON(w, 201, out)
	return nil
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	_, run, err := s.run(r, u)
	if err != nil {
		return err
	}
	jobs, err := s.F.Store.Jobs(r.Context(), run.ID)
	if err != nil {
		return err
	}
	d := toRun(run)
	for _, j := range jobs {
		d.Jobs = append(d.Jobs, toJob(j))
	}
	writeJSON(w, 200, d)
	return nil
}

func (s *Server) approveRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, run, err := s.run(r, u)
	if err != nil {
		return err
	}
	if err := s.F.ApproveRun(r.Context(), u, rp, run); err != nil {
		return err
	}
	w.WriteHeader(202)
	return nil
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, run, err := s.run(r, u)
	if err != nil {
		return err
	}
	if err := s.F.CancelRun(r.Context(), u, rp, run); err != nil {
		return err
	}
	w.WriteHeader(202)
	return nil
}

func (s *Server) rerunRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, run, err := s.run(r, u)
	if err != nil {
		return err
	}
	failed := r.URL.Query().Get("failed") == "true"
	if err := s.F.RerunRun(r.Context(), u, rp, run, failed); err != nil {
		return err
	}
	w.WriteHeader(202)
	return nil
}

// jobInRepo checks the job belongs to the repo in the URL.
func (s *Server) jobInRepo(r *http.Request, u *store.User) (*store.Job, error) {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return nil, err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	j, err := s.F.Store.JobByID(r.Context(), id)
	if err != nil {
		return nil, err
	}
	run, err := s.F.Store.RunByID(r.Context(), j.RunID)
	if err != nil || run.RepoID != rp.ID {
		return nil, store.ErrNotFound
	}
	return j, nil
}

// getJobLog returns the log; with ?follow=true it streams until the job ends.
func (s *Server) getJobLog(w http.ResponseWriter, r *http.Request, u *store.User) error {
	j, err := s.jobInRepo(r, u)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("follow") != "true" {
		b, err := s.F.Logs.Read(j.ID)
		if err != nil {
			return err
		}
		w.Write(b)
		return nil
	}
	snap, ch, cancel := s.F.Logs.Subscribe(j.ID)
	defer cancel()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(snap)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case b := <-ch:
			w.Write(b)
			if fl != nil {
				fl.Flush()
			}
		case <-tick.C:
			cur, err := s.F.Store.JobByID(r.Context(), j.ID)
			if err != nil || (store.IsTerminal(cur.Status) && len(ch) == 0) {
				return nil
			}
		case <-r.Context().Done():
			return nil
		}
	}
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request, u *store.User) error {
	_, run, err := s.run(r, u)
	if err != nil {
		return err
	}
	as, err := s.F.Store.RunArtifacts(r.Context(), run.ID)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, a := range as {
		out = append(out, map[string]any{"id": a.ID, "job_id": a.JobID, "name": a.Name, "size": a.Size, "created_at": a.CreatedAt})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	a, err := s.F.Store.ArtifactByID(r.Context(), id)
	if err != nil {
		return err
	}
	j, err := s.F.Store.JobByID(r.Context(), a.JobID)
	if err != nil {
		return err
	}
	run, err := s.F.Store.RunByID(r.Context(), j.RunID)
	if err != nil || run.RepoID != rp.ID {
		return store.ErrNotFound
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Name))
	http.ServeContent(w, r, a.Name, a.CreatedAt, f)
	return nil
}

func secretsJSON(ss []forge.SecretInfo) []map[string]any {
	out := []map[string]any{}
	for _, sc := range ss {
		m := map[string]any{"name": sc.Name, "updated_at": sc.UpdatedAt, "scope": "repo"}
		if sc.Owner != "" {
			m["scope"], m["owner"], m["overridden"] = "owner", sc.Owner, sc.Overridden
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	ss, err := s.F.RepoSecrets(r.Context(), u, rp)
	if err != nil {
		return err
	}
	writeJSON(w, 200, secretsJSON(ss))
	return nil
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	var req struct{ Value string }
	if err := decode(r, &req); err != nil {
		return err
	}
	if err := s.F.SetRepoSecret(r.Context(), u, rp, r.PathValue("name"), req.Value); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessAdmin)
	if err != nil {
		return err
	}
	if err := s.F.DeleteRepoSecret(r.Context(), u, rp, r.PathValue("name")); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

// secretOwner resolves {name} for user/org-level secrets, which only that
// user, the org's owners and site admins may manage.
func (s *Server) secretOwner(r *http.Request, u *store.User) (*store.User, error) {
	if u == nil {
		return nil, errUnauthorized
	}
	o, err := s.F.Store.UserByName(r.Context(), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	if !s.F.CanManageOwnerSecrets(r.Context(), u, o) {
		return nil, forge.ErrForbidden
	}
	return o, nil
}

func (s *Server) listOwnerSecrets(w http.ResponseWriter, r *http.Request, u *store.User) error {
	o, err := s.secretOwner(r, u)
	if err != nil {
		return err
	}
	ss, err := s.F.OwnerSecrets(r.Context(), u, o)
	if err != nil {
		return err
	}
	writeJSON(w, 200, secretsJSON(ss))
	return nil
}

func (s *Server) putOwnerSecret(w http.ResponseWriter, r *http.Request, u *store.User) error {
	o, err := s.secretOwner(r, u)
	if err != nil {
		return err
	}
	var req struct{ Value string }
	if err := decode(r, &req); err != nil {
		return err
	}
	if err := s.F.SetOwnerSecret(r.Context(), u, o, r.PathValue("secret"), req.Value); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) deleteOwnerSecret(w http.ResponseWriter, r *http.Request, u *store.User) error {
	o, err := s.secretOwner(r, u)
	if err != nil {
		return err
	}
	if err := s.F.DeleteOwnerSecret(r.Context(), u, o, r.PathValue("secret")); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if u == nil || !u.SiteAdmin() {
		return forge.ErrForbidden
	}
	rs, err := s.F.Store.Runners(r.Context())
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, rn := range rs {
		out = append(out, map[string]any{"name": rn.Name, "labels": rn.Labels, "enabled": rn.Enabled, "online": rn.Online(),
			"version": rn.Version, "last_seen": optTime(rn.LastSeen)})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createRunner(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if u == nil || !u.SiteAdmin() {
		return forge.ErrForbidden
	}
	var req struct {
		Name   string
		Labels []string
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	tok, err := s.F.Store.CreateRunner(r.Context(), req.Name, req.Labels)
	if err != nil {
		return err
	}
	writeJSON(w, 201, map[string]string{"name": req.Name, "token": tok})
	return nil
}
