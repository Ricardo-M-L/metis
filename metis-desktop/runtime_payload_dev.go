//go:build !production

package main

import "io/fs"

type noDesktopRuntimePayload struct{}

func (noDesktopRuntimePayload) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

var desktopRuntimePayloadFS fs.FS = noDesktopRuntimePayload{}

const desktopRuntimeProduction = false
