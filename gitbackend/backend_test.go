package gitbackend

import (
	"reflect"
	"sort"
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	cases := []struct {
		in              string
		owner, repo, ref string
		wantErr         bool
	}{
		{"https://github.com/owner/repo", "owner", "repo", "", false},
		{"https://github.com/owner/repo.git", "owner", "repo", "", false},
		{"https://github.com/owner/repo/tree/main", "owner", "repo", "main", false},
		{"https://github.com/owner/repo/blob/v1.2.3/README.md", "owner", "repo", "v1.2.3", false},
		{"http://github.com/owner/repo", "owner", "repo", "", false},
		{"github.com/owner/repo", "owner", "repo", "", false},
		{"git@github.com:owner/repo.git", "owner", "repo", "", false},
		{"git@github.com:owner/repo", "owner", "repo", "", false},

		{"", "", "", "", true},
		{"https://gitlab.com/owner/repo", "", "", "", true},
		{"https://github.com/owner", "", "", "", true},
		{"/local/path", "", "", "", true},
		{"github.com/", "", "", "", true},
	}

	for _, tc := range cases {
		got, err := ParseGitHubURL(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if got.Owner != tc.owner || got.Repo != tc.repo || got.Ref != tc.ref {
			t.Errorf("%q: got %+v, want owner=%s repo=%s ref=%s",
				tc.in, got, tc.owner, tc.repo, tc.ref)
		}
	}
}

func TestExtractLiterals(t *testing.T) {
	cases := []struct {
		pattern string
		want    []string
	}{
		{"handleAuth", []string{"handleAuth"}},
		{"func.*Login", []string{"func", "Login"}},
		{"a.*b", nil},
		{"\\.go$", []string{".go"}},
		{"package main", []string{"package main"}},
		{".*", nil},
		{"foo|bar", []string{"foo", "bar"}},
		{"a|b|c", nil},
		{"foobar|bazqux", []string{"foobar", "bazqux"}},
	}
	for _, tc := range cases {
		got := ExtractLiterals(tc.pattern)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ExtractLiterals(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"/":       "",
		"/foo":    "foo",
		"foo/":    "foo",
		"/foo/":   "foo",
		"foo/bar": "foo/bar",
		".":       "",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIncrementalIndex_AddSearch(t *testing.T) {
	idx := newIncrementalIndex()
	idx.Add("a.go", "sha1", []byte("package main\nfunc handleAuth() {}\n"))
	idx.Add("b.go", "sha2", []byte("package main\nfunc Login() {}\n"))
	idx.Add("bin", "sha3", []byte{0, 1, 2, 3, 4})

	if !idx.Has("a.go") || !idx.Has("b.go") {
		t.Error("expected both text files indexed")
	}

	hits, err := idx.Search("handleAuth", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.go" {
		t.Errorf("expected one hit on a.go, got %+v", hits)
	}

	// Pattern with no literal anchor should still find via full scan.
	hits2, _ := idx.Search("Login", "")
	gotPaths := []string{}
	for _, h := range hits2 {
		gotPaths = append(gotPaths, h.Path)
	}
	sort.Strings(gotPaths)
	if !reflect.DeepEqual(gotPaths, []string{"b.go"}) {
		t.Errorf("expected [b.go], got %v", gotPaths)
	}
}

func TestIncrementalIndex_PathScope(t *testing.T) {
	idx := newIncrementalIndex()
	idx.Add("src/auth.go", "s1", []byte("handleAuth"))
	idx.Add("docs/auth.md", "s2", []byte("handleAuth"))
	hits, _ := idx.Search("handleAuth", "src/")
	if len(hits) != 1 || hits[0].Path != "src/auth.go" {
		t.Errorf("expected only src match, got %+v", hits)
	}
}
