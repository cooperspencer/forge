package forge

import (
	"reflect"
	"testing"
)

func TestParseCodeQuery(t *testing.T) {
	q, err := ParseCodeQuery(`  "hello world"  path:*.go path:docs/ repo:API owner:acme fork:yes`)
	if err != nil {
		t.Fatal(err)
	}
	want := CodeQuery{Text: "hello world", Paths: []string{"*.go", "docs/"}, Repos: []string{"api"}, Owner: "acme", Forks: true}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("got %+v\nwant %+v", q, want)
	}
	if q, _ := ParseCodeQuery("Handler"); !q.Case {
		t.Error("capitals should make the search case-sensitive")
	}
	if q, _ := ParseCodeQuery(`/fo+\s*bar/`); !q.Regex || q.Text != `fo+\s*bar` || !q.Matcher().MatchString("FOO  bar") {
		t.Errorf("regex: %+v", q)
	}
	if q, _ := ParseCodeQuery("a.b"); !q.Matcher().MatchString("A.B") || q.Matcher().MatchString("axb") {
		t.Error("phrases are literal and case-insensitive")
	}
	if got := pathspecs([]string{"*.go", "docs/", "cmd/main.go"}); !reflect.DeepEqual(got, []string{":(glob,icase)**/*.go", ":(glob,icase)docs/**", ":(glob,icase)cmd/main.go"}) {
		t.Errorf("pathspecs: %v", got)
	}
}
