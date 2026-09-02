package main

import (
	"image"
	"image/color"
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
		{0.50, 0.10}, {0.15, 0.15}, {0.85, 0.15},
		{0.10, 0.35}, {0.90, 0.35}, {0.10, 0.50},
		{0.90, 0.50}, {0.15, 0.85}, {0.85, 0.85},
		{0.30, 0.88}, {0.70, 0.88}, {0.50, 0.95},
	}
	for _, point := range points {
		x := bounds.Min.X + int(point[0]*float64(bounds.Dx()-1))
		y := bounds.Min.Y + int(point[1]*float64(bounds.Dy()-1))
		r, g, b, a := icon.At(x, y).RGBA()
		if a != 0xffff || r != 0xffff || g != 0xffff || b != 0xffff {
			t.Fatalf("app icon background at (%d,%d) is not opaque pure white: rgba16=(%d,%d,%d,%d)", x, y, r, g, b, a)
		}
	}
	// The plate is intentionally flat white. A neutral non-white pixel would
	// reintroduce the gray bevel/shadow that becomes especially visible after
	// macOS scales the icon down in the Dock. Colored pixels belong to the mark.
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := color.NRGBAModel.Convert(icon.At(x, y)).(color.NRGBA)
			if pixel.A != 0 && pixel.R == pixel.G && pixel.G == pixel.B && pixel.R != 0xff {
				t.Fatalf("app icon contains neutral gray at (%d,%d): nrgba8=%v", x, y, pixel)
			}
		}
	}
}
