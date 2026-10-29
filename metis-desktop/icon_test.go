package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestAppIconHasTransparentMarginWithoutBackplate(t *testing.T) {
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
	assertBorderlessIcon(t, icon)
}

func TestWindowsIconUsesTheSameBorderlessMark(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("build", "windows", "icon.ico"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 22 || binary.LittleEndian.Uint16(raw[0:2]) != 0 || binary.LittleEndian.Uint16(raw[2:4]) != 1 || binary.LittleEndian.Uint16(raw[4:6]) != 1 {
		t.Fatal("Windows icon must contain one ICO image")
	}
	size := int(binary.LittleEndian.Uint32(raw[14:18]))
	offset := int(binary.LittleEndian.Uint32(raw[18:22]))
	if size <= 0 || offset < 22 || offset > len(raw)-size {
		t.Fatalf("invalid ICO payload: offset=%d size=%d file=%d", offset, size, len(raw))
	}
	icon, _, err := image.Decode(bytes.NewReader(raw[offset : offset+size]))
	if err != nil {
		t.Fatalf("decode embedded Windows icon: %v", err)
	}
	assertBorderlessIcon(t, icon)
}

func assertBorderlessIcon(t *testing.T, icon image.Image) {
	t.Helper()
	bounds := icon.Bounds()
	if bounds.Dx() != bounds.Dy() || bounds.Dx() < 256 {
		t.Fatalf("icon must be a square of at least 256 pixels: %v", bounds)
	}
	margin := bounds.Dx() / 16
	colored := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := color.NRGBAModel.Convert(icon.At(x, y)).(color.NRGBA)
			if pixel.A == 0 {
				continue
			}
			if x < bounds.Min.X+margin || x >= bounds.Max.X-margin || y < bounds.Min.Y+margin || y >= bounds.Max.Y-margin {
				t.Fatalf("icon perimeter must be transparent, found pixel at (%d,%d)", x, y)
			}
			if pixel.A > 200 && pixel.R == pixel.G && pixel.G == pixel.B {
				t.Fatalf("icon contains an opaque neutral backplate or rim at (%d,%d): %v", x, y, pixel)
			}
			colored++
		}
	}
	if colored < bounds.Dx()*bounds.Dy()/10 {
		t.Fatal("icon mark is missing or too small")
	}
}
