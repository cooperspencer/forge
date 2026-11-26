package ci

import (
	"strings"
	"testing"
)

func TestParseAndExpand(t *testing.T) {
	p, err := Parse([]byte(`
name: ci
env: {A: "1"}
jobs:
  lint:
    image: golang:1.23
    steps: ["go vet ./..."]
  test:
    image: golang:${{ matrix.go }}
    needs: lint
    runs-on: [docker, linux]
    matrix:
      go: ["1.22", "1.23"]
      os: [a, b]
    env: {B: "${{ matrix.os }}"}
    steps:
      - name: test ${{ matrix.go }}
        run: go test ./...
`))
	if err != nil {
		t.Fatal(err)
	}
	specs := p.Expand("ci")
	if len(specs) != 5 {
		t.Fatalf("want 1+4 jobs, got %d", len(specs))
	}
	if specs[0].Key != "lint" {
		t.Errorf("declaration order not kept: %s", specs[0].Key)
	}
	s := specs[1]
	if s.Image != "golang:1.22" || s.Env["B"] != "a" || s.Env["A"] != "1" || s.Env["MATRIX_GO"] != "1.22" {
		t.Errorf("interpolation: %+v", s)
	}
	if s.Steps[0].Name != "test 1.22" || !strings.Contains(s.Name, "go=1.22") {
		t.Errorf("names: %q %q", s.Steps[0].Name, s.Name)
	}
	if len(s.Needs) != 1 || s.Needs[0] != "lint" || len(s.Labels) != 2 {
		t.Errorf("needs/labels: %+v", s)
	}
	if specs[0].Steps[0].Name != "go vet ./..." {
		t.Errorf("default step name %q", specs[0].Steps[0].Name)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"no jobs":     `name: x`,
		"no image":    "jobs:\n  a:\n    steps: [x]",
		"no steps":    "jobs:\n  a:\n    image: x",
		"unknown dep": "jobs:\n  a:\n    image: x\n    needs: b\n    steps: [x]",
		"cycle":       "jobs:\n  a:\n    image: x\n    needs: b\n    steps: [x]\n  b:\n    image: x\n    needs: a\n    steps: [x]",
		"bad timeout": "jobs:\n  a:\n    image: x\n    timeout: soon\n    steps: [x]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTriggers(t *testing.T) {
	p, err := Parse([]byte(`
on:
  push:
    branches: [main, "release/*"]
    tags: ["v*"]
    paths: ["src/**"]
  pull_request:
    branches: [main]
jobs:
  a: {image: x, steps: [x]}
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ev   Event
		want bool
	}{
		{Event{Kind: "push", Ref: "refs/heads/main"}, true},
		{Event{Kind: "push", Ref: "refs/heads/release/1.0"}, true},
		{Event{Kind: "push", Ref: "refs/heads/dev"}, false},
		{Event{Kind: "tag", Ref: "refs/tags/v1.2"}, true},
		{Event{Kind: "tag", Ref: "refs/tags/nightly"}, false},
		{Event{Kind: "push", Ref: "refs/heads/main", ChangedFiles: []string{"docs/x.md"}}, false},
		{Event{Kind: "push", Ref: "refs/heads/main", ChangedFiles: []string{"src/a/b.go"}}, true},
		{Event{Kind: "pull_request", Ref: "refs/pull/1/head", BaseBranch: "main"}, true},
		{Event{Kind: "pull_request", Ref: "refs/pull/1/head", BaseBranch: "dev"}, false},
		{Event{Kind: "manual", Ref: "refs/heads/main"}, false},
	}
	for _, c := range cases {
		if got := p.Matches(c.ev); got != c.want {
			t.Errorf("%+v: got %v want %v", c.ev, got, c.want)
		}
	}
	all, _ := Parse([]byte("jobs:\n  a: {image: x, steps: [x]}"))
	if !all.Matches(Event{Kind: "manual"}) || !all.Matches(Event{Kind: "push", Ref: "refs/heads/x"}) {
		t.Error("no `on:` should match everything")
	}
}
