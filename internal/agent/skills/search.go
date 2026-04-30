package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// Re-export the public types so existing in-tree callers don't churn.
// 3rd parties should import pkg/skill directly.
type (
	SearchHit = pubskill.SearchHit
	Searcher  = pubskill.Searcher
)

// GitHubSource implements Searcher via the public GitHub code search API.
// We look for `<query> in:file path:skills extension:json`, which matches
// the marketplace convention of `skills/<name>.json` files.
//
// No auth → 10 req/min unauthenticated rate limit. Adequate for human-paced
// /skill search use; bulk callers should layer their own caching.
type gitHubSearchResp struct {
	Items []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		HTMLURL    string `json:"html_url"`
		Repository struct {
			FullName    string `json:"full_name"`
			Description string `json:"description"`
		} `json:"repository"`
	} `json:"items"`
}

// Search runs a GitHub code search scoped to skill manifest files.
// Returns up to `limit` (default 10, capped at 30 by GitHub's per-page max
// for code search).
func (g *GitHubSource) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 30 {
		limit = 30
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("github search: query required")
	}
	// GitHub's code-search API needs `path:` to be on its own; combining
	// works most of the time but `extension:` post-filters stricter than
	// the docs imply, so leave it off.
	full := fmt.Sprintf("%s path:skills extension:json", q)
	api := "https://api.github.com/search/code?per_page=" +
		fmt.Sprintf("%d", limit) +
		"&q=" + url.QueryEscape(full)
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "metis-skill-search")

	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return nil, fmt.Errorf("github search rate-limited (status %d) — try again in a minute or set GITHUB_TOKEN", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github search %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}
	var sr gitHubSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	hits := make([]SearchHit, 0, len(sr.Items))
	for _, it := range sr.Items {
		// Skip files whose immediate parent directory isn't literally
		// "skills". Substring matching alone wrongly accepts paths like
		// "not-skills/foo.json" which contain the bytes "skills/" but
		// don't follow the convention.
		segs := strings.Split(it.Path, "/")
		if len(segs) < 2 || segs[len(segs)-2] != "skills" {
			continue
		}
		// Derive skill name from filename (path/to/foo.json → foo).
		base := it.Name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		base = strings.TrimSuffix(base, ".json")
		ref := it.Repository.FullName + ":" + base
		hits = append(hits, SearchHit{
			Source: g.Name(),
			Ref:    ref,
			Name:   base,
			// Use the repo description as a hint; per-skill description
			// would require a second round-trip per hit.
			Description: it.Repository.Description,
			URL:         it.HTMLURL,
		})
	}
	return hits, nil
}
