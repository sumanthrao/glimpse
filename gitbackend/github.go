package gitbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// githubClient wraps net/http with auth, sane timeouts, and rate-limit tracking.
//
// The MCP server constructs one client per backend. All GitHub-flavored network
// IO (REST, raw CDN, code search) goes through it.
type githubClient struct {
	http  *http.Client
	token string

	// rate-limit state, last seen from response headers.
	rateMu       chan struct{} // tiny semaphore to serialize state writes
	searchRem    int
	searchLimit  int
	searchReset  time.Time
	restRem      int
	restLimit    int
	restReset    time.Time
}

func newGitHubClient(token string) *githubClient {
	return &githubClient{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		token:       token,
		rateMu:      make(chan struct{}, 1),
		searchLimit: 10,
		restLimit:   60,
	}
}

// RateSnapshot is a point-in-time view of GitHub rate-limit state, as last
// observed in response headers. It's part of Backend.Stats() so agents can
// reason about whether they're about to be throttled.
type RateSnapshot struct {
	SearchRemaining int       `json:"search_remaining"`
	SearchLimit     int       `json:"search_limit"`
	SearchReset     time.Time `json:"search_reset"`
	RestRemaining   int       `json:"rest_remaining"`
	RestLimit       int       `json:"rest_limit"`
	RestReset       time.Time `json:"rest_reset"`
}

func (c *githubClient) Rate() RateSnapshot {
	c.rateMu <- struct{}{}
	defer func() { <-c.rateMu }()
	return RateSnapshot{
		SearchRemaining: c.searchRem,
		SearchLimit:     c.searchLimit,
		SearchReset:     c.searchReset,
		RestRemaining:   c.restRem,
		RestLimit:       c.restLimit,
		RestReset:       c.restReset,
	}
}

func (c *githubClient) recordRate(h http.Header, isSearch bool) {
	rem := atoi(h.Get("X-RateLimit-Remaining"))
	limit := atoi(h.Get("X-RateLimit-Limit"))
	reset := atoi(h.Get("X-RateLimit-Reset"))
	resource := h.Get("X-RateLimit-Resource")

	if rem == 0 && limit == 0 {
		return
	}

	c.rateMu <- struct{}{}
	defer func() { <-c.rateMu }()

	resetT := time.Unix(int64(reset), 0)
	if isSearch || resource == "search" {
		c.searchRem, c.searchLimit, c.searchReset = rem, limit, resetT
	} else {
		c.restRem, c.restLimit, c.restReset = rem, limit, resetT
	}
}

func (c *githubClient) addAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "glimpse-mcp")
}

