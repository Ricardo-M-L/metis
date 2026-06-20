package permission

import "testing"

// Whitespace padding must not evade the dangerous-pattern blacklist.
func TestCheckDangerousPattern_WhitespaceEvasion(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf /",
		"rm  -rf  /",   // double spaces
		"rm -rf\t/",    // tab
		"rm -rf\n/",    // newline
		"RM   -RF   /", // case + spaces
	} {
		if CheckDangerousPattern(cmd) == nil {
			t.Errorf("CheckDangerousPattern(%q) = nil, want a match (evasion)", cmd)
		}
	}
	// A benign command must NOT match.
	if CheckDangerousPattern("git status && ls -la") != nil {
		t.Errorf("benign command should not match any dangerous pattern")
	}
}
