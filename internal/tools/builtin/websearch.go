package builtin

// websearch.go — multi-backend WebSearch tool.
//
// Backend chain (first-match-wins, ordered by output quality):
//
//  1. TAVILY_API_KEY        → tavily.com (1k/mo free tier, richest snippets,
//                              also returns relevance scores)
//  2. BRAVE_SEARCH_API_KEY  → api.search.brave.com (2k/mo free, native index)
//  3. SERPER_API_KEY        → google.serper.dev (paid, Google SERP)
//  4. (zero-config fallback) → lite.duckduckgo.com HTML scrape
//
// UX conventions, mirroring cli-web-search (only open-source project
// surveyed 2026-05 that surfaces backend identity transparently):
//
//   - Every output ends with a "[via <backend>]" footer so the user
//     and the model both know which tier produced the results.
//   - When the preferred backend fails (rate-limit, 5xx, parse error),
//     the footer also lists "fell back from X: <reason>" so the user
//     can see WHY they're on the DDG floor and act on it (add an
//     env var, retry later, change query).
//
// Tool name is "WebSearch" not "Search" — claude-code's training-set
// canonical name. Keeping it aligned avoids confusing the LLM about
// whether this is the same capability it learned about during
// pretraining.
//
// The DuckDuckGo path is adapted from Crush's Go implementation
// (github.com/charmbracelet/crush, internal/agent/tools/search.go).
// We hit lite.duckduckgo.com/lite/ — DDG's lightweight HTML view —
// because the JS-driven main UI is opaque to a non-headless scraper.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// WebSearch is the claude-code-named web search tool. Zero-config:
// works without any API key via DuckDuckGo's lite HTML view. Upgrades
// to richer backends when env vars are present, in the order:
// TAVILY_API_KEY → BRAVE_SEARCH_API_KEY → SERPER_API_KEY.
type WebSearch struct {
	tools.BaseTool
	gate *permission.Gate
}

func (WebSearch) Name() string { return "WebSearch" }

func (WebSearch) Description() string {
	return "Search the web for up-to-date information. Returns titles, snippets, and URLs for the top results. " +
		"Works without any API key (DuckDuckGo fallback). For richer snippets and quotas, set one of: " +
		"TAVILY_API_KEY (1k/mo free, tavily.com), BRAVE_SEARCH_API_KEY (2k/mo free, brave.com), " +
		"or SERPER_API_KEY (paid Google SERP). Backends are tried in that priority order; " +
		"the output footer always reports which backend served the request."
}

func (WebSearch) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"query"},
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results (1-20, default 10).",
			},
		},
	}
}

// Concurrency: WebSearch hits the network — independent calls in the
// same batch are fine, just rate-limit at the per-call layer.
func (WebSearch) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// IsReadOnly so the dispatcher doesn't serialize it behind writes.
func (WebSearch) IsReadOnly(map[string]any) bool { return true }

func (s WebSearch) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "WebSearch", strFromAny(in["query"]))
	return mapDecision(d), src
}

// webSearchBackend is one entry in the priority fallback chain. envVar
// empty means "no key required" — only DDG today, but the shape leaves
// room for future zero-config backends (Ollama web search, SearXNG).
type webSearchBackend struct {
	name   string // user-facing label: "tavily" / "brave" / "serper" / "ddg"
	envVar string // env var that gates this backend ("" = always available)
	search func(ctx context.Context, client *http.Client, query, key string, maxResults int) ([]webSearchResult, error)
}

// resolveSearchKey returns the API key to use for a backend, picking
// the highest-precedence source available:
//
//  1. Environment variable (b.envVar) — CI / shell-rc / one-shot
//     overrides win. Matches how every other backend in the chain
//     reads its key today and keeps CI workflows free of touching
//     auth.json on the runner.
//  2. ~/.metis/auth.json under "search:<name>" — the persistent
//     store written by `metis auth keys put <name> <value>`. 0o600
//     perms enforced by internal/auth, so this is safer than
//     stuffing keys in ~/.zshrc where every shell process inherits
//     them via the global environment.
//
// Returns "" when neither source has a value; callers skip the
// backend silently in that case (it's "not configured", not a
// failure worth surfacing in the fallback trail).
func resolveSearchKey(b webSearchBackend) string {
	if b.envVar == "" {
		return ""
	}
	if v := os.Getenv(b.envVar); v != "" {
		return v
	}
	if v, _ := auth.GetSearchKey(b.name); v != "" {
		return v
	}
	return ""
}

