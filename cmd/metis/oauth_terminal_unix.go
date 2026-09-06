//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// automaticOAuthPasteCode returns a cancellable hidden terminal reader for
// the browser/callback race. A normal term.ReadPassword call cannot be
// interrupted when the browser callback wins and may leave terminal echo
// disabled while the process exits.
func automaticOAuthPasteCode() func(context.Context, string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return func(ctx context.Context, _ string) (string, error) {
		return readHiddenOAuthCodeContext(ctx, "Paste authorization code or redirect URL (input hidden; browser callback also works): ")
	}
}

func readHiddenOAuthCodeContext(ctx context.Context, prompt string) (result string, err error) {
	value, err := readHiddenTerminalInputContext(ctx, prompt, 512<<10)
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("OAuth authorization code is empty")
	}
	return value, nil
}

func readHiddenTerminalInputContext(ctx context.Context, prompt string, maxInputBytes int) (result string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("hidden input requires an interactive stdin terminal")
	}
	previous, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("prepare hidden terminal input: %w", err)
	}
	fmt.Fprint(os.Stderr, prompt)
	defer func() {
		restoreErr := term.Restore(fd, previous)
		fmt.Fprintln(os.Stderr)
		if err == nil && restoreErr != nil {
			err = fmt.Errorf("restore terminal after hidden input: %w", restoreErr)
		}
	}()

	buf := make([]byte, 0, 512)
	defer func() {
		for i := range buf {
			buf[i] = 0
		}
	}()
	chunk := make([]byte, 256)
	defer clear(chunk)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, pollErr := unix.Poll(pollFDs, 100)
		if pollErr != nil {
			if errors.Is(pollErr, unix.EINTR) {
				continue
			}
			return "", fmt.Errorf("wait for hidden terminal input: %w", pollErr)
		}
		if pollFDs[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return "", errors.New("hidden input terminal closed")
		}
		if pollFDs[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, readErr := unix.Read(fd, chunk)
		if readErr != nil {
			if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
				continue
			}
			return "", fmt.Errorf("read hidden terminal input: %w", readErr)
		}
		for _, b := range chunk[:n] {
			switch b {
			case '\r', '\n':
				if err := ctx.Err(); err != nil {
					return "", err
				}
				return string(buf), nil
			case 0x03:
				return "", context.Canceled
			case 0x7f, 0x08:
				if len(buf) > 0 {
					_, size := utf8.DecodeLastRune(buf)
					buf = buf[:len(buf)-size]
				}
			default:
				if len(buf) >= maxInputBytes {
					return "", errors.New("hidden input is too large")
				}
				buf = append(buf, b)
			}
		}
	}
}
