package main

import "testing"

func TestAgentTeamsFlagActivatesCoordinatorRuntime(t *testing.T) {
	flags, rest, err := parseFlags([]string{"--agent-teams"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected args: %v", rest)
	}
	if !flags.agentTeams || !flags.coordinator {
		t.Fatalf("--agent-teams flags = %+v", flags)
	}
}
