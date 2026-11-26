// Package ci implements pipeline definitions, run orchestration and log
// streaming. Execution happens elsewhere (see package runner): the server only
// decides *what* runs and *when*; runners pull work and report back.
package ci

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pipeline files live at .forge.yml or .forge/pipelines/*.yml in the repository.
const (
	RootFile    = ".forge.yml"
	PipelineDir = ".forge/pipelines"
)

func IsPipelineFile(p string) bool {
	if p == RootFile {
		return true
	}
	return strings.HasPrefix(p, PipelineDir+"/") && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")) &&
		!strings.Contains(strings.TrimPrefix(p, PipelineDir+"/"), "/")
}

type Pipeline struct {
	Name string            `yaml:"name"`
	On   *Triggers         `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]*Job   `yaml:"jobs"`

	order []string // job keys in file order
}

type Triggers struct {
	Push        *RefFilter `yaml:"push"`
	PullRequest *RefFilter `yaml:"pull_request"`
	Manual      bool       `yaml:"manual"`
	// Schedule runs the pipeline on the default branch at cron times.
	Schedule []Schedule `yaml:"schedule"`
}

// Schedule is one cron entry: `- cron: "0 18 * * *"` (optionally with
// `timezone: Europe/Vienna`; default UTC), or just `- "0 18 * * *"`.
type Schedule struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`

	cron *Cron
	loc  *time.Location
}

func (s *Schedule) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Cron = n.Value
		return nil
	}
	type raw Schedule
	return n.Decode((*raw)(s))
}

func (s *Schedule) compile() error {
	c, err := ParseCron(s.Cron)
	if err != nil {
		return err
	}
	loc := time.UTC
	if s.Timezone != "" {
		if loc, err = time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("schedule timezone %q: %v", s.Timezone, err)
		}
	}
	s.cron, s.loc = c, loc
	return nil
}

// DueAt reports whether any schedule fires in the minute containing t.
func (p *Pipeline) DueAt(t time.Time) bool {
	if p.On == nil {
		return false
	}
	for _, s := range p.On.Schedule {
		if s.cron != nil && s.cron.Matches(t.In(s.loc)) {
			return true
		}
	}
	return false
}

type RefFilter struct {
	Branches []string `yaml:"branches"`
	Tags     []string `yaml:"tags"`
	Paths    []string `yaml:"paths"`
}

type Job struct {
	Name      string              `yaml:"name"`
	Image     string              `yaml:"image"`
	Needs     StringList          `yaml:"needs"`
	RunsOn    StringList          `yaml:"runs-on"`
	Env       map[string]string   `yaml:"env"`
	Secrets   []string            `yaml:"secrets"`
	Matrix    map[string][]string `yaml:"matrix"`
	Steps     []Step              `yaml:"steps"`
	Artifacts []string            `yaml:"artifacts"`
	Timeout   string              `yaml:"timeout"`
	If        string              `yaml:"if"` // "always" runs even if needs failed
}

type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

// StringList accepts either a scalar or a sequence in YAML.
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = []string{n.Value}
		return nil
	}
	var l []string
	if err := n.Decode(&l); err != nil {
		return err
	}
	*s = l
	return nil
}

// Step also accepts a bare string ("- go test ./...").
func (s *Step) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Run = n.Value
		return nil
	}
	type raw Step
	return n.Decode((*raw)(s))
}

// JobSpec is a fully resolved job (one matrix cell) as handed to a runner.
type JobSpec struct {
	Pipeline  string            `json:"pipeline"`
	Key       string            `json:"key"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Steps     []Step            `json:"steps"`
	Env       map[string]string `json:"env"`
	Secrets   []string          `json:"secrets"`
	Artifacts []string          `json:"artifacts"`
	Timeout   time.Duration     `json:"timeout"`
	Labels    []string          `json:"labels"`
	Needs     []string          `json:"needs"`
	Matrix    map[string]string `json:"matrix,omitempty"`
	Always    bool              `json:"always,omitempty"`
	// Fork: the job runs code from a fork's pull request — no secrets, and
	// its job token may not push packages.
	Fork bool `json:"fork,omitempty"`
}

func Parse(data []byte) (*Pipeline, error) {
	var p Pipeline
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if err := root.Decode(&p); err != nil {
		return nil, err
	}
	// Recover declaration order of jobs for stable display.
	if len(root.Content) > 0 {
		m := root.Content[0]
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == "jobs" {
				jm := m.Content[i+1]
				for k := 0; k+1 < len(jm.Content); k += 2 {
					p.order = append(p.order, jm.Content[k].Value)
				}
			}
		}
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Pipeline) validate() error {
	if len(p.Jobs) == 0 {
		return fmt.Errorf("pipeline has no jobs")
	}
	if p.On != nil {
		for i := range p.On.Schedule {
			if err := p.On.Schedule[i].compile(); err != nil {
				return err
			}
		}
	}
	for k, j := range p.Jobs {
		if j == nil {
			return fmt.Errorf("job %q is empty", k)
		}
		if j.Image == "" {
			return fmt.Errorf("job %q: image is required", k)
		}
		if !ValidImage(j.Image) {
			return fmt.Errorf("job %q: invalid image %q", k, j.Image)
		}
		if len(j.Steps) == 0 {
			return fmt.Errorf("job %q: at least one step is required", k)
		}
		for _, n := range j.Needs {
			if _, ok := p.Jobs[n]; !ok {
				return fmt.Errorf("job %q needs unknown job %q", k, n)
			}
		}
		for i, name := range j.Secrets {
			n, err := NormalizeSecretName(name)
			if err != nil {
				return fmt.Errorf("job %q: %v", k, err)
			}
			j.Secrets[i] = n
		}
		if j.Timeout != "" {
			if _, err := time.ParseDuration(j.Timeout); err != nil {
				return fmt.Errorf("job %q: bad timeout: %v", k, err)
			}
		}
	}
	// cycle detection
	state := map[string]int{}
	var visit func(string) error
	visit = func(k string) error {
		switch state[k] {
		case 1:
			return fmt.Errorf("dependency cycle involving job %q", k)
		case 2:
			return nil
		}
		state[k] = 1
		for _, n := range p.Jobs[k].Needs {
			if err := visit(n); err != nil {
				return err
			}
		}
		state[k] = 2
		return nil
	}
	for k := range p.Jobs {
		if err := visit(k); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) JobOrder() []string {
	if len(p.order) == len(p.Jobs) {
		return p.order
	}
	keys := make([]string, 0, len(p.Jobs))
	for k := range p.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Event describes what happened in the repository.
type Event struct {
	Kind         string // push | tag | pull_request | manual | schedule
	Ref          string // full ref name
	BaseBranch   string // pull_request: the target branch (filters match this)
	ChangedFiles []string
}

// Matches reports whether the pipeline should run for ev.
// Without an `on:` section, pipelines run on every push, PR and manual trigger.
func (p *Pipeline) Matches(ev Event) bool {
	if ev.Kind == "schedule" { // only pipelines that ask for it
		return p.On != nil && len(p.On.Schedule) > 0
	}
	if p.On == nil {
		return true
	}
	switch ev.Kind {
	case "manual":
		return p.On.Manual
	case "push", "tag":
		return p.On.Push != nil && p.On.Push.match(ev)
	case "pull_request":
		if p.On.PullRequest == nil {
			return false
		}
		// branch filters on pull_request refer to the base branch
		pe := ev
		pe.Ref = "refs/heads/" + ev.BaseBranch
		return p.On.PullRequest.match(pe)
	}
	return false
}

func (f *RefFilter) match(ev Event) bool {
	if ev.Kind == "tag" || strings.HasPrefix(ev.Ref, "refs/tags/") {
		if len(f.Tags) == 0 {
			return false
		}
		if !globAny(f.Tags, strings.TrimPrefix(ev.Ref, "refs/tags/")) {
			return false
		}
	} else {
		// No branch filter means all branches; tags-only filters skip branches.
		if len(f.Branches) == 0 && len(f.Tags) > 0 {
			return false
		}
		if len(f.Branches) > 0 && !globAny(f.Branches, strings.TrimPrefix(ev.Ref, "refs/heads/")) {
			return false
		}
	}
	if len(f.Paths) > 0 && ev.ChangedFiles != nil {
		hit := false
		for _, cf := range ev.ChangedFiles {
			if globAny(f.Paths, cf) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// globAny matches name against patterns; "**" matches across slashes.
func globAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if strings.Contains(p, "**") {
			if strings.HasPrefix(name, strings.SplitN(p, "**", 2)[0]) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

var exprRe = regexp.MustCompile(`\$\{\{\s*([a-zA-Z_]+)\.([a-zA-Z0-9_\-]+)\s*\}\}`)

func interpolate(s string, matrix map[string]string) string {
	return exprRe.ReplaceAllStringFunc(s, func(m string) string {
		g := exprRe.FindStringSubmatch(m)
		if g[1] == "matrix" {
			if v, ok := matrix[g[2]]; ok {
				return v
			}
		}
		return m
	})
}

// Expand resolves matrices into concrete job specs, in declaration order.
func (p *Pipeline) Expand(pipelineName string) []JobSpec {
	var out []JobSpec
	for _, key := range p.JobOrder() {
		j := p.Jobs[key]
		for _, cell := range matrixCells(j.Matrix) {
			name := j.Name
			if name == "" {
				name = key
			}
			name = interpolate(name, cell)
			if len(cell) > 0 && !strings.Contains(j.Name, "${{") {
				name += " (" + cellLabel(cell) + ")"
			}
			env := map[string]string{}
			for k, v := range p.Env {
				env[k] = interpolate(v, cell)
			}
			for k, v := range j.Env {
				env[k] = interpolate(v, cell)
			}
			for k, v := range cell {
				env["MATRIX_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = v
			}
			steps := make([]Step, len(j.Steps))
			for i, s := range j.Steps {
				steps[i] = Step{Name: interpolate(s.Name, cell), Run: interpolate(s.Run, cell)}
				if steps[i].Name == "" {
					first, _, _ := strings.Cut(strings.TrimSpace(steps[i].Run), "\n")
					steps[i].Name = first
				}
			}
			timeout := time.Hour
			if j.Timeout != "" {
				timeout, _ = time.ParseDuration(j.Timeout)
			}
			out = append(out, JobSpec{
				Pipeline: pipelineName, Key: key, Name: name, Image: interpolate(j.Image, cell),
				Steps: steps, Env: env, Secrets: j.Secrets, Artifacts: j.Artifacts, Timeout: timeout,
				Labels: j.RunsOn, Needs: j.Needs, Matrix: cell, Always: j.If == "always",
			})
		}
	}
	return out
}

func cellLabel(cell map[string]string) string {
	keys := make([]string, 0, len(cell))
	for k := range cell {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + cell[k]
	}
	return strings.Join(parts, ", ")
}

// matrixCells computes the cartesian product; an empty matrix yields one empty cell.
func matrixCells(m map[string][]string) []map[string]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cells := []map[string]string{{}}
	for _, k := range keys {
		var next []map[string]string
		for _, c := range cells {
			for _, v := range m[k] {
				n := make(map[string]string, len(c)+1)
				for kk, vv := range c {
					n[kk] = vv
				}
				n[k] = v
				next = append(next, n)
			}
		}
		cells = next
	}
	if len(m) == 0 {
		return []map[string]string{nil}
	}
	return cells
}

// JobPayload is what the server hands to a runner when it claims a job.
type JobPayload struct {
	JobID      int64             `json:"job_id"`
	RunID      int64             `json:"run_id"`
	RunNumber  int64             `json:"run_number"`
	Repo       string            `json:"repo"`
	CloneURL   string            `json:"clone_url"`
	CloneUser  string            `json:"clone_user"`
	CloneToken string            `json:"clone_token"`
	LFS        bool              `json:"lfs,omitempty"` // fetch Git LFS objects after checkout
	Ref        string            `json:"ref"`
	SHA        string            `json:"sha"`
	Spec       JobSpec           `json:"spec"`
	Secrets    map[string]string `json:"secrets"`
	Env        map[string]string `json:"env"`
}

// ValidImage rejects image references that a container CLI could parse as an
// option (leading "-") or that contain whitespace or control characters.
// `${{ matrix.x }}` placeholders are allowed; the runner re-checks the
// interpolated value.
func ValidImage(img string) bool {
	if img == "" || strings.HasPrefix(img, "-") || len(img) > 512 {
		return false
	}
	for _, c := range strings.ReplaceAll(strings.ReplaceAll(img, "${{ ", ""), " }}", "") {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}
