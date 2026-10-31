//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Compile the production Objective-C implementation into a disposable native
// app. The probe reads NSApp.applicationIconImage, rather than the unrelated
// NSRunningApplication.icon which keeps returning the static bundle icon.
func TestNativeDockIconAppearanceAndTransparency(t *testing.T) {
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang unavailable")
	}
	root := t.TempDir()
	bundle := filepath.Join(root, "MetisDockProbe.app", "Contents")
	if err := os.MkdirAll(filepath.Join(bundle, "MacOS"), 0700); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.metis.tests.dock</string><key>CFBundleExecutable</key><string>probe</string><key>CFBundlePackageType</key><string>APPL</string><key>LSUIElement</key><true/></dict></plist>`
	if err := os.WriteFile(filepath.Join(bundle, "Info.plist"), []byte(info), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "probe.m")
	if err := os.WriteFile(source, []byte(nativeDockProbeSource), 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(bundle, "MacOS", "probe")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	compile := exec.CommandContext(ctx, clang, "-I", cwd, "-framework", "Cocoa", "-fblocks", source,
		filepath.Join(cwd, "native_dock_icon_darwin.m"), "-o", binary)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile production Dock implementation: %v\n%s", err, out)
	}
	run := exec.CommandContext(ctx, binary, filepath.Join(cwd, "build", "appicon.png"), root)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("native Dock probe: %v\n%s", err, out)
	}
	var report struct {
		LightCached bool `json:"light_cached"`
		DarkCached  bool `json:"dark_cached"`
		AutoInherit bool `json:"auto_inherit"`
		Repeated    bool `json:"repeated_install_stable"`
	}
	data, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.LightCached || !report.DarkCached || !report.AutoInherit || !report.Repeated {
		t.Fatalf("native appearance/cache behavior: %s", data)
	}
	lightFile, err := os.Open(filepath.Join(root, "light.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer lightFile.Close()
	light, err := png.Decode(lightFile)
	if err != nil {
		t.Fatal(err)
	}
	darkFile, err := os.Open(filepath.Join(root, "dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer darkFile.Close()
	dark, err := png.Decode(darkFile)
	if err != nil {
		t.Fatal(err)
	}
	assertDockIconTile(t, light, false)
	assertDockIconTile(t, dark, true)
	if light.Bounds() != dark.Bounds() {
		t.Fatal("appearance change resized the Dock icon")
	}
	changed := 0
	for y := 0; y < light.Bounds().Dy(); y++ {
		for x := 0; x < light.Bounds().Dx(); x++ {
			l := color.NRGBAModel.Convert(light.At(x, y)).(color.NRGBA)
			d := color.NRGBAModel.Convert(dark.At(x, y)).(color.NRGBA)
			if l.A != d.A {
				t.Fatalf("appearance tint changed alpha at %d,%d: %d -> %d", x, y, l.A, d.A)
			}
			if l.A > 200 && int(l.B) > int(l.R)+40 && int(l.B) > int(l.G)+30 && (d.R > l.R || d.G > l.G || d.B > l.B) {
				changed++
			}
		}
	}
	if changed < light.Bounds().Dx()*light.Bounds().Dy()/16 {
		t.Fatalf("dark appearance did not brighten the existing mark: %d pixels", changed)
	}
}

// The runtime icon deliberately has a flat, rounded tile. The source artwork
// tests still require a transparent mark, but that is not the Dock presentation.
func assertDockIconTile(t *testing.T, icon image.Image, dark bool) {
	t.Helper()
	b := icon.Bounds()
	if b.Dx() != b.Dy() || b.Dx() < 256 {
		t.Fatalf("unexpected tile bounds: %v", b)
	}
	at := func(x, y int) color.NRGBA {
		return color.NRGBAModel.Convert(icon.At(x*b.Dx()/1024, y*b.Dy()/1024)).(color.NRGBA)
	}
	for _, p := range [][2]int{{0, 0}, {512, 40}, {40, 512}, {110, 110}, {913, 110}, {110, 913}, {913, 913}} {
		if v := at(p[0], p[1]); v.A != 0 {
			t.Fatalf("tile corner or perimeter must be transparent at %v: %v", p, v)
		}
	}
	expected := color.NRGBA{R: 243, G: 244, B: 247, A: 255}
	if dark {
		expected = color.NRGBA{R: 37, G: 39, B: 45, A: 255}
	}
	// Every side must wrap the mark, with one flat color rather than an inset rim.
	for _, p := range [][2]int{{512, 104}, {512, 920}, {104, 512}, {920, 512}, {512, 112}, {112, 512}, {512, 900}, {900, 512}} {
		v := at(p[0], p[1])
		delta := func(a, b uint8) int {
			if a > b {
				return int(a) - int(b)
			}
			return int(b) - int(a)
		}
		if v.A != 255 || delta(v.R, expected.R) > 2 || delta(v.G, expected.G) > 2 || delta(v.B, expected.B) > 2 {
			t.Fatalf("flat %v tile missing or rim found at %v: got %v want %v", dark, p, v, expected)
		}
	}
	blue := 0
	for y := 180; y < 845; y++ {
		for x := 180; x < 845; x++ {
			v := at(x, y)
			if v.A == 255 && int(v.B) > int(v.R)+40 && int(v.B) > int(v.G)+30 {
				blue++
			}
		}
	}
	if blue < 70000 {
		t.Fatalf("blue/purple mark missing inside the tile: %d pixels", blue)
	}
}

const nativeDockProbeSource = `
#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#include "native_dock_icon_darwin.h"
static void saveIcon(NSString *path) {
  NSBitmapImageRep *bitmap = [NSBitmapImageRep imageRepWithData:[NSApp.applicationIconImage TIFFRepresentation]];
  [[bitmap representationUsingType:NSBitmapImageFileTypePNG properties:@{}] writeToFile:path atomically:YES];
}
int main(int argc, char **argv) {
 @autoreleasepool {
  [NSApplication sharedApplication];
  [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
  NSString *output = [NSString stringWithUTF8String:argv[2]];
  NSData *data = [NSData dataWithContentsOfFile:[NSString stringWithUTF8String:argv[1]]];
  [NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameAqua]];
  metis_install_dock_icon(data.bytes, data.length);
  dispatch_async(dispatch_get_main_queue(), ^{
    NSData *light = [[NSApp.applicationIconImage TIFFRepresentation] copy];
    saveIcon([output stringByAppendingPathComponent:@"light.png"]);
    [NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameDarkAqua]];
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 150 * NSEC_PER_MSEC), dispatch_get_main_queue(), ^{
    NSString *darkName = NSApp.effectiveAppearance.name;
    NSData *dark = [[NSApp.applicationIconImage TIFFRepresentation] copy];
    saveIcon([output stringByAppendingPathComponent:@"dark.png"]);
    BOOL darkCached = ![dark isEqualToData:light];
    BOOL lightCached = YES;
    for (int i = 0; i < 12; i++) {
      [NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameAqua]];
      metis_refresh_dock_icon();
      lightCached = lightCached && [[NSApp.applicationIconImage TIFFRepresentation] isEqualToData:light];
      [NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameDarkAqua]];
      metis_refresh_dock_icon();
      darkCached = darkCached && [[NSApp.applicationIconImage TIFFRepresentation] isEqualToData:dark];
    }
    [NSApp setAppearance:nil];
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 150 * NSEC_PER_MSEC), dispatch_get_main_queue(), ^{
    BOOL autoInherit = NSApp.appearance == nil;
    NSString *match = [NSApp.effectiveAppearance bestMatchFromAppearancesWithNames:@[NSAppearanceNameAqua,NSAppearanceNameDarkAqua]];
    autoInherit = autoInherit && [[NSApp.applicationIconImage TIFFRepresentation] isEqualToData:([match isEqualToString:NSAppearanceNameDarkAqua] ? dark : light)];
    NSData *automatic = [[NSApp.applicationIconImage TIFFRepresentation] copy];
    metis_install_dock_icon(data.bytes, data.length);
    dispatch_async(dispatch_get_main_queue(), ^{
      NSDictionary *result = @{@"dark_name":darkName, @"light_cached":@(lightCached), @"dark_cached":@(darkCached), @"auto_inherit":@(autoInherit), @"repeated_install_stable":@((BOOL)[automatic isEqualToData:[NSApp.applicationIconImage TIFFRepresentation]])};
      [[NSJSONSerialization dataWithJSONObject:result options:0 error:nil] writeToFile:[output stringByAppendingPathComponent:@"result.json"] atomically:YES];
      exit(0);
    });
    });
    });
  });
  [NSApp run];
 }
 return 1;
}
`
