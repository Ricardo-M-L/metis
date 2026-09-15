package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
)

func TestManagedComputerUseDoesNotReplaceCustomServer(t *testing.T) {
	reg := &Registry{Servers: []ServerEntry{{Name: ReservedComputerUseName, Command: "/custom/metis-cu"}}}
	if err := SetManagedComputerUseServer(reg); err == nil {
		t.Fatal("silently replaced custom Computer Use server")
	}
	if reg.Servers[0].Command != "/custom/metis-cu" {
		t.Fatal("modified custom entry")
	}
}

func TestManagedComputerUseEntryRejectsTransportOverrides(t *testing.T) {
	for _, entry := range []ServerEntry{
		{Name: "other", Command: ManagedComputerUseCommand},
		{Name: ReservedComputerUseName, Command: ManagedComputerUseCommand, URL: "http://localhost:1234"},
		{Name: ReservedComputerUseName, Command: ManagedComputerUseCommand, Args: []string{"--evil"}},
		{Name: ReservedComputerUseName, Command: ManagedComputerUseCommand, Env: map[string]string{"METIS_CU_HOST_TERMINAL_TIER": "full"}},
	} {
		_, err := prepareManagedComputerUseEntry(context.Background(), entry)
		if err == nil || !strings.Contains(err.Error(), "managed Computer Use") {
			t.Fatalf("override accepted: %#v: %v", entry, err)
		}
	}
}

func TestManagedComputerUseRegistrationIsIdempotent(t *testing.T) {
	reg := &Registry{}
	if err := SetManagedComputerUseServer(reg); err != nil {
		t.Fatal(err)
	}
	reg.Servers[0].Disabled = true
	reg.Servers[0].DisabledTools = []string{"macro"}
	if err := SetManagedComputerUseServer(reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Servers) != 1 || reg.Servers[0].Disabled || len(reg.Servers[0].DisabledTools) != 1 {
		t.Fatalf("unexpected state: %#v", reg)
	}
}

func TestComputerUseInputOwnershipProfileRequiresManagedProvenance(t *testing.T) {
	legacyEnv, legacy := stdioLaunchEnvAndProfile(ServerEntry{Name: ReservedComputerUseName, Command: ReservedComputerUseBinary})
	if legacy != mcpsdk.StdioSandboxProfileComputerUse || envMapFromSlice(legacyEnv)["METIS_CU_HOST_TERMINAL_TIER"] != "full" {
		t.Fatal("legacy Computer Use environment compatibility changed")
	}
	_, managed := stdioLaunchEnvAndProfile(ServerEntry{Name: ReservedComputerUseName, Command: "/managed/versions/test/metis-cu", managedComputerUse: true})
	if managed != mcpsdk.StdioSandboxProfileManagedComputerUse {
		t.Fatal("verified managed entry did not select the ownership profile")
	}
	for _, entry := range []ServerEntry{
		{Name: ReservedComputerUseName, Command: ReservedComputerUseBinary},
		{Name: ReservedComputerUseName, Command: "/managed/versions/test/metis-cu"},
		{Name: ReservedComputerUseName, Command: ReservedComputerUseBinary, Env: map[string]string{"METIS_CU_MANAGED": "1"}},
	} {
		_, profile := stdioLaunchEnvAndProfile(entry)
		if profile == mcpsdk.StdioSandboxProfileManagedComputerUse {
			t.Fatalf("name, path or environment forged managed provenance: %+v", entry)
		}
	}
}
