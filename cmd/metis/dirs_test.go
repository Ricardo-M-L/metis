package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDirs_DefaultLayout(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := resolveDirs()
	for _, key := range []string{"config", "data", "sessions", "logs"} {
		if _, ok := d[key]; !ok {
			t.Errorf("resolveDirs missing key %q", key)
		}
	}
	if !strings.HasSuffix(d["config"], "config.toml") {
		t.Errorf("config path should end with config.toml; got %q", d["config"])
	}
	if !strings.HasSuffix(d["sessions"], filepath.Join("sessions")) {
		t.Errorf("sessions path should end with sessions/; got %q", d["sessions"])
	}
}

func TestPrintDirsAll_NonTTY(t *testing.T) {
	dirs := map[string]string{
		"config":   "/x/config.toml",
		"data":     "/x",
		"sessions": "/x/sessions",
		"logs":     "/x/logs",
	}
	var buf bytes.Buffer
	printDirsAll(&buf, dirs)
	out := buf.String()
	// Non-TTY: tab-separated. The "config\t/x/config.toml" line MUST be present.
	for _, want := range []string{"config\t/x/config.toml", "data\t/x", "sessions\t/x/sessions", "logs\t/x/logs"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestCmdDirs_UnknownKeyErrors(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	err := cmdDirs([]string{"nope"})
	if err == nil {
		t.Fatal("unknown key should error")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key; got %v", err)
	}
}
