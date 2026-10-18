package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopRuntimeCheckUsesBundledPayloadWithoutGUIOrExternalOverride(t *testing.T) {
	r, _, manifest := runtimeTestResolver(t, "linux", "amd64", []byte("fixture runtime"))
	external := filepath.Join(t.TempDir(), "metis")
	if err := os.WriteFile(external, []byte("external"), 0755); err != nil {
		t.Fatal(err)
	}
	r.getenv = func(string) string { return external }
	calls := 0
	var output bytes.Buffer
	handled, err := desktopRuntimeCheckCommand([]string{"desktop", "--runtime-check"}, &output, r, func(ctx context.Context, binary string) (string, error) {
		calls++
		if binary == external || !strings.Contains(binary, manifest.SHA256) {
			t.Fatalf("external executable checked: %s", binary)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("runtime check has no deadline")
		}
		return "v1.2.3 (Metis)", nil
	})
	if err != nil || !handled || calls != 1 {
		t.Fatalf("check handled=%v calls=%d err=%v", handled, calls, err)
	}
	var result desktopRuntimeCheckResult
	if err = json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Source != "bundled" || result.Version != "1.2.3" || result.SHA256 != manifest.SHA256 || !filepath.IsAbs(result.Path) {
		t.Fatalf("check result %+v", result)
	}
}
func TestDesktopRuntimeCheckRejectsMissingTamperedWrongVersionAndExtraArgs(t *testing.T) {
	for _, tc := range []string{"missing", "tampered", "version", "child_error", "changed_during_check", "extra_args"} {
		t.Run(tc, func(t *testing.T) {
			r, _, _ := runtimeTestResolver(t, "linux", "amd64", []byte("fixture runtime"))
			args := []string{"desktop", "--runtime-check"}
			calls := 0
			if tc == "missing" {
				r.payload = nil
			}
			if tc == "tampered" {
				selected, err := r.requireBundled()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(selected.Path, []byte("corrupt"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc == "extra_args" {
				args = append(args, "--metis-bin", "/external")
			}
			var output bytes.Buffer
			handled, err := desktopRuntimeCheckCommand(args, &output, r, func(_ context.Context, binary string) (string, error) {
				calls++
				if tc == "child_error" {
					return "", errors.New("version child failed")
				}
				if tc == "changed_during_check" {
					if err := os.WriteFile(binary, []byte("corrupt"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if tc == "version" {
					return "v9.9.9 (Metis)", nil
				}
				return "v1.2.3 (Metis)", nil
			})
			if !handled || err == nil || output.Len() != 0 {
				t.Fatalf("unsafe check accepted %s: %v %s", tc, err, output.String())
			}
			if (tc == "missing" || tc == "tampered" || tc == "extra_args") && calls != 0 {
				t.Fatalf("executed invalid runtime %s", tc)
			}
		})
	}
	handled, err := desktopRuntimeCheckCommand([]string{"desktop", "--workspace", "/project"}, nil, nil, nil)
	if handled || err != nil {
		t.Fatalf("normal GUI arguments intercepted: %v %v", handled, err)
	}
}
