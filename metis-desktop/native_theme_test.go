package main

import "testing"

func TestNativeThemeRejectsUnknownValue(t *testing.T) {
	if err := (&App{}).SetNativeTheme("sepia"); err == nil {
		t.Fatal("SetNativeTheme accepted an unsupported theme")
	}
}
