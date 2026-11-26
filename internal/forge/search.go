package forge

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cooperspencer/forge/internal/store"
)

// Code search runs `git grep` on the default branch of each repository the
// actor can read. There is no index to build or keep up to date; for the
// repository counts a self-hosted forge has this is fast enough, and the
// limits below keep a broad query from running away.
//
// Query syntax: words are searched as one literal phrase, case-insensitive
// unless the query has capitals. Filters:
//
//	/regex/        an extended regular expression instead of a phrase
//	path:*.go      only files matching a glob (path:internal/ for a folder)
//	repo:name      only repositories whose owner/name contains name
//	owner:acme     only repositories of that user or organization
//	case:yes       case-sensitive
//	fork:yes       include forks (skipped when searching everything)

type CodeQuery struct {
	Text    string // what to look for
	Regex   bool
	Case    bool // case-sensitive
	Paths   []string
	Repos   []string // owner/name substrings
	Owner   string
	Forks   bool
	Limit   int // total matches (default 300)
	PerRepo int // matches per repository (default 100)
}

// ParseCodeQuery turns "foo bar path:*.go repo:api" into a CodeQuery.
func ParseCodeQuery(q string) (CodeQuery, error) {
	var cq CodeQuery
	var words []string
	for _, tok := range splitQuery(q) {
		k, v, ok := strings.Cut(tok, ":")
		switch {
		case ok && k == "path" && v != "":
			cq.Paths = append(cq.Paths, v)
		case ok && k == "repo" && v != "":
			cq.Repos = append(cq.Repos, strings.ToLower(v))
		case ok && k == "owner" && v != "":
			cq.Owner = v
		case ok && k == "case":
			cq.Case = v == "yes" || v == "true"
		case ok && k == "fork":
			cq.Forks = v == "yes" || v == "true"
		default:
			words = append(words, tok)
		}
	}
	text := strings.Join(words, " ")
	if len(text) > 2 && strings.HasPrefix(text, "/") && strings.HasSuffix(text, "/") {
		cq.Regex, text = true, text[1:len(text)-1]
		if _, err := regexp.Compile(text); err != nil {
			return cq, fmt.Errorf("invalid regular expression: %v", err)
		}
	}
	if strings.TrimSpace(text) == "" {
		return cq, fmt.Errorf("enter something to search for")
	}
	if !cq.Regex && len([]rune(text)) < 2 {
		return cq, fmt.Errorf("search for at least 2 characters")
	}
	if !cq.Case {
		cq.Case = strings.IndexFunc(text, unicode.IsUpper) >= 0 // smart case
	}
	cq.Text = text
	return cq, nil
}

// splitQuery splits on spaces, keeping "quoted phrases" together.
func splitQuery(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// Matcher returns a Go regexp that finds the query in a line (for highlighting).
func (q CodeQuery) Matcher() *regexp.Regexp {
	pat := regexp.QuoteMeta(q.Text)
	if q.Regex {
		pat = q.Text
	}
	if !q.Case {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return regexp.MustCompile(regexp.QuoteMeta(q.Text))
	}
	return re
}

type CodeMatch struct {
	Repo *store.Repo
	Ref  string // branch searched
	Path string
	Line int
	Text string
}

type CodeResult struct {
	Matches   []CodeMatch
	Repos     int  // repositories searched
	Hits      int  // repositories with matches
	Truncated bool // limits reached: refine the query
	Took      time.Duration
}

// SearchCode searches one repository (scope != nil) or all the actor can read.
func (f *Forge) SearchCode(ctx context.Context, actor *store.User, scope *store.Repo, q CodeQuery) (*CodeResult, error) {
	start := time.Now()
	if q.Limit <= 0 {
		q.Limit = 300
	}
	if q.PerRepo <= 0 {
		q.PerRepo = 100
	}
	var repos []*store.Repo
	if scope != nil {
		if f.Store.AccessLevel(ctx, actor, scope) < store.AccessRead {
			return nil, ErrNotFound
		}
		repos = []*store.Repo{scope}
	} else {
		all, err := f.Store.VisibleRepos(ctx, actor)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			if r.ForkOf != 0 && !q.Forks {
				continue
			}
			if q.Owner != "" && !strings.EqualFold(r.Owner, q.Owner) {
				continue
			}
			if len(q.Repos) > 0 && !containsAny(strings.ToLower(r.FullName()), q.Repos) {
				continue
			}
			repos = append(repos, r)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	res := &CodeResult{Repos: len(repos)}
	results := make([][]CodeMatch, len(repos))
	truncated := make([]bool, len(repos))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, r := range repos {
		wg.Add(1)
		go func(i int, r *store.Repo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], truncated[i] = f.grepRepo(ctx, r, q)
		}(i, r)
	}
	wg.Wait()
	// most recently updated repositories first
	order := make([]int, len(repos))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return repos[order[a]].UpdatedAt.After(repos[order[b]].UpdatedAt) })
	for _, i := range order {
		if len(results[i]) > 0 {
			res.Hits++
		}
		if truncated[i] {
			res.Truncated = true
		}
		for _, m := range results[i] {
			if len(res.Matches) >= q.Limit {
				res.Truncated = true
				break
			}
			res.Matches = append(res.Matches, m)
		}
	}
	if ctx.Err() != nil {
		res.Truncated = true
	}
	res.Took = time.Since(start)
	return res, nil
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// pathspecs turns path: filters into git pathspecs: a bare name or glob
// matches at any depth, a trailing slash means a folder.
func pathspecs(paths []string) []string {
	var out []string
	for _, p := range paths {
		p = strings.TrimPrefix(p, "/")
		switch {
		case strings.HasSuffix(p, "/"):
			p += "**"
		case !strings.Contains(p, "/"):
			p = "**/" + p
		}
		out = append(out, ":(glob,icase)"+p)
	}
	return out
}

// grepRepo returns up to q.PerRepo matches and whether it stopped early.
func (f *Forge) grepRepo(ctx context.Context, r *store.Repo, q CodeQuery) ([]CodeMatch, bool) {
	ref := "refs/heads/" + r.DefaultBranch
	g := f.Git(r)
	if _, err := g.ResolveRef(ctx, ref); err != nil {
		return nil, false // empty repository
	}
	args := []string{"--git-dir", f.RepoPath(r), "grep", "-I", "-n", "-z", "--no-color", "--full-name"}
	if !q.Case {
		args = append(args, "-i")
	}
	if q.Regex {
		args = append(args, "-E")
	} else {
		args = append(args, "-F")
	}
	args = append(args, "-e", q.Text, ref, "--")
	args = append(args, pathspecs(q.Paths)...)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := exec.CommandContext(ctx, "git", args...)
	c.Env = append(c.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := c.Start(); err != nil {
		return nil, false
	}
	defer c.Wait()
	var ms []CodeMatch
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	prefix := ref + ":"
	for sc.Scan() {
		// <ref>:<path>\0<line>\0<text>
		parts := bytes.SplitN(sc.Bytes(), []byte{0}, 3)
		if len(parts) != 3 {
			continue
		}
		n, _ := strconv.Atoi(string(parts[1]))
		text := string(parts[2])
		if len(text) > 400 {
			text = text[:400]
		}
		ms = append(ms, CodeMatch{Repo: r, Ref: r.DefaultBranch, Path: strings.TrimPrefix(string(parts[0]), prefix), Line: n, Text: text})
		if len(ms) >= q.PerRepo {
			cancel()
			return ms, true
		}
	}
	return ms, false
}
