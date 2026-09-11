package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func typePanelTestText(m *Model, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestEscEmptyPaletteFollowsMainScreenBehaviour(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			m := newSlashTestModel(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if active {
				m.turnActive, m.spinnerActive = true, true
				m.turnCancel = cancel
			}
			const draft = "/zzzzzzzz"
			typePanelTestText(m, draft)
			if len(m.palMatched) != 0 || renderPalette(m) != "" {
				t.Fatal("fixture unexpectedly matched a command")
			}
			if view := stripANSI(m.View().Content); strings.Contains(view, "Esc to close") {
				t.Error("empty completion list advertises a nonexistent panel")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.input.Value() != draft {
				t.Errorf("first Esc discarded the draft without a visible panel: %q", m.input.Value())
			}
			if active {
				if ctx.Err() != context.Canceled || !m.turnCancelledByUser {
					t.Fatal("first Esc with no visible panel did not cancel the task")
				}
			} else {
				m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if m.input.Value() != "" {
					t.Fatal("idle second Esc must retain double-tap draft clearing")
				}
			}
		})
	}
}

func TestPaletteBecomesVisibleAgainAfterBackspacingNoMatch(t *testing.T) {
	m := newSlashTestModel(t)
	typePanelTestText(m, "/modelzzzzzzzz")
	if len(m.palMatched) != 0 {
		t.Fatal("fixture unexpectedly matched a command")
	}
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "Esc to close") || !strings.Contains(stripANSI(renderPalette(m)), "/model") {
		t.Fatalf("backspacing to a known command did not restore visible completion:\n%s", view)
	}
}

func TestHistoryPanelIsVisibleBeforeEscConsumesIt(t *testing.T) {
	for _, layout := range []string{"welcome", "chat", "running"} {
		for _, empty := range []bool{false, true} {
			name := layout + "/matches"
			if empty {
				name = layout + "/empty"
			}
			t.Run(name, func(t *testing.T) {
				m := newSlashTestModel(t)
				if layout == "welcome" {
					m.messages = nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if layout == "running" {
					m.turnActive, m.spinnerActive = true, true
					m.turnCancel = cancel
				}
				m.histAll = []string{"history-visible-fixture"}
				m.input.SetValue("keep my draft")
				m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
				if empty {
					typePanelTestText(m, "zzzzzzzz")
				}
				view := stripANSI(m.View().Content)
				if !strings.Contains(view, "reverse-i-search") || (!empty && !strings.Contains(view, "history-visible-fixture")) || (empty && !strings.Contains(view, "no match")) {
					t.Fatalf("history panel owns input but is not visible:\n%s", view)
				}
				m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if m.showHistory || strings.Contains(stripANSI(m.View().Content), "reverse-i-search") || ctx.Err() != nil || m.input.Value() != "keep my draft" {
					t.Fatal("first Esc must only close visible history, preserving task and draft")
				}
				if layout == "running" {
					m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
					if ctx.Err() != context.Canceled {
						t.Fatal("second Esc must cancel the task")
					}
				}
			})
		}
	}
}

func TestAtMentionPanelIsVisibleBeforeEscConsumesIt(t *testing.T) {
	// Seed the file index only; exercise the real typing/filtering/render path.
	atMentionFilesMu.Lock()
	oldFiles, oldAt := atMentionFiles, atMentionFilesAt
	atMentionFiles, atMentionFilesAt = []string{"panel-visible-fixture.go"}, time.Now()
	atMentionFilesMu.Unlock()
	t.Cleanup(func() {
		atMentionFilesMu.Lock()
		atMentionFiles, atMentionFilesAt = oldFiles, oldAt
		atMentionFilesMu.Unlock()
	})
	for _, layout := range []string{"welcome", "chat", "running"} {
		t.Run(layout, func(t *testing.T) {
			m := newSlashTestModel(t)
			if layout == "welcome" {
				m.messages = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if layout == "running" {
				m.turnActive, m.spinnerActive = true, true
				m.turnCancel = cancel
			}
			const draft = "@panel-visible"
			typePanelTestText(m, draft)
			view := stripANSI(m.View().Content)
			if !strings.Contains(view, "@-mention") || !strings.Contains(view, "panel-visible-fixture.go") {
				t.Fatalf("file completion owns input but is not visible:\n%s", view)
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.atActive || strings.Contains(stripANSI(m.View().Content), "@-mention") || ctx.Err() != nil || m.input.Value() != draft {
				t.Fatal("first Esc must only close visible file completion, preserving task and draft")
			}
			if layout == "running" {
				m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if ctx.Err() != context.Canceled {
					t.Fatal("second Esc must cancel the task")
				}
			}
		})
	}
}
