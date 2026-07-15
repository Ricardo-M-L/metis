package tui

import "testing"

// TestParsePriorityCommand_NowFormats — every reasonable shape of
// `/now <body>` should match. Tab as separator is tolerated because
// keyboard shortcuts sometimes insert a tab; bare `/now` (no body)
// is recognised so the empty-body hint can fire.
func TestParsePriorityCommand_NowFormats(t *testing.T) {
	cases := []struct {
		in   string
		body string
	}{
		{"/now write tests", "write tests"},
		{"/now   leading spaces", "leading spaces"},
		{"/now\trun the pipeline", "run the pipeline"},
		{"/now actually look at deploy.yml first", "actually look at deploy.yml first"},
		{"/now", ""},
		{"/now ", ""},
	}
	for _, c := range cases {
		prio, body, ok := parsePriorityCommand(c.in)
		if !ok {
			t.Errorf("parsePriorityCommand(%q) = !ok; want match", c.in)
			continue
		}
		if prio != QueuePriorityNow {
			t.Errorf("parsePriorityCommand(%q): prio = %d, want Now (%d)", c.in, prio, QueuePriorityNow)
		}
		if body != c.body {
			t.Errorf("parsePriorityCommand(%q): body = %q, want %q", c.in, body, c.body)
		}
	}
}

// TestParsePriorityCommand_LaterFormats — symmetric coverage for the
// /later variant.
func TestParsePriorityCommand_LaterFormats(t *testing.T) {
	prio, body, ok := parsePriorityCommand("/later if you have time, also check X")
	if !ok || prio != QueuePriorityLater || body != "if you have time, also check X" {
		t.Errorf("/later parse wrong: ok=%v prio=%d body=%q", ok, prio, body)
	}
}

// TestParsePriorityCommand_Misses — inputs that LOOK similar but
// shouldn't trigger the priority path: substrings, body without
// whitespace separator, unrelated slash commands. The boundary cases
// matter because `/nowhere` or `/laterally` would otherwise eat the
// command name.
func TestParsePriorityCommand_Misses(t *testing.T) {
	for _, in := range []string{
		"",
		"/nowhere",             // continuation after /now
		"/laterally important", // continuation after /later
		"/notify me",
		"/help",
		"now write tests", // no leading slash
		"hello /now world",
		"/Now uppercase", // case-sensitive
	} {
		if _, _, ok := parsePriorityCommand(in); ok {
			t.Errorf("parsePriorityCommand(%q) matched; want miss", in)
		}
	}
}

// TestParsePriorityCommand_DrainHonoursPriority — end-to-end sanity:
// /now-parsed item really does get drained before /later-parsed item
// even when /later was enqueued first.
func TestParsePriorityCommand_DrainHonoursPriority(t *testing.T) {
	m := &Model{}

	// User typed /later first while one Next was queued, then /now.
	m.enqueueQueuedItem("next-msg", QueuePriorityNext)

	prio, body, _ := parsePriorityCommand("/later catch up on this when free")
	m.enqueueQueuedItem(body, prio)

	prio, body, _ = parsePriorityCommand("/now interrupt — try X first")
	m.enqueueQueuedItem(body, prio)

	// Drain order: Now → Next → Later.
	text, _ := m.drainNextQueuedBatch()
	if text != "interrupt — try X first" {
		t.Errorf("first drain should be the /now item; got %q", text)
	}
	text, _ = m.drainNextQueuedBatch()
	if text != "next-msg" {
		t.Errorf("second drain should be the default-priority Next item; got %q", text)
	}
	text, _ = m.drainNextQueuedBatch()
	if text != "catch up on this when free" {
		t.Errorf("third drain should be the /later item; got %q", text)
	}
}

// TestPrioCommandName — names match what the parser recognises so
// error messages stay consistent.
func TestPrioCommandName(t *testing.T) {
	if got := prioCommandName(QueuePriorityNow); got != "/now" {
		t.Errorf("Now name = %q, want /now", got)
	}
	if got := prioCommandName(QueuePriorityLater); got != "/later" {
		t.Errorf("Later name = %q, want /later", got)
	}
	if got := prioCommandName(QueuePriorityNext); got != "" {
		t.Errorf("Next has no UI name; got %q", got)
	}
}
