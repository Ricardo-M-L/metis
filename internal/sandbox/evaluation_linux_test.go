//go:build linux

package sandbox

import (
	"slices"
	"testing"
)

func TestEvaluationLinuxStartsEmptyAndDoesNotExposeHostProc(t *testing.T) {
	args := buildEvaluationLinuxArgs("/fixture/work", "/fixture/tests", []string{"/bin/sh", "-c", "true"})
	if !slices.Contains(args, "--unshare-all") || !slices.Contains(args, "--tmpfs") || !slices.Contains(args, "--remount-ro") {
		t.Fatalf("namespace/root isolation missing: %q", args)
	}
	for i, arg := range args {
		if arg == "--proc" || arg == "/proc" {
			t.Fatalf("host process view exposed: %q", args)
		}
		if arg == "--ro-bind" && i+1 < len(args) && args[i+1] == "/" {
			t.Fatalf("host root exposed: %q", args)
		}
	}
	if !slices.Contains(args, "/fixture/tests") || !slices.Contains(args, "/fixture/work") {
		t.Fatalf("fixture mounts missing: %q", args)
	}
}
