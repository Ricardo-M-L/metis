package main

// dispatch_test.go pins the routing rules that decide chat vs run when
// the user didn't type an explicit subcommand. The 2026-05-08 user
// video showed `metis -r` falling into cmdRun and emitting
// `metis: run: prompt is required`, because the bare resume gesture
// looked like "no subcommand" and the default fallback was cmdRun.
// We special-case the interactive-intent flags here.

import "testing"

func TestHasInteractiveIntentFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare -r alone", []string{"-r"}, true},
		{"bare --resume alone", []string{"--resume"}, true},
		{"-r with uuid", []string{"-r", "abc-123-def"}, true},
		{"--resume with uuid", []string{"--resume", "abc-123-def"}, true},
		{"--resume=xyz form", []string{"--resume=abc"}, true},
		{"-c alone", []string{"-c"}, true},
		{"--continue alone", []string{"--continue"}, true},
		{"--continue=true form", []string{"--continue=true"}, true},
		{"-r mixed with other flags", []string{"-m", "gpt-4", "-r"}, true},
		{"-m alone (model is not interactive intent)", []string{"-m", "gpt-4"}, false},
		{"-p alone (provider is not interactive intent)", []string{"-p", "openai"}, false},
		{"plain prompt", []string{"explain", "this"}, false},
		{"prompt that contains -r as text", []string{"give -r flag docs"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasInteractiveIntentFlag(tc.args)
			if got != tc.want {
				t.Errorf("hasInteractiveIntentFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
