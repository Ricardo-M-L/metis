package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestConfiguredThinkingDisplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{name: "nil", want: "auto"},
		{name: "empty", cfg: &config.Config{}, want: "auto"},
		{name: "show", cfg: &config.Config{UI: config.UI{ThinkingDisplay: " SHOW "}}, want: "show"},
		{name: "hide", cfg: &config.Config{UI: config.UI{ThinkingDisplay: "hide"}}, want: "hide"},
		{name: "invalid", cfg: &config.Config{UI: config.UI{ThinkingDisplay: "verbose"}}, want: "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configuredThinkingDisplay(tc.cfg); got != tc.want {
				t.Fatalf("configuredThinkingDisplay() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInProgressThinkingShowExpandsAndAutoCompacts(t *testing.T) {
	text := "one\ntwo\nthree\nfour\nfive\nsix"
	show := (&inProgressThinkingItem{text: text, expand: true}).Render(80)
	if !strings.Contains(show, "one") || !strings.Contains(show, "six") {
		t.Fatalf("show mode did not include full live trace: %s", show)
	}
	auto := (&inProgressThinkingItem{text: text, expand: false}).Render(80)
	if strings.Contains(auto, "one") || !strings.Contains(auto, "six") {
		t.Fatalf("auto mode did not keep a tail preview: %s", auto)
	}
}

func TestLiveThinkingTailIsBoundedAndUTF8Safe(t *testing.T) {
	input := strings.Repeat("思考", thinkingLiveMaxBytes)
	tail, clipped := liveThinkingTail(input)
	if !clipped {
		t.Fatal("large trace was not clipped")
	}
	if len(tail) > thinkingLiveMaxBytes {
		t.Fatalf("tail bytes = %d, max = %d", len(tail), thinkingLiveMaxBytes)
	}
	if !strings.Contains((&inProgressThinkingItem{text: input}).Render(80), "思考") {
		t.Fatal("UTF-8 tail did not render")
	}
}
