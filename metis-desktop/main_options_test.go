package main

import "testing"

func TestDesktopWindowKeepsResizeAndMacZoomAvailable(t *testing.T) {
	opts := desktopOptions(NewApp())
	if opts.DisableResize {
		t.Fatal("desktop window must remain resizable")
	}
	if opts.Mac == nil {
		t.Fatal("desktop window must set explicit macOS options so Wails does not disable the zoom button")
	}
	if opts.Mac.DisableZoom {
		t.Fatal("desktop window must keep the macOS zoom/maximize button enabled")
	}
}
