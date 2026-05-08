package main

import (
	"strings"
	"testing"
)

func TestLiftBareResume(t *testing.T) {
	cases := []struct {
		in       []string
		want     []string
		wantPick bool
		name     string
	}{
		{
			name:     "explicit_value_short",
			in:       []string{"-r", "abc-123", "hello"},
			want:     []string{"-r", "abc-123", "hello"},
			wantPick: false,
		},
		{
			name:     "explicit_value_long",
			in:       []string{"--resume", "xyz-id"},
			want:     []string{"--resume", "xyz-id"},
			wantPick: false,
		},
		{
			name:     "bare_short_alone",
			in:       []string{"-r"},
			want:     []string{},
			wantPick: true,
		},
		{
			name:     "bare_short_followed_by_flag",
			in:       []string{"-r", "-c"},
			want:     []string{"-c"},
			wantPick: true,
		},
		{
			name:     "bare_long_followed_by_flag",
			in:       []string{"--resume", "--debug"},
			want:     []string{"--debug"},
			wantPick: true,
		},
		{
			name:     "value_then_other_flags",
			in:       []string{"-r", "abc-123", "--bare"},
			want:     []string{"-r", "abc-123", "--bare"},
			wantPick: false,
		},
		{
			name:     "no_resume_at_all",
			in:       []string{"-c", "--debug"},
			want:     []string{"-c", "--debug"},
			wantPick: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pick := false
			got := liftBareResume(tc.in, &pick)
			if pick != tc.wantPick {
				t.Errorf("pick = %v, want %v", pick, tc.wantPick)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseFlags_BareResumeSetsPickFlag(t *testing.T) {
	flags, _, err := parseFlags([]string{"-r"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !flags.pickResume {
		t.Errorf("bare -r should set pickResume=true; got %+v", flags)
	}
	if flags.resumeID != "" {
		t.Errorf("bare -r should NOT set resumeID; got %q", flags.resumeID)
	}
}

func TestParseFlags_ResumeWithIdLeavesPickFalse(t *testing.T) {
	flags, _, err := parseFlags([]string{"-r", "abc-123"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if flags.pickResume {
		t.Errorf("-r abc-123 should NOT set pickResume")
	}
	if flags.resumeID != "abc-123" {
		t.Errorf("expected resumeID=abc-123; got %q", flags.resumeID)
	}
}
