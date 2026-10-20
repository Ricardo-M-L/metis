//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

func TestEvaluationDarwinPolicyHasNoBroadHostReadsOrIPC(t *testing.T) {
	profile := buildEvaluationDarwinProfile("/private/fixture/work", "/private/fixture/tests")
	for _, disallowed := range []string{"(allow file-read*)", "(allow network", "(allow mach-lookup", "(allow iokit-open", "(allow sysctl-read)", "(subpath \"/Users\")", "(subpath \"/private\")", "(subpath \"/private/var\")", "(subpath \"/var\")", "(subpath \"/proc\")"} {
		if strings.Contains(profile, disallowed) {
			t.Fatalf("profile has broad privilege %q: %s", disallowed, profile)
		}
	}
	for _, selector := range []string{`(literal "/private/var/select/sh")`, `(literal "/var/select/sh")`} {
		if !strings.Contains(profile, selector) {
			t.Fatalf("fixed system shell selector unavailable: %s", selector)
		}
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/private/fixture/work"))`) {
		t.Fatal("workspace is not writable")
	}
	if strings.Contains(profile, `(allow file-write* (subpath "/private/fixture/tests"))`) {
		t.Fatal("host tests are writable")
	}
}