// doJSON GETs a GitHub REST endpoint and decodes into v.
func (c *githubClient) doJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	c.addAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	c.recordRate(resp.Header, strings.HasPrefix(path, "/search/"))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return c.rateOrAuthError(resp)
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if v == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// fetchRawBlob streams raw.githubusercontent.com for a (commitSHA, path) pair.
//
// Public repos: the CDN doesn't require auth and has no documented rate limit.
// Private repos: pass the token; GitHub redirects through CDN and serves the
// blob.
func (c *githubClient) fetchRawBlob(ctx context.Context, owner, repo, sha, path string) ([]byte, error) {
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		urlEscape(owner), urlEscape(repo), urlEscape(sha), urlEscapePath(path))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "glimpse-mcp")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("raw fetch %s/%s: %w", repo, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("raw fetch %s: %s", path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// codeSearch queries GitHub Code Search for files in this repo matching the
// given literal terms. Each term is ANDed.
func (c *githubClient) codeSearch(ctx context.Context, owner, repo string, literals []string, pathScope string) ([]codeSearchHit, error) {
	if len(literals) == 0 {
		return nil, nil
	}

	var qb strings.Builder
	for i, lit := range literals {
		if i > 0 {
			qb.WriteByte(' ')
		}
		// Quote literals to keep operators inert.
		qb.WriteByte('"')
		qb.WriteString(strings.ReplaceAll(lit, `"`, ``))
		qb.WriteByte('"')
	}
	fmt.Fprintf(&qb, " repo:%s/%s", owner, repo)
	if pathScope != "" {
		fmt.Fprintf(&qb, " path:%s", pathScope)
	}

	q := url.Values{}
	q.Set("q", qb.String())
	q.Set("per_page", "100")

	var resp codeSearchResponse
	if err := c.doJSON(ctx, "/search/code?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

type codeSearchResponse struct {
	TotalCount int             `json:"total_count"`
	Items      []codeSearchHit `json:"items"`
}

type codeSearchHit struct {
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	URL  string `json:"url"`
}

// fetchTree fetches the recursive git tree at the given commit SHA. Returns
// an opaque struct that the backend converts into its tree map.
func (c *githubClient) fetchTree(ctx context.Context, owner, repo, sha string) (*treeResponse, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", urlEscape(owner), urlEscape(repo), urlEscape(sha))
	var t treeResponse
	if err := c.doJSON(ctx, path, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

type treeResponse struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"` // blob, tree, commit (submodule)
	SHA  string `json:"sha"`
	Size int64  `json:"size,omitempty"`
}

// resolveCommit takes a user-supplied ref (branch, tag, SHA, or "") and
// returns the commit SHA that ref points at. An empty ref means the repo's
// default branch.
func (c *githubClient) resolveCommit(ctx context.Context, owner, repo, ref string) (string, string, error) {
	if ref == "" {
		var info repoInfo
		if err := c.doJSON(ctx, fmt.Sprintf("/repos/%s/%s", urlEscape(owner), urlEscape(repo)), &info); err != nil {
			return "", "", err
		}
		ref = info.DefaultBranch
		if ref == "" {
			return "", "", fmt.Errorf("could not resolve default branch for %s/%s", owner, repo)
		}
	}

	// Already a SHA?
	if isHexSHA(ref) {
		return ref, ref, nil
	}

	// Try as branch first, then tag, then commit-ish via /commits/{ref}.
	var commit commitInfo
	if err := c.doJSON(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s",
		urlEscape(owner), urlEscape(repo), urlEscape(ref)), &commit); err != nil {
		return "", "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}
	return ref, commit.SHA, nil
}

type repoInfo struct {
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	Language      string `json:"language"`
}

type commitInfo struct {
	SHA string `json:"sha"`
}

// fetchLanguages returns the language → bytes map.
func (c *githubClient) fetchLanguages(ctx context.Context, owner, repo string) (map[string]int64, error) {
	var langs map[string]int64
	if err := c.doJSON(ctx, fmt.Sprintf("/repos/%s/%s/languages", urlEscape(owner), urlEscape(repo)), &langs); err != nil {
		return nil, err
	}
	return langs, nil
}

func (c *githubClient) rateOrAuthError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := strings.TrimSpace(string(body))
	if h := resp.Header.Get("X-RateLimit-Remaining"); h == "0" {
		reset, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset"))
		secs := int64(0)
		if reset > 0 {
			secs = int64(reset) - time.Now().Unix()
			if secs < 0 {
				secs = 0
			}
		}
		hint := "Set GITHUB_TOKEN to raise limits (10 -> 30 search/min, 60 -> 5000 REST/hr)."
		if c.token != "" {
			hint = "Token already set; wait for the rate-limit window to reset."
		}
		return fmt.Errorf("github rate limited (resets in %ds). %s", secs, hint)
	}
	if c.token == "" {
		return fmt.Errorf("github auth required (status %s). Set GITHUB_TOKEN for private repos or to raise rate limits", resp.Status)
	}
	return fmt.Errorf("github auth failed (%s): %s", resp.Status, msg)
}

var errNotFound = fmt.Errorf("not found")

// IsNotFound reports whether err signals a 404 or missing object.
func IsNotFound(err error) bool {
	return err == errNotFound
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func urlEscape(s string) string  { return url.PathEscape(s) }
func urlEscapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
