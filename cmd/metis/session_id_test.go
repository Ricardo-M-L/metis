package main

import "testing"

func TestValidExplicitSessionID(t *testing.T) {
	for _, id := range []string{
		"desktop-0123456789abcdef",
		"550e8400-e29b-41d4-a716-446655440000",
		"native_client.v2_1",
	} {
		if !validExplicitSessionID(id) {
			t.Errorf("validExplicitSessionID(%q) = false", id)
		}
	}
	for _, id := range []string{
		"", ".", "..", "../escape", `..\escape`, "nested/id",
		" leading", "trailing ", "line\nbreak", "中文", "$shell",
	} {
		if validExplicitSessionID(id) {
			t.Errorf("validExplicitSessionID(%q) = true", id)
		}
	}
}

func TestParseFlagsAcceptsExplicitSessionID(t *testing.T) {
	flags, rest, err := parseFlags([]string{"--session-id", "desktop-abc", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if flags.newSessionID != "desktop-abc" || len(rest) != 1 || rest[0] != "hello" {
		t.Fatalf("flags=%+v rest=%q", flags, rest)
	}
}
