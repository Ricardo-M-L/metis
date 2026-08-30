package builtin

import (
	"strings"
	"testing"
)

func TestWebToolRoutingDescriptionsAreMutuallyExclusive(t *testing.T) {
	tests := []struct {
		name        string
		description string
		want        []string
	}{
		{
			name:        "WebSearch",
			description: (WebSearch{}).Description(),
			want:        []string{"keywords", "current information", "not for fetching a known URL"},
		},
		{
			name:        "WebFetch",
			description: (WebFetch{}).Description(),
			want:        []string{"known absolute URL", "does not execute JavaScript", "not for keyword search"},
		},
		{
			name:        "WebBrowse",
			description: (WebBrowse{}).Description(),
			want:        []string{"known absolute URL", "only after WebFetch", "JavaScript"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, phrase := range tt.want {
				if !strings.Contains(strings.ToLower(tt.description), strings.ToLower(phrase)) {
					t.Errorf("Description() = %q; want routing phrase %q", tt.description, phrase)
				}
			}
		})
	}
}

func TestWebToolsPublishCuratedSearchHints(t *testing.T) {
	tests := []struct {
		name string
		hint string
		want []string
	}{
		{name: "WebSearch", hint: (WebSearch{}).SearchHint(), want: []string{"search", "keywords", "current"}},
		{name: "WebFetch", hint: (WebFetch{}).SearchHint(), want: []string{"fetch", "known", "url"}},
		{name: "WebBrowse", hint: (WebBrowse{}).SearchHint(), want: []string{"javascript", "known", "url"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			words := strings.Fields(tt.hint)
			if len(words) < 3 || len(words) > 10 {
				t.Fatalf("SearchHint() = %q; want a curated 3-10 word hint", tt.hint)
			}
			for _, term := range tt.want {
				if !strings.Contains(strings.ToLower(tt.hint), term) {
					t.Errorf("SearchHint() = %q; want term %q", tt.hint, term)
				}
			}
		})
	}
}
