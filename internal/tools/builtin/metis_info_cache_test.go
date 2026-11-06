package builtin

import (
	"context"
	"strings"
	"testing"
)

type fixtureCacheSummary struct{}

func (fixtureCacheSummary) CacheSummary() string {
	return "[cache]\nprojection_source = local_request_estimate\nactual_cache_read = 900\n"
}

func TestMetisInfoCacheDiagnostics(t *testing.T) {
	m := NewMetisInfo(nil, nil, nil, nil, nil).WithCacheStats(fixtureCacheSummary{})
	result, err := m.Execute(context.Background(), map[string]any{"section": "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "projection_source = local_request_estimate") || !strings.Contains(result.Output, "actual_cache_read = 900") || strings.Contains(result.Output, "[model]") {
		t.Fatal("cache filter lost diagnostics")
	}
}
