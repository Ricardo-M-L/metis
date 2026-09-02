package main

import (
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestAppIconHasOneCanonicalTransparentAsset(t *testing.T) {
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
		{0.05, 0.05}, {0.50, 0.05}, {0.95, 0.05},
		{0.05, 0.50}, {0.95, 0.50},
		{0.05, 0.95}, {0.50, 0.95}, {0.95, 0.95},
	}
	for _, point := range points {
		x := bounds.Min.X + int(point[0]*float64(bounds.Dx()-1))
		y := bounds.Min.Y + int(point[1]*float64(bounds.Dy()-1))
		_, _, _, a := icon.At(x, y).RGBA()
		if a != 0 {
			t.Fatalf("app icon background at (%d,%d) is not transparent: alpha16=%d", x, y, a)
		}
	}
	// A visible neutral pixel would reintroduce the white plate or gray shadow
	// that macOS emphasizes after scaling the icon down in the Dock. Only the
	// blue/cyan/purple mark is allowed to remain visible.
	visiblePixels := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := color.NRGBAModel.Convert(icon.At(x, y)).(color.NRGBA)
			if pixel.A == 0 {
				continue
			}
			visiblePixels++
			maxChannel := max(pixel.R, pixel.G, pixel.B)
			minChannel := min(pixel.R, pixel.G, pixel.B)
			if maxChannel-minChannel <= 2 {
				t.Fatalf("app icon contains a visible neutral plate/shadow pixel at (%d,%d): nrgba8=%v", x, y, pixel)
			}
		}
	}
	if visiblePixels < bounds.Dx()*bounds.Dy()/10 {
		t.Fatalf("app icon mark is unexpectedly sparse: %d visible pixels", visiblePixels)
	}
}
