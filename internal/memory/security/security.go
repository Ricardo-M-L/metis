// Package security provides memory content scanning for prompt injection and other attacks.
package security

import (
	"regexp"
	"strings"
)

// ThreatKind classifies the type of security threat detected.
type ThreatKind string

const (
	ThreatInjection  ThreatKind = "prompt_injection"
	ThreatRoleHijack ThreatKind = "role_hijack"
	ThreatCredential ThreatKind = "credential_exposure"
	ThreatBackdoor   ThreatKind = "backdoor"
	ThreatInvisible  ThreatKind = "invisible_chars"
)

// Threat represents a detected security threat.
type Threat struct {
	Kind    ThreatKind `json:"kind"`
	Pattern string     `json:"pattern"`
	Match   string     `json:"match"`
}

// Injection patterns to detect.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(previous|all)\s+(instructions?|commands?)`),
	regexp.MustCompile(`(?i)disregard\s+(your|their)\s+(rules?|instructions?)`),
	regexp.MustCompile(`(?i)forget\s+everything`),
	regexp.MustCompile(`(?i)new\s+instructions?`),
	regexp.MustCompile(`(?i)override\s+(your|their)\s+(safety|security)`),
}

// Role hijack patterns.
var roleHijackPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you\s+are\s+now\s+`),                  // "you are now a different AI"
	regexp.MustCompile(`(?i)pretend\s+you\s+are\s+`),              // "pretend you are"
	regexp.MustCompile(`(?i)as\s+an\s+(AI|LLM|language\s+model)`), // "as an AI, ignore..."
	regexp.MustCompile(`(?i)forget\s+(your|all)\s+(previous|built-in)`),
}

// Credential patterns.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\$` + `[A-Z_][A-Z0-9_]*KEY`),
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token)\s*=\s*['"]?[\w-]{20,}`),
	regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[:=]\s*['"]?.{8,}`),
	regexp.MustCompile(`~/.env`),
	regexp.MustCompile(`\$HOME/.ssh/`),
}

// Backdoor patterns (e.g., authorized_keys tampering).
var backdoorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ssh-rsa\s+[A-Za-z0-9+/]{40,}`), // SSH public key without proper prefix check
	regexp.MustCompile(`authorized_keys`),
	regexp.MustCompile(`(?i)(eval|exec|system)\s*\(`), // Code execution patterns
}

// Invisible character ranges.
var invisibleChars = []rune{
	0x200B, // zero-width space
	0x200C, // zero-width non-joiner
	0x200D, // zero-width joiner
	0xFEFF, // BOM
	0x00A0, // non-breaking space
}

// Scan checks content for potential security issues.
// Returns true if suspicious content is detected.
func Scan(content string) bool {
	return len(ScanAll(content)) > 0
}

// ScanAll checks content for all types of security issues and returns details.
func ScanAll(content string) []Threat {
	var threats []Threat

	if content == "" {
		return threats
	}

	// Check injection patterns
	for _, pat := range injectionPatterns {
		if idx := pat.FindStringIndex(content); idx != nil {
			threats = append(threats, Threat{
				Kind:    ThreatInjection,
				Pattern: pat.String(),
				Match:   content[idx[0]:idx[1]],
			})
		}
	}

	// Check role hijack patterns
	for _, pat := range roleHijackPatterns {
		if idx := pat.FindStringIndex(content); idx != nil {
			threats = append(threats, Threat{
				Kind:    ThreatRoleHijack,
				Pattern: pat.String(),
				Match:   content[idx[0]:idx[1]],
			})
		}
	}

	// Check credential patterns
	for _, pat := range credentialPatterns {
		if idx := pat.FindStringIndex(content); idx != nil {
			threats = append(threats, Threat{
				Kind:    ThreatCredential,
				Pattern: pat.String(),
				Match:   content[idx[0]:idx[1]],
			})
		}
	}

	// Check backdoor patterns
	for _, pat := range backdoorPatterns {
		if idx := pat.FindStringIndex(content); idx != nil {
			threats = append(threats, Threat{
				Kind:    ThreatBackdoor,
				Pattern: pat.String(),
				Match:   content[idx[0]:idx[1]],
			})
		}
	}

	// Check for invisible characters
	for _, char := range invisibleChars {
		if strings.ContainsRune(content, char) {
			threats = append(threats, Threat{
				Kind:    ThreatInvisible,
				Pattern: "invisible char",
				Match:   string(char),
			})
		}
	}

	return threats
}

// HasThreat checks if content contains any threat of the given kind.
func HasThreat(content string, kind ThreatKind) bool {
	for _, t := range ScanAll(content) {
		if t.Kind == kind {
			return true
		}
	}
	return false
}
