package tui

// Flag parsing for `/mcp add --env KEY=VAL …`. The function exists in
// commands.go alongside the cmdMCP dispatch; we keep parsing logic
// dumb-and-pure so it can be exercised without the full TUI surface.

import (
	"testing"
)

func TestParseMCPAddFlags_NoFlags(t *testing.T) {
	env, rest, err := parseMCPAddFlags([]string{"myserver", "node", "server.js"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("env should be empty; got %v", env)
	}
	if len(rest) != 3 || rest[0] != "myserver" {
		t.Errorf("rest should be passthrough; got %v", rest)
	}
}

func TestParseMCPAddFlags_SingleEnv(t *testing.T) {
	env, rest, err := parseMCPAddFlags([]string{"--env", "KEY=VAL", "fc", "node"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if env["KEY"] != "VAL" {
		t.Errorf("env not parsed; got %v", env)
	}
	if len(rest) != 2 || rest[0] != "fc" {
		t.Errorf("rest after flags wrong; got %v", rest)
	}
}

func TestParseMCPAddFlags_RepeatedEnv(t *testing.T) {
	env, _, err := parseMCPAddFlags([]string{
		"--env", "A=1", "--env", "B=2", "--env=C=3", "name", "cmd",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for k, want := range map[string]string{"A": "1", "B": "2", "C": "3"} {
		if env[k] != want {
			t.Errorf("env[%s]=%q want %q", k, env[k], want)
		}
	}
}

func TestParseMCPAddFlags_BadFormat(t *testing.T) {
	if _, _, err := parseMCPAddFlags([]string{"--env", "BADFORMAT", "name"}); err == nil {
		t.Errorf("missing = should error")
	}
	if _, _, err := parseMCPAddFlags([]string{"--env", "=val", "name"}); err == nil {
		t.Errorf("empty key should error")
	}
}

func TestParseMCPAddFlags_MissingValue(t *testing.T) {
	if _, _, err := parseMCPAddFlags([]string{"--env"}); err == nil {
		t.Errorf("dangling --env should error")
	}
}

func TestParseMCPAddFlags_EmptyValueAllowed(t *testing.T) {
	// `KEY=` is valid — drops the var to empty string explicitly. The
	// spawned subprocess sees an empty value, which is sometimes what
	// you want (e.g., to unset a parent env var by override).
	env, _, err := parseMCPAddFlags([]string{"--env", "DROP=", "name", "cmd"})
	if err != nil {
		t.Fatalf("KEY= should be allowed; got %v", err)
	}
	if v, ok := env["DROP"]; !ok || v != "" {
		t.Errorf("DROP should be present with empty value; got ok=%v v=%q", ok, v)
	}
}