// webSearchBackends is the ordered fallback chain. Order = quality
// estimate (Tavily snippets are the longest + score-ranked; Brave is
// a real index; Serper is paid Google; DDG is the zero-config floor).
// Edit here to reorder or add backends; the dispatcher walks this
// slice top-down and uses the first one whose env var is set (or
// which has no env var requirement).
//
// nolint:gochecknoglobals — package-level table is the cleanest way
// to expose the chain to both Execute() and the diag command, which
// reports each backend's status. Adding a getter would hide the
// declarative shape that makes it easy to audit.
var webSearchBackends = []webSearchBackend{
	{name: "tavily", envVar: "TAVILY_API_KEY", search: tavilySearch},
	{name: "brave", envVar: "BRAVE_SEARCH_API_KEY", search: braveSearch},
	{name: "serper", envVar: "SERPER_API_KEY", search: serperSearch},
	{name: "ddg", envVar: "", search: ddgSearch},
}

func (WebSearch) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	query, _ := in["query"].(string)
	if strings.TrimSpace(query) == "" {
		return &tools.Result{Output: "query is required", IsError: true}, nil
	}

	maxResults := 10
	if n, ok := in["max_results"].(float64); ok && n > 0 {
		maxResults = int(n)
	} else if n, ok := in["max_results"].(int); ok && n > 0 {
		maxResults = n
	}
	if maxResults > 20 {
		maxResults = 20
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// Walk the fallback chain. Keep a record of skipped/failed
	// backends so the output footer can explain WHY we ended up on
	// whichever backend succeeded — important when a paid backend
	// silently rate-limits and the user wonders why snippets got
	// shorter. cli-web-search shows the same kind of "via X" tag in
	// its output; we extend it with the fallback reason for parity
	// with what the user can already see in the metis transcript
	// (no hidden state).
	var fallbacks []string
	for _, b := range webSearchBackends {
		key := resolveSearchKey(b)
		if b.envVar != "" && key == "" {
			// Backend gated on a key and we have neither env nor
			// persisted auth.json entry — skip silently. No
			// fallbacks-trail entry because skipping isn't a
			// failure, it's "not configured".
			continue
		}
		if b.envVar == "" {
			// Only the DDG path needs rate-limiting (paid APIs
			// handle it server-side); keep it out of the per-
			// backend code so adding a new keyed backend doesn't
			// accidentally trip it.
			maybeDelaySearch()
		}
		results, err := b.search(ctx, client, query, key, maxResults)
		if err != nil {
			fallbacks = append(fallbacks, fmt.Sprintf("%s failed: %s", b.name, err.Error()))
			continue
		}
		return &tools.Result{Output: formatSearchResults(query, results, b.name, fallbacks)}, nil
	}
	// Should never happen — DDG has no env-var gate, so the loop
	// always reaches it. If we did fall through, every backend
	// errored; surface the trail so the user can debug.
	msg := "WebSearch: every backend failed"
	if len(fallbacks) > 0 {
		msg = msg + " — " + strings.Join(fallbacks, "; ")
	}
	return &tools.Result{Output: msg, IsError: true}, nil
}

// tavilySearch hits api.tavily.com/search. Snippet quality is the
// best of the four backends (Tavily fans out across multiple
// underlying SERPs and re-ranks; each result also carries a `score`
// field, currently unused but worth keeping in mind if we add
// re-ranking). Auth: `Authorization: Bearer tvly-...`. Free tier:
// 1k searches/month, no credit card.
func tavilySearch(ctx context.Context, client *http.Client, query, key string, maxResults int) ([]webSearchResult, error) {
	payload := map[string]any{
		"query":        query,
		"max_results":  maxResults,
		"search_depth": "basic", // "advanced" is 2x latency and 2x quota
		"topic":        "general",
	}
	pb, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(pb))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"` // tavily's snippet field
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]webSearchResult, 0, len(out.Results))
	for i, r := range out.Results {
		if i >= maxResults {
			break
		}
		results = append(results, webSearchResult{
			Title:    r.Title,
			Link:     r.URL,
			Snippet:  r.Content,
			Position: i + 1,
		})
	}
	return results, nil
}

