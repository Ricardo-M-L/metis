//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include "native_dock_icon_darwin.h"
*/
import "C"

import (
	_ "embed"
	"unsafe"
)

// Embed the unprocessed, transparent artwork. Loading the bundle icon through
// NSWorkspace would load macOS' compatibility backplate again.
//
//go:embed build/appicon.png
var nativeDockIconPNG []byte

func installNativeDockIcon() {
	if len(nativeDockIconPNG) == 0 {
		return
	}
	C.metis_install_dock_icon(unsafe.Pointer(&nativeDockIconPNG[0]), C.size_t(len(nativeDockIconPNG)))
}
