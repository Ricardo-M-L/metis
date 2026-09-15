package main

import (
	"os"
	"strings"
	"testing"
)

func TestFrontendThemeBridgeControlsNativeWindowAppearance(t *testing.T) {
	body, err := os.ReadFile("frontend/src/main.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, want := range []string{
		"SetNativeTheme",
		"'set-theme': payload =>",
		"return SetNativeTheme(theme)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("frontend theme bridge missing %q", want)
		}
	}
}
