package transport

import (
	"errors"
	"strings"
	"testing"
)

func TestRegister_AndLookup(t *testing.T) {
	// Use a unique name to avoid clobbering anything imported by
	// another test in this package.
	const name = "fake_unit_test_transport_xyz"
	defer func() {
		// Best-effort cleanup; package-level registry persists across
		// tests by design.
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	}()

	Register(name, func(opts BuildOpts) (*Result, error) {
		return nil, errors.New("constructor was called")
	})

	c, ok := Lookup(name)
	if !ok {
		t.Fatal("Lookup: not found after Register")
	}
	if _, err := c(BuildOpts{}); err == nil || !strings.Contains(err.Error(), "constructor was called") {
		t.Errorf("constructor not invoked through registry; err=%v", err)
	}
}

func TestLookup_UnknownReturnsFalse(t *testing.T) {
	_, ok := Lookup("definitely-not-registered-anywhere-yyy")
	if ok {
		t.Error("Lookup of unknown transport returned ok=true")
	}
}

func TestNames_IncludesRegistered(t *testing.T) {
	const a = "registry_test_a"
	const b = "registry_test_b"
	defer func() {
		registryMu.Lock()
		delete(registry, a)
		delete(registry, b)
		registryMu.Unlock()
	}()
	Register(a, func(BuildOpts) (*Result, error) { return nil, nil })
	Register(b, func(BuildOpts) (*Result, error) { return nil, nil })

	got := Names()
	gotMap := map[string]bool{}
	for _, n := range got {
		gotMap[n] = true
	}
	if !gotMap[a] || !gotMap[b] {
		t.Errorf("Names() missing test entries; got %v", got)
	}
}

func TestMustBuild_UnknownErrorListsAvailable(t *testing.T) {
	_, err := MustBuild("nonexistent-xyz-zzz", BuildOpts{})
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
	if !strings.Contains(err.Error(), "Available:") {
		t.Errorf("error should list available transports; got %q", err.Error())
	}
}
