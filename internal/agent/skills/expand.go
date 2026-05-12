package skills

import "strings"

// expand.go — template-variable expansion for SKILL.md prompt bodies.
// Mirrors claude-code's loadSkillsDir.ts:340-410 substitution layer:
// the skill author can reference their own directory and the active
// session id from inside the prompt without having to hardcode paths.
//
// Variables (both ${METIS_*} and ${CLAUDE_*} forms accepted — the
// latter lets users paste a SKILL.md straight from claude-code without
// editing it):
//
//   ${METIS_SKILL_DIR}    → the directory containing the SKILL.md
//                          (useful for `cat ${METIS_SKILL_DIR}/template.json`
//                          style references to skill-local assets)
//   ${CLAUDE_SKILL_DIR}   → alias for METIS_SKILL_DIR
//   ${METIS_SESSION_ID}   → current chat session id (when known)
//   ${CLAUDE_SESSION_ID}  → alias for METIS_SESSION_ID
//
// Unknown ${…} sequences are left intact so the agent / shell can
// still see them (helpful for skills that intentionally generate code
// referencing other tools' variables).

// ExpandTemplateVars returns body with every recognised template
// variable replaced. Empty inputs short-circuit — the common case is
// a skill that uses no variables, and we don't want to allocate.
//
// skillDir and sessionID are the substitution values; pass "" to
// suppress that particular variable (the placeholder stays as-is in
// that case, which is also what claude-code does — better than
// silently inserting "" and breaking shell quoting).
func ExpandTemplateVars(body, skillDir, sessionID string) string {
	if body == "" {
		return body
	}
	if !strings.Contains(body, "${") {
		return body
	}
	out := body
	if skillDir != "" {
		out = strings.ReplaceAll(out, "${METIS_SKILL_DIR}", skillDir)
		out = strings.ReplaceAll(out, "${CLAUDE_SKILL_DIR}", skillDir)
	}
	if sessionID != "" {
		out = strings.ReplaceAll(out, "${METIS_SESSION_ID}", sessionID)
		out = strings.ReplaceAll(out, "${CLAUDE_SESSION_ID}", sessionID)
	}
	return out
}
