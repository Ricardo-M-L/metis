package skills

import (
	"strings"
	"testing"
)

func TestExpandTemplateVars_NoVarsNoop(t *testing.T) {
	in := "## Skill body\nNo template references here."
	got := ExpandTemplateVars(in, "/some/dir", "session-1")
	if got != in {
		t.Errorf("expected unchanged; got %q", got)
	}
}

func TestExpandTemplateVars_MetisSkillDir(t *testing.T) {
	in := "Read ${METIS_SKILL_DIR}/template.json for the schema."
	got := ExpandTemplateVars(in, "/opt/skills/foo", "sid")
	want := "Read /opt/skills/foo/template.json for the schema."
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestExpandTemplateVars_ClaudeSkillDirAlias(t *testing.T) {
	// Paste-compat with claude-code: ${CLAUDE_SKILL_DIR} works too.
	in := "cd ${CLAUDE_SKILL_DIR} && ls"
	got := ExpandTemplateVars(in, "/x/y", "")
	if !strings.Contains(got, "/x/y") || strings.Contains(got, "${CLAUDE_SKILL_DIR}") {
		t.Errorf("alias not substituted; got %q", got)
	}
}

func TestExpandTemplateVars_SessionID(t *testing.T) {
	in := "session=${METIS_SESSION_ID} alt=${CLAUDE_SESSION_ID}"
	got := ExpandTemplateVars(in, "", "s-42")
	want := "session=s-42 alt=s-42"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestExpandTemplateVars_UnknownVarLeftIntact(t *testing.T) {
	// Other ${...} tokens (env vars the agent or shell will resolve)
	// must pass through untouched.
	in := "user=${USER} skill=${METIS_SKILL_DIR}"
	got := ExpandTemplateVars(in, "/d", "")
	if !strings.Contains(got, "${USER}") {
		t.Errorf("unknown var should pass through; got %q", got)
	}
	if !strings.Contains(got, "/d") {
		t.Errorf("known var not substituted; got %q", got)
	}
}

func TestExpandTemplateVars_EmptyValueSkipsSubstitution(t *testing.T) {
	// Passing "" for sessionID should leave the placeholder alone —
	// inserting an empty string would break shell quoting and is
	// almost never what the author wanted (claude-code parity).
	in := "id=${METIS_SESSION_ID}"
	got := ExpandTemplateVars(in, "/d", "")
	if !strings.Contains(got, "${METIS_SESSION_ID}") {
		t.Errorf("empty sessionID should leave placeholder; got %q", got)
	}
}

func TestExpandTemplateVars_RepeatedReferences(t *testing.T) {
	in := "${METIS_SKILL_DIR} and ${METIS_SKILL_DIR}"
	got := ExpandTemplateVars(in, "/d", "")
	if strings.Count(got, "/d") != 2 {
		t.Errorf("both occurrences must substitute; got %q", got)
	}
}
