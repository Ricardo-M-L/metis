package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputerUseBundleEnvironmentUsesHostResources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metis.app", "Contents")
	bundle := filepath.Join(root, "Resources", "computer-use")
	if err := os.MkdirAll(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "MacOS", "metis-desktop")
	got := computerUseBundleEnvironment([]string{"OTHER=retained", "METIS_CU_BUNDLE_DIR=/unrelated"}, exe)
	if strings.Join(got, "\n") != "OTHER=retained\nMETIS_CU_BUNDLE_DIR="+bundle {
		t.Fatalf("environment: %v", got)
	}
}

func TestComputerUseBundleEnvironmentDropsStaleHint(t *testing.T) {
	for _, exe := range []string{"", "/tmp/metis-desktop", filepath.Join(t.TempDir(), "Metis.app", "Contents", "MacOS", "metis-desktop")} {
		got := computerUseBundleEnvironment([]string{"OTHER=retained", "METIS_CU_BUNDLE_DIR=/unrelated"}, exe)
		if len(got) != 1 || got[0] != "OTHER=retained" {
			t.Fatalf("unexpected hint: %v", got)
		}
	}
}