// braveSearch hits api.search.brave.com/res/v1/web/search. Brave runs
// its own crawler so results don't depend on Google. Auth: header
// `X-Subscription-Token: <key>`. Free tier: 2k queries/month, also
// no credit card. `count` caps at 20 server-side.
func braveSearch(ctx context.Context, client *http.Client, query, key string, maxResults int) ([]webSearchResult, error) {
	count := maxResults
	if count > 20 {
		count = 20
	}
	u := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query) + "&count=" + fmt.Sprint(count)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"` // brave's snippet field
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]webSearchResult, 0, len(out.Web.Results))
	for i, r := range out.Web.Results {
		if i >= maxResults {
			break
		}
		results = append(results, webSearchResult{
			Title:    r.Title,
			Link:     r.URL,
			Snippet:  r.Description,
			Position: i + 1,
		})
	}
	return results, nil
}

// serperSearch hits google.serper.dev — paid Google SERP API. Returns
// up to maxResults organic hits.
func serperSearch(ctx context.Context, client *http.Client, query, key string, maxResults int) ([]webSearchResult, error) {
	payload := map[string]any{"q": query, "num": maxResults}
	pb, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://google.serper.dev/search", bytes.NewReader(pb))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
			Link    string `json:"link"`
		} `json:"organic"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]webSearchResult, 0, len(out.Organic))
	for i, r := range out.Organic {
		if i >= maxResults {
			break
		}
		results = append(results, webSearchResult{
			Title:    r.Title,
			Link:     r.Link,
			Snippet:  r.Snippet,
			Position: i + 1,
		})
	}
	return results, nil
}

// ddgSearch is the zero-config fallback adapter — wraps the existing
// searchDuckDuckGo() to match the webSearchBackend.search signature
// (ignores the key parameter since DDG needs none).
func ddgSearch(ctx context.Context, client *http.Client, query, _ string, maxResults int) ([]webSearchResult, error) {
	return searchDuckDuckGo(ctx, client, query, maxResults)
}

// webSearchResult is the unified shape both backends produce.
type webSearchResult struct {
	Title    string
	Link     string
	Snippet  string
	Position int
}

// User-Agent + Accept-Language randomization — DDG lite is a public
// HTML page but rotating UAs avoids the "you look like a bot" 403
// some installations have hit during sustained use. Adapted directly
// from crush's set; refresh annually as browsers update.
var (
	wsUserAgents = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
	}
	wsAcceptLangs = []string{
		"en-US,en;q=0.9",
		"en-US,en;q=0.9,zh-CN;q=0.8",
		"en-GB,en;q=0.9,en-US;q=0.8",
	}
)

// searchDuckDuckGo hits lite.duckduckgo.com/lite/?q=<encoded> and
// parses the resulting HTML5 table. The lite endpoint is stable
// enough that the parser only needs to look for two element classes:
// `result-link` (anchor with title+href) and `result-snippet` (td).
func searchDuckDuckGo(ctx context.Context, client *http.Client, query string, maxResults int) ([]webSearchResult, error) {
	if maxResults <= 0 {
		maxResults = 10
	}
	searchURL := "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", wsUserAgents[rand.IntN(len(wsUserAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", wsAcceptLangs[rand.IntN(len(wsAcceptLangs))])
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return parseDDGLite(string(body), maxResults)
}

// parseDDGLite walks the parsed HTML tree gathering pairs of
// (result-link, result-snippet). DDG renders one anchor per hit
// followed (a few rows later in the same <table>) by a snippet td.
// The traversal flushes a result whenever a new result-link is hit
// while a previous one is still in-progress, so missing snippets
// don't drop the result entirely.
func parseDDGLite(htmlBody string, maxResults int) ([]webSearchResult, error) {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	var results []webSearchResult
	var current *webSearchResult

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= maxResults {
			return
		}
		if n.Type == html.ElementNode {
			if n.Data == "a" && hasNodeClass(n, "result-link") {
				// Flush the previous in-progress result before starting
				// a new one — its snippet may have been missing but the
				// title+link are still useful to the LLM.
				if current != nil && current.Link != "" {
					current.Position = len(results) + 1
					results = append(results, *current)
					if len(results) >= maxResults {
						return
					}
				}
				current = &webSearchResult{Title: nodeText(n)}
				for _, a := range n.Attr {
					if a.Key == "href" {
						current.Link = cleanDDGRedirect(a.Val)
						break
					}
				}
			}
			if n.Data == "td" && hasNodeClass(n, "result-snippet") && current != nil {
				current.Snippet = nodeText(n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
			if len(results) >= maxResults {
				return
			}
		}
	}
	walk(doc)
	if current != nil && current.Link != "" && len(results) < maxResults {
		current.Position = len(results) + 1
		results = append(results, *current)
	}
	return results, nil
}

func hasNodeClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" && slices.Contains(strings.Fields(a.Val), class) {
			return true
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.TrimSpace(sb.String())
}

// cleanDDGRedirect unwraps DuckDuckGo's tracking redirect format
// `//duckduckgo.com/l/?uddg=<urlencoded-target>&rut=...` so the LLM
// gets the actual destination URL, not a DDG-mediated one.
func cleanDDGRedirect(raw string) string {
	if !strings.Contains(raw, "uddg=") {
		return raw
	}
	_, after, ok := strings.Cut(raw, "uddg=")
	if !ok {
		return raw
	}
	encoded := after
	if amp := strings.Index(encoded, "&"); amp != -1 {
		encoded = encoded[:amp]
	}
	if decoded, err := url.QueryUnescape(encoded); err == nil {
		return decoded
	}
	return raw
}

