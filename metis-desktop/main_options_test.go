package main

import "testing"

func TestDesktopWindowKeepsResizeAndMacZoomAvailable(t *testing.T) {
	opts := desktopOptions(NewApp())
	if opts.DisableResize {
		t.Fatal("desktop window must remain resizable")
	}
	if opts.Frameless {
		t.Fatal("desktop must retain native window controls on every platform")
	}
	if opts.Mac == nil {
		t.Fatal("desktop window must set explicit macOS options so Wails does not disable the zoom button")
	}
	if opts.Mac.DisableZoom {
		t.Fatal("desktop window must keep the macOS zoom/maximize button enabled")
	}
	titlebar := opts.Mac.TitleBar
	if titlebar == nil || titlebar.HideTitleBar || !titlebar.HideTitle ||
		!titlebar.TitlebarAppearsTransparent || !titlebar.FullSizeContent ||
		!titlebar.UseToolbar || !titlebar.HideToolbarSeparator {
		t.Fatal("macOS must use inset native traffic lights over full-size content without an opaque titlebar")
	}
}
