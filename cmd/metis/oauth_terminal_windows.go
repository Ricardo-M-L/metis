//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// Windows keeps the browser callback path only. If binding fails, the command
// falls back to the ordinary explicit hidden reader via
// OAuthOptions.FallbackPasteCodeContext.
func automaticOAuthPasteCode() func(context.Context, string) (string, error) {
	return nil
}

func readHiddenOAuthCodeContext(ctx context.Context, prompt string) (string, error) {
	value, err := readHiddenTerminalInputContext(ctx, prompt, 512<<10)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// Read console events directly: ReadPassword/ReadConsole can block after a
// signal cancels ctx. Waiting on console events lets us check cancellation and
// restore the input mode without leaving a blocked background reader behind.
func readHiddenTerminalInputContext(ctx context.Context, prompt string, maxInputBytes int) (result string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(os.Stdin.Fd())
	previous, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("hidden input requires an interactive stdin terminal: %w", err)
	}
	input := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(input, &mode); err != nil {
		return "", err
	}
	mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(input, mode); err != nil {
		return "", fmt.Errorf("prepare hidden terminal input: %w", err)
	}
	defer func() {
		restoreErr := term.Restore(fd, previous)
		fmt.Fprintln(os.Stderr)
		if err == nil && restoreErr != nil {
			err = fmt.Errorf("restore terminal after hidden input: %w", restoreErr)
		}
	}()
	fmt.Fprint(os.Stderr, prompt)
	readConsoleInput := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")
	buf := make([]uint16, 0, 512)
	defer func() { clear(buf[:cap(buf)]) }()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := windows.WaitForSingleObject(input, 100)
		if err != nil {
			return "", fmt.Errorf("wait for hidden terminal input: %w", err)
		}
		if ready == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		// INPUT_RECORD holds a 16-byte event union, with KEY_EVENT_RECORD
		// laid out here. Other console events are consumed and ignored.
		var event struct {
			Type, Padding uint16
			KeyDown       int32
			Repeat        uint16
			VirtualKey    uint16
			ScanCode      uint16
			Char          uint16
			ControlState  uint32
		}
		var count uint32
		ok, _, readErr := readConsoleInput.Call(uintptr(input), uintptr(unsafe.Pointer(&event)), 1, uintptr(unsafe.Pointer(&count)))
		if ok == 0 {
			return "", fmt.Errorf("read hidden terminal input: %w", readErr)
		}
		if count == 0 || event.Type != 1 || event.KeyDown == 0 || event.Char == 0 {
			continue
		}
		for range event.Repeat {
			switch event.Char {
			case '\r', '\n':
				if err := ctx.Err(); err != nil {
					return "", err
				}
				value := string(utf16.Decode(buf))
				if len(value) > maxInputBytes {
					return "", errors.New("hidden input is too large")
				}
				return value, nil
			case 0x03:
				return "", context.Canceled
			case 0x7f, 0x08:
				if len(buf) > 0 {
					buf[len(buf)-1] = 0
					buf = buf[:len(buf)-1]
				}
			default:
				if len(buf) >= maxInputBytes {
					return "", errors.New("hidden input is too large")
				}
				buf = append(buf, event.Char)
			}
		}
	}
}
