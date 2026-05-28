package gitbackend

import (
	"fmt"
	"strings"
)

// RepoRef identifies a GitHub repository at a specific ref.
//
// CommitSHA is filled in by Open after resolving Ref against the GitHub API.
// All raw URL fetches use CommitSHA so reads stay consistent across a session
// even if the branch advances upstream.
type RepoRef struct {
	Owner     string
	Repo      string
	Ref       string // user-supplied: branch, tag, commit, or empty -> default branch
	CommitSHA string // resolved at Open time
}

// ParseGitHubURL accepts the common spellings of a github.com URL and rejects
// everything else. This is intentional — glimpse only supports github.com.
//
// Accepted forms:
//
//	https://github.com/owner/repo
//	https://github.com/owner/repo.git
//	https://github.com/owner/repo/tree/<branch>
//	git@github.com:owner/repo.git
//	github.com/owner/repo
//
// Anything else returns an error pointing at the constraint.
func ParseGitHubURL(s string) (RepoRef, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return RepoRef{}, fmt.Errorf("empty URL; pass a github.com URL like https://github.com/owner/repo")
	}

	ref := ""

	// SSH form: git@github.com:owner/repo[.git]
	if strings.HasPrefix(s, "git@github.com:") {
		rest := strings.TrimPrefix(s, "git@github.com:")
		owner, repo, err := splitOwnerRepo(rest)
		if err != nil {
			return RepoRef{}, fmt.Errorf("parse ssh URL %q: %w", orig, err)
		}
		return RepoRef{Owner: owner, Repo: repo, Ref: ref}, nil
	}

	// Strip scheme.
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(s, scheme) {
			s = strings.TrimPrefix(s, scheme)
			break
		}
	}

	// Strip leading github.com/ host.
	if !strings.HasPrefix(s, "github.com/") {
		return RepoRef{}, fmt.Errorf("only github.com URLs are supported; got %q", orig)
	}
	s = strings.TrimPrefix(s, "github.com/")

	// At this point s should start with owner/repo and may have /tree/<ref> or /blob/<ref>/... after it.
	parts := strings.SplitN(s, "/", 5)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return RepoRef{}, fmt.Errorf("expected owner/repo in URL: %q", orig)
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if repo == "" {
		return RepoRef{}, fmt.Errorf("expected repo name in URL: %q", orig)
	}

	// Optional /tree/<ref>... or /blob/<ref>/<path>...
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
	}

	return RepoRef{Owner: owner, Repo: repo, Ref: ref}, nil
}

func splitOwnerRepo(s string) (string, string, error) {
	s = strings.TrimSuffix(s, ".git")
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected owner/repo, got %q", s)
	}
	return parts[0], parts[1], nil
}

// String renders RepoRef as a short human label like "owner/repo@HEAD" or
// "owner/repo@main (abc1234)" once a CommitSHA is resolved.
func (r RepoRef) String() string {
	ref := r.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if r.CommitSHA != "" {
		return fmt.Sprintf("%s/%s@%s (%s)", r.Owner, r.Repo, ref, shortSHA(r.CommitSHA))
	}
	return fmt.Sprintf("%s/%s@%s", r.Owner, r.Repo, ref)
}

func shortSHA(s string) string {
	if len(s) < 7 {
		return s
	}
	return s[:7]
}
