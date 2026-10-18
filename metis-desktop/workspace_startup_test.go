package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkspaceStartupDefaults(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user-home")
	project := filepath.Join(t.TempDir(), "project")
	root := filepath.VolumeName(project) + string(filepath.Separator)
	for _, tc := range []struct {
		name    string
		args    []string
		cwd     string
		cwdErr  error
		homeErr error
		want    string
	}{
		{name: "normal development directory", cwd: project, want: project},
		{name: "Finder filesystem root", cwd: root, want: home},
		{name: "working directory unavailable", cwdErr: errors.New("removed cwd"), want: home},
		{name: "empty working directory", want: home},
		{name: "explicit root is preserved", args: []string{"desktop", "--workspace", root}, cwd: project, want: root},
		{name: "explicit equals directory", args: []string{"desktop", "--workspace=" + project}, cwd: root, want: project},
		{name: "no usable default", cwdErr: errors.New("removed cwd"), homeErr: errors.New("missing home"), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = []string{"desktop"}
			}
			got := parseWorkspaceArgWithDirectories(args, func() (string, error) { return tc.cwd, tc.cwdErr }, func() (string, error) { return home, tc.homeErr })
			if got != tc.want {
				t.Fatalf("workspace = %q, want %q", got, tc.want)
			}
		})
	}
}
