package builtin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/spill"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type searchReuseEntry struct {
	output string
	until  time.Time
}

var searchReuse = struct {
	sync.Mutex
	entries map[[32]byte]searchReuseEntry
}{entries: make(map[[32]byte]searchReuseEntry)}

func executeBudgetedSearch(ctx context.Context, in map[string]any, fallback func(context.Context, map[string]any) (*tools.Result, error)) (*tools.Result, error) {
	// Session-scoped, brief and success-only. Failures/empty results must remain
	// retryable; fresh=true opts out for rapidly changing information.
	sid := tasks.SessionIDFromContext(ctx)
	encoded, _ := json.Marshal(in)
	identity := ""
	for _, b := range webSearchBackends {
		identity += b.name + ":" + resolveSearchKey(b) + "\x00"
	}
	key := sha256.Sum256(append([]byte(sid+"\x00"+identity), encoded...))
	fresh, _ := in["fresh"].(bool)
	if sid != "" && !fresh {
		searchReuse.Lock()
		e, ok := searchReuse.entries[key]
		searchReuse.Unlock()
		if ok && time.Now().Before(e.until) {
			return &tools.Result{Output: e.output}, nil
		}
	}
	var res *tools.Result
	var err error
	if _, ok := in["sources"]; ok {
		res, err = searchMultiple(ctx, in)
	} else {
		res, err = fallback(ctx, in)
	}
	if err != nil || res == nil {
		return res, err
	}
	res.Output = webBudget(res.Output, webOutputBudget(in))
	if sid != "" && !fresh && !res.IsError && res.Meta["search_reusable"] == true {
		searchReuse.Lock()
		now := time.Now()
		for k, e := range searchReuse.entries {
			if now.After(e.until) {
				delete(searchReuse.entries, k)
			}
		}
		if len(searchReuse.entries) >= 128 {
			clear(searchReuse.entries)
		}
		searchReuse.entries[key] = searchReuseEntry{res.Output, now.Add(15 * time.Second)}
		searchReuse.Unlock()
	}
	return res, nil
}

func searchMultiple(ctx context.Context, in map[string]any) (*tools.Result, error) {
	wanted := map[string]bool{}
	if strings.TrimSpace(strFromAny(in["query"])) == "" {
		return &tools.Result{Output: "query is required", IsError: true}, nil
	}
	if _, explicit := in["sources"]; !explicit {
		for _, b := range webSearchBackends {
			if b.envVar == "" || resolveSearchKey(b) != "" {
				wanted[b.name] = true
			}
		}
	}
	switch v := in["sources"].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				wanted[s] = true
			}
		}
	case []string:
		for _, s := range v {
			wanted[s] = true
		}
	}
	if len(wanted) == 0 {
		return &tools.Result{Output: "sources must name at least one backend", IsError: true}, nil
	}
	for name := range wanted {
		valid := false
		for _, b := range webSearchBackends {
			if b.name == name {
				valid = true
			}
		}
		if !valid {
			return &tools.Result{Output: "unknown search source: " + name, IsError: true}, nil
		}
	}
	maxResults := intArg(in, "max_results", 10)
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 20 {
		maxResults = 20
	}
	type answer struct {
		results []webSearchResult
		err     error
		name    string
	}
	answers := make([]answer, len(webSearchBackends))
	var wg sync.WaitGroup
	for i, b := range webSearchBackends {
		if !wanted[b.name] {
			continue
		}
		answers[i].name = b.name
		key := resolveSearchKey(b)
		if b.envVar != "" && key == "" {
			answers[i].err = fmt.Errorf("not configured")
			continue
		}
		wg.Add(1)
		go func(i int, b webSearchBackend, key string) {
			defer wg.Done()
			if b.envVar == "" {
				maybeDelaySearch()
			}
			answers[i].results, answers[i].err = b.search(ctx, &http.Client{Timeout: 30 * time.Second}, strFromAny(in["query"]), key, maxResults)
		}(i, b, key)
	}
	wg.Wait()
	var merged []webSearchResult
	seen := map[string]int{}
	var sources, failures []string
	successes := 0
	for _, a := range answers {
		if a.name == "" {
			continue
		}
		if a.err != nil {
			failures = append(failures, a.name+" failed: "+a.err.Error())
			continue
		}
		successes++
		sources = append(sources, a.name)
		for _, r := range a.results {
			canonical := canonicalSearchURL(r.Link)
			if idx, ok := seen[canonical]; ok {
				merged[idx].Sources = append(merged[idx].Sources, a.name)
				if len(r.Snippet) > len(merged[idx].Snippet) {
					merged[idx].Snippet = r.Snippet
				}
				continue
			}
			r.Sources = []string{a.name}
			seen[canonical] = len(merged)
			merged = append(merged, r)
		}
	}
	for i := range merged {
		merged[i].Position = i + 1
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	full := formatSearchResults(strFromAny(in["query"]), merged, strings.Join(sources, ","), failures)
	saved, err := spill.Store(filepath.Join(config.Home(), "tool-results"), "websearch-"+hex.EncodeToString(nonce[:]), full)
	if err != nil {
		return &tools.Result{Output: "WebSearch: cannot safely save full search results; output withheld", IsError: true}, nil
	}
	if len(merged) > maxResults {
		merged = merged[:maxResults]
	}
	hint := fmt.Sprintf("[full search results saved to %s — use Read tool with this exact path]\n", saved.Path)
	budget := webOutputBudget(in) - webTokenEstimate(hint)
	if budget < 128 {
		return &tools.Result{Output: "WebSearch recovery metadata exceeds budget", IsError: true}, nil
	}
	return &tools.Result{Output: hint + formatBudgetedSearchResults(strFromAny(in["query"]), merged, strings.Join(sources, ","), failures, budget), IsError: successes == 0, Meta: map[string]any{"search_reusable": len(merged) > 0 && len(failures) == 0}}, nil
}

// Keep complete result blocks and the provider footer; never cut a URL in
// half to meet the budget. Omitted lower-ranked hits are explicitly counted.
func formatBudgetedSearchResults(query string, results []webSearchResult, via string, failures []string, budget int) string {
	query = webBudget(query, 256)
	clipped := append([]webSearchResult(nil), results...)
	for i := range clipped {
		clipped[i].Title = webBudget(clipped[i].Title, 256)
		clipped[i].Snippet = webBudget(clipped[i].Snippet, 768)
	}
	for len(clipped) > 0 {
		out := formatSearchResults(query, clipped, via, failures)
		if len(clipped) < len(results) {
			out += fmt.Sprintf("\n[%d lower-ranked results omitted by output budget]", len(results)-len(clipped))
		}
		if webTokenEstimate(out) <= budget {
			return out
		}
		clipped = clipped[:len(clipped)-1]
	}
	if len(results) > 0 {
		return webBudget(fmt.Sprintf("WebSearch %q: results omitted because complete citations exceed output budget.\n[via %s]", query, via), budget)
	}
	return webBudget(formatSearchResults(query, nil, via, failures), budget)
}

func canonicalSearchURL(raw string) string {
	u, err := url.Parse(cleanDDGRedirect(raw))
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "gclid" || k == "fbclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}