// formatSearchResults renders the SERP for the model and tags the
// output with the backend that produced it. The `via` tag lets the
// model decide how much to trust the snippets (DDG snippets cap
// around 150 chars; Tavily/Brave run 200-400; Serper varies). The
// `fallbacks` log lists backends we tried before this one — empty
// when the first-choice backend worked.
func formatSearchResults(query string, results []webSearchResult, via string, fallbacks []string) string {
	var sb strings.Builder
	if len(results) == 0 {
		sb.WriteString("WebSearch \"" + query + "\": no results. Try rephrasing the query.")
	} else {
		fmt.Fprintf(&sb, "WebSearch \"%s\" — %d results:\n\n", query, len(results))
		for _, r := range results {
			fmt.Fprintf(&sb, "%d. %s\n", r.Position, r.Title)
			if r.Link != "" {
				fmt.Fprintf(&sb, "   %s\n", r.Link)
			}
			if r.Snippet != "" {
				fmt.Fprintf(&sb, "   %s\n", r.Snippet)
			}
			sb.WriteString("\n")
		}
	}
	// Footer: always print the backend tag so the model + user know
	// which tier served the request. cli-web-search shows the same
	// "Provider: brave" header (see survey 2026-05-20); we put it at
	// the END instead of the top so the actual results stay above
	// the visible-area fold on small terminals.
	sb.WriteString("\n[via " + via)
	if via == "ddg" {
		// Floor backend — flag it explicitly so the user has a
		// clear nudge to set TAVILY_API_KEY / BRAVE_SEARCH_API_KEY.
		sb.WriteString(" · zero-config; for richer results set TAVILY_API_KEY or BRAVE_SEARCH_API_KEY")
	}
	sb.WriteString("]")
	if len(fallbacks) > 0 {
		// Surface the failure trail so a paid backend's rate-limit
		// or transient 5xx isn't invisible. Without this the user
		// thinks they're on the backend they configured but
		// silently got dropped to DDG.
		sb.WriteString("\n[fallback: " + strings.Join(fallbacks, " → ") + "]")
	}
	return sb.String()
}

// Rate limiter: DDG lite rate-limits aggressively when hit faster
// than a human would. The 500-2000ms randomized minimum gap matches
// crush's value and seems to keep DDG happy for hours of continuous
// use during dogfooding.
var (
	wsLastSearchMu sync.Mutex
	wsLastSearchAt time.Time
)

func maybeDelaySearch() {
	wsLastSearchMu.Lock()
	defer wsLastSearchMu.Unlock()
	gap := time.Duration(500+rand.IntN(1500)) * time.Millisecond
	if elapsed := time.Since(wsLastSearchAt); elapsed < gap {
		time.Sleep(gap - elapsed)
	}
	wsLastSearchAt = time.Now()
}
