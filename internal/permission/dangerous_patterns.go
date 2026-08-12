package permission

// dangerous_patterns.go — hard-coded pre-filter that hard-denies a
// curated set of "no defensible reason for an agent to ever run
// this" command shapes BEFORE the YOLO classifier (LLM round-trip)
// gets involved.
//
// Why pre-filter: an LLM classifier that returns ALLOW for one of
// these patterns is almost certainly hallucinated; we'd rather
// short-circuit the call entirely than trust a 0.5%-failure-rate
// LLM with the keys to `rm -rf /`. Mirrors openclaude's
// `yoloClassifier.ts:DANGEROUS_PATTERNS` and claude-code-sourcemap's
// command-blacklist.
//
// All patterns are case-insensitive substring checks (cheap). The
// match is intentionally generous — an attacker who reshapes the
// command can probably evade us, but the goal is to catch the
// obvious shapes the model itself might propose ("hey, let me clean
// up this disk!") so a moment of thoughtlessness doesn't become a
// catastrophe.

import "strings"

// DangerousPattern is one entry in the pre-filter list. Substring +
// human-friendly reason for the audit log.
type DangerousPattern struct {
	Substr string
	Reason string
}

// dangerousPatterns is the hard-deny list. New entries should be
// well-justified ("why would an agent EVER need this?") and tested
// via dangerous_patterns_test.go.
var dangerousPatterns = []DangerousPattern{
	// Filesystem-mass-deletion family.
	{Substr: "rm -rf /", Reason: "rm -rf / wipes the filesystem"},
	{Substr: "rm -fr /", Reason: "rm -fr / wipes the filesystem"},
	{Substr: "rm -rf ~", Reason: "rm -rf ~ wipes the user's home dir"},
	{Substr: "rm -rf $home", Reason: "rm -rf $HOME wipes the user's home dir"},
	{Substr: "rm -rf /*", Reason: "rm -rf /* wipes the filesystem"},
	{Substr: "rm -rf .", Reason: "rm -rf . wipes the working dir"},

	// Disk / device-level destruction.
	{Substr: "dd if=/dev/zero of=/", Reason: "dd zeroing the root device"},
	{Substr: "dd if=/dev/random of=/", Reason: "dd randomizing the root device"},
	{Substr: "mkfs", Reason: "mkfs reformats a filesystem"},
	{Substr: "shred /", Reason: "shred on /"},
	{Substr: "wipe /", Reason: "wipe on /"},

	// Forking-bomb / DOS shapes.
	{Substr: ":(){ :|:& };:", Reason: "fork bomb"},

	// Credential / secret exfil shapes.
	{Substr: "curl -k ", Reason: "curl -k disables TLS verification"},
	{Substr: "wget --no-check-certificate", Reason: "wget --no-check-certificate disables TLS"},
	{Substr: "/.ssh/id_", Reason: "reading private SSH key files"},
	{Substr: "~/.ssh/id_", Reason: "reading private SSH key files"},
	{Substr: "$home/.ssh/id_", Reason: "reading private SSH key files"},
	{Substr: "/.aws/credentials", Reason: "reading AWS credentials file"},
	{Substr: "/.gnupg/", Reason: "reading GnuPG private keyring"},

	// Dangerous git ops on protected branches.
	{Substr: "git push --force origin main", Reason: "force-push to main"},
	{Substr: "git push -f origin main", Reason: "force-push to main"},
	{Substr: "git push --force origin master", Reason: "force-push to master"},
	{Substr: "git push -f origin master", Reason: "force-push to master"},

	// Privilege-escalation invocations.
	{Substr: "sudo rm -rf", Reason: "sudo rm -rf"},
	{Substr: "sudo dd if=/dev/", Reason: "sudo dd to a device"},
	{Substr: "sudo mkfs", Reason: "sudo mkfs"},
	{Substr: "sudo systemctl mask", Reason: "sudo systemctl mask"},

	// Remote-shell exfil shapes.
	{Substr: "nc -e /bin/sh", Reason: "netcat with -e (reverse shell)"},
	{Substr: "nc -e /bin/bash", Reason: "netcat with -e (reverse shell)"},
	{Substr: "bash -i >& /dev/tcp/", Reason: "/dev/tcp reverse shell"},
}

// CheckDangerousPattern scans the canonicalised command/argument
// string for any DangerousPattern match. Returns the first match (or
// nil when the input is clean). Case-insensitive — attackers love
// `Rm -RF /` etc.
//
// stringInput is the same value the gate already canonicalises for
// classifier input — typically the Bash command, or the
// concatenated args of an arbitrary tool.
func CheckDangerousPattern(stringInput string) *DangerousPattern {
	if stringInput == "" {
		return nil
	}
	low := strings.ToLower(stringInput)
	// Collapse runs of whitespace (spaces, tabs, newlines) to a single space
	// so trivial padding can't evade the substring table: `rm  -rf  /`,
	// `rm -rf\t/`, and `rm -rf\n/` all canonicalise to `rm -rf /` and match.
	// The table's patterns are written with single spaces.
	canon := strings.Join(strings.Fields(low), " ")
	for i := range dangerousPatterns {
		sub := dangerousPatterns[i].Substr
		if strings.Contains(low, sub) || strings.Contains(canon, sub) {
			return &dangerousPatterns[i]
		}
	}
	return nil
}

// DangerousPatternsCount exposes the table size for tests / docs.
// Read-only; the table itself is package-private to keep callers
// using the canonicalised CheckDangerousPattern API.
func DangerousPatternsCount() int { return len(dangerousPatterns) }
