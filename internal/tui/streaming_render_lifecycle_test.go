package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestStreamingRenderCache_ReleasesBeforeEarlyViewReturn(t *testing.T) {
	for _, mode := range []string{"welcome", "copy", "screen"} {
		t.Run(mode, func(t *testing.T) {
			m := newCachedScrollModel(80, 30)
			m.streamingText = strings.Repeat("large completed reply ", 4096)
			m.spinnerActive = true
			_ = m.View()
			if !m.streamingRenderCache.valid {
				t.Fatal("fixture did not populate live layout")
			}
			m.streamingText = ""
			m.messages = nil
			m.spinnerActive = false
			switch mode {
			case "copy":
				m.copyMode = true
			case "screen":
				m.activeScreen = screen.NewBodyScreen("/test", "overlay")
			}
			_ = m.View()
			if m.streamingRenderCache != (streamingRenderCache{}) {
				t.Fatal("inactive reply layout survived an early View return")
			}
		})
	}
}

func TestStreamingRenderCache_KeepsActiveReplyBehindOverlay(t *testing.T) {
	m := newCachedScrollModel(80, 30)
	m.streamingText = "active reply"
	m.spinnerActive = true
	_ = m.View()
	before := m.streamingRenderCache
	m.activeScreen = screen.NewBodyScreen("/test", "overlay")
	_ = m.View()
	if m.streamingRenderCache != before || !before.valid {
		t.Fatal("opening an overlay discarded a still-active reply layout")
	}
}
