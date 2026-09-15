//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework Cocoa
#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#include <stdlib.h>
#include <string.h>

static void metis_set_native_theme(const char *theme) {
	char *theme_copy = strdup(theme == NULL ? "" : theme);
	dispatch_async(dispatch_get_main_queue(), ^{
		NSString *themeName = [NSString stringWithUTF8String:theme_copy];
		NSAppearance *appearance = nil;
		if ([themeName isEqualToString:@"light"]) {
			appearance = [NSAppearance appearanceNamed:NSAppearanceNameAqua];
		} else if ([themeName isEqualToString:@"dark"]) {
			if (@available(macOS 10.14, *)) {
				appearance = [NSAppearance appearanceNamed:NSAppearanceNameDarkAqua];
			}
		}
		for (NSWindow *window in [NSApp windows]) {
			[window setAppearance:appearance];
		}
		free(theme_copy);
	});
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func applyNativeTheme(theme string) error {
	if theme != "auto" && theme != "light" && theme != "dark" {
		return fmt.Errorf("unsupported native theme %q", theme)
	}
	value := C.CString(theme)
	defer C.free(unsafe.Pointer(value))
	C.metis_set_native_theme(value)
	return nil
}
