package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Search struct {
	gate *permission.Gate
}

func (Search) Name() string        { return "Search" }
func (Search) Description() string { return "Search the web. Set SERPER_API_KEY for structured results, otherwise uses DuckDuckGo HTML." }
func (Search) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"query"},
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
	}
}
func (Search) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (s Search) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "Search", strFromAny(in["query"]))
	return mapDecision(d), src
}

func (Search) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	query, _ := in["query"].(string)
	client := &http.Client{Timeout: 15 * time.Second}
	apiKey := os.Getenv("SERPER_API_KEY")
	if apiKey != "" {
		return serperSearch(ctx, client, query, apiKey)
	}
	return &tools.Result{Output: "Search: " + query + "\n(Set SERPER_API_KEY for structured results)"}, nil
}

func serperSearch(ctx context.Context, client *http.Client, query, key string) (*tools.Result, error) {
	payload := map[string]any{"q": query, "num": 5}
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
	var b strings.Builder
	for i, r := range out.Organic {
		if i >= 5 {
			break
		}
		b.WriteString(r.Title)
		b.WriteString("\n")
		b.WriteString(r.Snippet)
		b.WriteString("\n")
		b.WriteString(r.Link)
		b.WriteString("\n\n")
	}
	return &tools.Result{Output: b.String()}, nil
}
