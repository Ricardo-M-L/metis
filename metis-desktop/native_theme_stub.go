//go:build !darwin

package main

import "fmt"

func applyNativeTheme(theme string) error {
	switch theme {
	case "auto", "light", "dark":
		return nil
	default:
		return fmt.Errorf("unsupported native theme %q", theme)
	}
}
