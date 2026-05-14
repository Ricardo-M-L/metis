package builtin

// websearch.go — claude-code-style WebSearch tool.
//
// Backend chain (first-match-wins):
//
//  1. SERPER_API_KEY set      → google.serper.dev (paid, structured, fast)
//  2. otherwise               → DuckDuckGo lite HTML scraper (free, zero-config)
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

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// WebSearch is the claude-code-named web search tool. Zero-config:
// works without any API key via DuckDuckGo's lite HTML view; upgrades
// to Serper.dev's structured Google API when SERPER_API_KEY is set.
type WebSearch struct {
	tools.BaseTool
	gate *permission.Gate
}

func (WebSearch) Name() string { return "WebSearch" }

func (WebSearch) Description() string {
	return "Search the web for up-to-date information. Returns titles, snippets, and URLs for the top results. Works without any API key (DuckDuckGo); set SERPER_API_KEY to upgrade to Google search results."
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

	if key := os.Getenv("SERPER_API_KEY"); key != "" {
		out, err := serperSearch(ctx, client, query, key, maxResults)
		if err == nil {
			return out, nil
		}
		// Serper failed — fall through to DDG so the tool still
		// produces useful output instead of erroring out the turn.
	}

	maybeDelaySearch()
	results, err := searchDuckDuckGo(ctx, client, query, maxResults)
	if err != nil {
		return &tools.Result{
			Output:  "WebSearch failed: " + err.Error(),
			IsError: true,
		}, nil
	}
	return &tools.Result{Output: formatSearchResults(query, results)}, nil
}

// serperSearch hits google.serper.dev — paid Google SERP API. Returns
// up to maxResults organic hits formatted the same way as the DDG
// backend so the model can't tell them apart.
func serperSearch(ctx context.Context, client *http.Client, query, key string, maxResults int) (*tools.Result, error) {
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
		return nil, fmt.Errorf("serper status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
	return &tools.Result{Output: formatSearchResults(query, results)}, nil
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

func formatSearchResults(query string, results []webSearchResult) string {
	if len(results) == 0 {
		return "WebSearch \"" + query + "\": no results. Try rephrasing the query."
	}
	var sb strings.Builder
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
	return strings.TrimRight(sb.String(), "\n")
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
