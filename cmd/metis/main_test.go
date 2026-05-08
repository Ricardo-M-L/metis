package main

// Phase B parse tests — assert each new flag actually lands on cliFlags.
// The original bug was `-r` raising "flag provided but not defined" at
// runtime; the regression test for that is the simplest "parse and check"
// pattern.

import (
	"strings"
	"testing"
)

func TestParseFlags_ResumeShorthand(t *testing.T) {
	flags, rest, err := parseFlags([]string{"-r", "abc-123", "hello world"})
	if err != nil {
		t.Fatalf("parse -r failed: %v", err)
	}
	if flags.resumeID != "abc-123" {
		t.Errorf("expected resumeID=abc-123, got %q", flags.resumeID)
	}
	// Rest is the positional ("hello world") joined by parseFlags's caller.
	if len(rest) == 0 || strings.Join(rest, " ") != "hello world" {
		t.Errorf("expected positional rest=hello world; got %v", rest)
	}
}

func TestParseFlags_ResumeLongFormStillWorks(t *testing.T) {
	// Regression: the short alias must coexist with the long form. They
	// land on the same field, so `--resume foo` after `-r bar` is the
	// one we keep — flag.Parse left-to-right.
	flags, _, err := parseFlags([]string{"--resume", "long-id"})
	if err != nil {
		t.Fatalf("parse --resume failed: %v", err)
	}
	if flags.resumeID != "long-id" {
		t.Errorf("expected --resume to populate resumeID; got %q", flags.resumeID)
	}
}

func TestParseFlags_Continue(t *testing.T) {
	flags, _, err := parseFlags([]string{"-c"})
	if err != nil {
		t.Fatalf("-c parse: %v", err)
	}
	if !flags.cont {
		t.Errorf("-c should set cont=true")
	}
	flags2, _, err := parseFlags([]string{"--continue"})
	if err != nil {
		t.Fatalf("--continue parse: %v", err)
	}
	if !flags2.cont {
		t.Errorf("--continue should set cont=true")
	}
}

func TestParseFlags_Debug(t *testing.T) {
	flags, _, err := parseFlags([]string{"-d"})
	if err != nil {
		t.Fatalf("-d parse: %v", err)
	}
	if !flags.debug {
		t.Errorf("-d should set debug=true")
	}
}

func TestParseFlags_Bare(t *testing.T) {
	flags, _, err := parseFlags([]string{"--bare"})
	if err != nil {
		t.Fatalf("--bare parse: %v", err)
	}
	if !flags.bare {
		t.Errorf("--bare should set bare=true")
	}
}

func TestParseFlags_DangerouslySkipPerms(t *testing.T) {
	flags, _, err := parseFlags([]string{"--dangerously-skip-permissions"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !flags.dangerouslySkipPerms {
		t.Errorf("--dangerously-skip-permissions not parsed")
	}
}

func TestParseFlags_Scope(t *testing.T) {
	flags, _, err := parseFlags([]string{"-s", "user"})
	if err != nil {
		t.Fatalf("-s parse: %v", err)
	}
	if flags.scope != "user" {
		t.Errorf("expected scope=user; got %q", flags.scope)
	}
	flags2, _, err := parseFlags([]string{"--scope", "project"})
	if err != nil {
		t.Fatalf("--scope parse: %v", err)
	}
	if flags2.scope != "project" {
		t.Errorf("expected scope=project; got %q", flags2.scope)
	}
}

func TestParseFlags_IOFormats(t *testing.T) {
	flags, _, err := parseFlags([]string{"--input-format", "json", "--output-format", "stream-json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if flags.inputFormat != "json" {
		t.Errorf("input-format wrong: %q", flags.inputFormat)
	}
	if flags.outputFormat != "stream-json" {
		t.Errorf("output-format wrong: %q", flags.outputFormat)
	}
}

func TestParseFlags_AllPhaseBFlagsCoexist(t *testing.T) {
	// One mega-invocation to make sure no two flags collide on the same
	// short name (a regression I was particularly worried about with
	// -d / -s coexisting with the existing -m / -p / -W).
	args := []string{
		"-c", "-d", "--bare", "-r", "sess-id",
		"--dangerously-skip-permissions",
		"-s", "user",
		"--input-format", "json",
		"--output-format", "json",
		"hello prompt",
	}
	flags, rest, err := parseFlags(args)
	if err != nil {
		t.Fatalf("mega parse: %v", err)
	}
	if !flags.cont || !flags.debug || !flags.bare || !flags.dangerouslySkipPerms {
		t.Errorf("one of the bool flags didn't latch: %+v", flags)
	}
	if flags.resumeID != "sess-id" {
		t.Errorf("resumeID lost: %q", flags.resumeID)
	}
	if flags.scope != "user" {
		t.Errorf("scope lost: %q", flags.scope)
	}
	if strings.Join(rest, " ") != "hello prompt" {
		t.Errorf("rest lost: %v", rest)
	}
}
