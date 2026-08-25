package main

import (
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestAppIconHasOneCanonicalWhiteBackgroundAsset(t *testing.T) {
	for _, obsolete := range []string{
		"build/appicon-generated-source.png",
		"build/appicon-previous.png",
		"build/appicon-white-generated-source.png",
	} {
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Fatalf("obsolete app icon asset %s must be removed; stat error: %v", obsolete, err)
		}
	}

	file, err := os.Open(filepath.Join("build", "appicon.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	icon, _, err := image.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	bounds := icon.Bounds()
	points := [][2]float64{
		{0.50, 0.10}, {0.18, 0.20}, {0.82, 0.20},
		{0.10, 0.50}, {0.90, 0.50}, {0.50, 0.90},
	}
	for _, point := range points {
		x := bounds.Min.X + int(point[0]*float64(bounds.Dx()-1))
		y := bounds.Min.Y + int(point[1]*float64(bounds.Dy()-1))
		r, g, b, a := icon.At(x, y).RGBA()
		if a < 0xf000 || r < 0xd000 || g < 0xd000 || b < 0xd000 {
			t.Fatalf("app icon background at (%d,%d) is not opaque white: rgba16=(%d,%d,%d,%d)", x, y, r, g, b, a)
		}
	}
}
