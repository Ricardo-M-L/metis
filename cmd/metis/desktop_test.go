package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParseDesktopOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		web  bool
		port string
		err  bool
	}{
		{name: "native default", port: "8080"},
		{name: "native ignores web port environment", env: "9090", port: "8080"},
		{name: "web environment", args: []string{"--web"}, env: "9090", web: true, port: "9090"},
		{name: "flag implies web and wins", args: []string{"--port", "7070"}, env: "9090", web: true, port: "7070"},
		{name: "missing value", args: []string{"-p"}, err: true},
		{name: "not numeric", args: []string{"-p", "abc"}, err: true},
		{name: "out of range", args: []string{"-p", "70000"}, err: true},
		{name: "unknown", args: []string{"--wat"}, err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDesktopOptions(tc.args, func(string) string { return tc.env })
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error %v", err, tc.err)
			}
			if !tc.err && (got.web != tc.web || got.port != tc.port) {
				t.Fatalf("options = %+v, want web=%v port=%q", got, tc.web, tc.port)
			}
		})
	}
}

func TestCmdDesktopDefaultsToNativeWorkspace(t *testing.T) {
	old := launchNativeDesktop
	t.Cleanup(func() { launchNativeDesktop = old })

	var got string
	launchNativeDesktop = func(workspace string) error {
		got = workspace
		return nil
	}
	if err := cmdDesktop(context.Background(), nil); err != nil {
		t.Fatalf("cmdDesktop: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Fatalf("native workspace = %q, want absolute cwd", got)
	}
}
