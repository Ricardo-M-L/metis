// img2braille — one-shot CLI that converts a PNG/JPEG into a block
// of Braille characters (U+2800–U+28FF). Each Braille glyph carries
// 2×4 = 8 dot positions, so an N×M Braille grid encodes a virtual
// 2N×4M monochrome bitmap — about 8× the density of standard ANSI
// block art (▟▙█).
//
// This is a developer tool (NOT shipped in the metis binary). Run it
// once to convert an owl illustration to a string literal, paste the
// output into internal/tui/render_welcome.go, and commit.
//
// Usage:
//
//	img2braille -in owl.png -w 30 -h 18 -threshold 128 > banner.txt
//	img2braille -in owl.png -w 30 -h 18 -crop 200,100,1200,1000
//
// Flags:
//
//	-in        input image (.png / .jpg / .jpeg)
//	-w         output width in Braille chars  (default 30)
//	-h         output height in Braille rows  (default 18)
//	-threshold luminance threshold 0..255, pixels brighter become dots
//	           (default 128 — invert with -invert for dark-on-light art)
//	-invert    flip threshold sense (dark pixels become dots)
//	-crop      x,y,w,h source rectangle in pixels (default = whole image)
//	-quoted    wrap each output row as a Go string literal "..."
//
// Algorithm:
//
//  1. Optional crop to a sub-rectangle.
//
//  2. Nearest-neighbor downscale to (2W × 4H) pixels — that's exactly
//     the virtual resolution Braille can represent.
//
//  3. For each 2×4 cell, compute the 8-bit dotmask:
//
//     bit positions  bit#  hex
//     ┌───┬───┐
//     │ 1 │ 4 │       1=0x01  4=0x08
//     ├───┼───┤
//     │ 2 │ 5 │       2=0x02  5=0x10
//     ├───┼───┤
//     │ 3 │ 6 │       3=0x04  6=0x20
//     ├───┼───┤
//     │ 7 │ 8 │       7=0x40  8=0x80
//     └───┴───┘
//
//     Output char = rune(0x2800 + dotmask).
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
)

// brailleDotBits maps a (col, row) index inside the 2×4 cell to the
// bit position in the Braille dotmask. Same layout as the doc comment
// above: col 0 = left column (bits 1,2,3,7), col 1 = right column
// (bits 4,5,6,8).
var brailleDotBits = [2][4]byte{
	{0x01, 0x02, 0x04, 0x40}, // col 0
	{0x08, 0x10, 0x20, 0x80}, // col 1
}

func main() {
	in := flag.String("in", "", "input image path (.png / .jpg)")
	w := flag.Int("w", 30, "output width in Braille chars (1 char = 2 px)")
	h := flag.Int("h", 18, "output height in Braille rows (1 row = 4 px)")
	thr := flag.Int("threshold", 128, "luminance threshold 0..255")
	invert := flag.Bool("invert", false, "invert threshold sense")
	cropFlag := flag.String("crop", "", "x,y,w,h source rect in pixels (default = full image)")
	quoted := flag.Bool("quoted", false, "wrap each output row as a Go string literal")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "img2braille: -in is required")
		os.Exit(2)
	}

	f, err := os.Open(*in)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		fail(err)
	}

	if *cropFlag != "" {
		src, err = cropImage(src, *cropFlag)
		if err != nil {
			fail(err)
		}
	}

	// Downscale to exactly (2w × 4h) pixels. CatmullRom gives smoother
	// edges than NearestNeighbor on figurative art; nearest is harsher
	// but preserves hard silhouettes better. We pick CatmullRom for
	// general-purpose, then the threshold step binarizes anyway.
	dstW, dstH := *w*2, *h*4
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	// Per-cell dotmask: walk 2×4 blocks, accumulate bits.
	for row := 0; row < *h; row++ {
		var line strings.Builder
		for col := 0; col < *w; col++ {
			var mask byte
			for dx := 0; dx < 2; dx++ {
				for dy := 0; dy < 4; dy++ {
					px := col*2 + dx
					py := row*4 + dy
					lum := luminance(dst.At(px, py))
					lit := lum >= byte(*thr)
					if *invert {
						lit = !lit
					}
					if lit {
						mask |= brailleDotBits[dx][dy]
					}
				}
			}
			line.WriteRune(rune(0x2800 + int(mask)))
		}
		if *quoted {
			fmt.Printf("\t%q,\n", line.String())
		} else {
			fmt.Println(line.String())
		}
	}
}

func luminance(c color.Color) byte {
	// Standard Rec. 601 grayscale: Y = 0.299R + 0.587G + 0.114B.
	// Divide by 65535 (16-bit RGBA channel range) to get 0..255.
	r, g, b, _ := c.RGBA()
	y := (299*uint32(r>>8) + 587*uint32(g>>8) + 114*uint32(b>>8)) / 1000
	if y > 255 {
		y = 255
	}
	return byte(y)
}

func cropImage(src image.Image, spec string) (image.Image, error) {
	parts := strings.Split(spec, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("crop: want x,y,w,h, got %q", spec)
	}
	vals := make([]int, 4)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("crop: parse %s: %w", p, err)
		}
		vals[i] = v
	}
	rect := image.Rect(vals[0], vals[1], vals[0]+vals[2], vals[1]+vals[3])
	// Intersect with source bounds — a too-large rect silently clamps
	// rather than panicking.
	rect = rect.Intersect(src.Bounds())
	if rect.Empty() {
		return nil, fmt.Errorf("crop: rect outside image bounds")
	}
	// image.Image lacks a portable SubImage method on every concrete
	// type; copy into a fresh RGBA so callers can rely on .At().
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	return dst, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "img2braille:", err)
	os.Exit(1)
}
