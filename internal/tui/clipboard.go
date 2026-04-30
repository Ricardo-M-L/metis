package tui

// clipboard.go shells out to OS-specific tools to read clipboard
// content (image bytes preferred, text fallback). Pattern lifted
// from opencode's clipboard.ts — terminals don't surface image
// paste through bracketed-paste, so we have to hit the system
// clipboard directly when Ctrl+V is pressed.
//
// Per-OS strategy:
//   - macOS:    osascript reads PNG via "the clipboard as «class PNGf»"
//   - Linux:    wl-paste (Wayland) → xclip (X11) → xsel
//   - Windows:  PowerShell System.Windows.Forms.Clipboard.GetImage
//   - Fallback: pbpaste / xclip / clip.exe for plain text
//
// We don't take a heavy dep like golang.design/x/clipboard because
// (a) it requires CGO on Linux, (b) it doesn't read image data on
// macOS reliably. Shell-out is portable and matches opencode's
// proven pattern.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ClipboardContent is what readClipboard returns. Mime tells the
// caller whether to attach as image (image/png, image/jpeg) or to
// inject as plain text (text/plain).
type ClipboardContent struct {
	Data []byte
	Mime string
}

// readClipboard probes the OS clipboard for an image first, then
// falls back to text. 1s exec timeout per probe. Returns nil
// (Content, nil err) when the clipboard is empty or unreadable —
// caller should treat that as "nothing happened" silently.
func readClipboard(ctx context.Context) (*ClipboardContent, error) {
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		if img := readDarwinClipboardImage(c); img != nil {
			return img, nil
		}
		if text := runClip(c, "pbpaste"); text != nil {
			return &ClipboardContent{Data: text, Mime: "text/plain"}, nil
		}
	case "linux":
		// Wayland first (newer distros default to it).
		if img := runClipBytes(c, "wl-paste", "-t", "image/png"); len(img) > 0 {
			return &ClipboardContent{Data: img, Mime: "image/png"}, nil
		}
		if img := runClipBytes(c, "xclip", "-selection", "clipboard", "-t", "image/png", "-o"); len(img) > 0 {
			return &ClipboardContent{Data: img, Mime: "image/png"}, nil
		}
		if text := runClip(c, "wl-paste"); text != nil {
			return &ClipboardContent{Data: text, Mime: "text/plain"}, nil
		}
		if text := runClip(c, "xclip", "-selection", "clipboard", "-o"); text != nil {
			return &ClipboardContent{Data: text, Mime: "text/plain"}, nil
		}
		if text := runClip(c, "xsel", "--clipboard", "--output"); text != nil {
			return &ClipboardContent{Data: text, Mime: "text/plain"}, nil
		}
	case "windows":
		// PowerShell embedded script: try image first, then text.
		script := `Add-Type -AssemblyName System.Windows.Forms; ` +
			`$img = [System.Windows.Forms.Clipboard]::GetImage(); ` +
			`if ($img) { ` +
			`  $ms = New-Object System.IO.MemoryStream; ` +
			`  $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); ` +
			`  Write-Output ("IMG:" + [System.Convert]::ToBase64String($ms.ToArray())) ` +
			`} else { Write-Output ("TXT:" + [System.Windows.Forms.Clipboard]::GetText()) }`
		out := runClip(c, "powershell.exe", "-NonInteractive", "-NoProfile", "-Command", script)
		if len(out) > 4 {
			tag := string(out[:4])
			body := out[4:]
			if tag == "IMG:" {
				// PowerShell base64 decode happens client-side in opencode;
				// we keep the raw base64 here and the caller writes it.
				return &ClipboardContent{Data: body, Mime: "image/png-base64"}, nil
			}
			if tag == "TXT:" {
				return &ClipboardContent{Data: body, Mime: "text/plain"}, nil
			}
		}
	}
	return nil, nil
}

// readDarwinClipboardImage uses osascript to extract a PNG from the
// macOS clipboard. AppleScript is the only first-party way to get
// image bytes — pbpaste doesn't handle images. Returns nil when
// the clipboard doesn't hold an image (osascript exits non-zero).
func readDarwinClipboardImage(ctx context.Context) *ClipboardContent {
	tmpfile := filepath.Join(os.TempDir(), fmt.Sprintf("metis-clip-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmpfile)
	script := fmt.Sprintf(`set imageData to the clipboard as «class PNGf»
set fileRef to open for access POSIX file %q with write permission
set eof fileRef to 0
write imageData to fileRef
close access fileRef`, tmpfile)
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return nil
	}
	data, err := os.ReadFile(tmpfile)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &ClipboardContent{Data: data, Mime: "image/png"}
}

func runClip(ctx context.Context, name string, args ...string) []byte {
	out := runClipBytes(ctx, name, args...)
	if len(out) == 0 {
		return nil
	}
	return out
}

func runClipBytes(ctx context.Context, name string, args ...string) []byte {
	if _, err := exec.LookPath(name); err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// saveClipboardImage persists a clipboard-pasted image to
// ~/.metis/cache/clipboard-<ts>.png and returns the absolute path.
// The path is what we inject into the input as [image: ...] so
// the model can pick it up by reading the file (multimodal models
// already understand "read this file as an image").
func saveClipboardImage(data []byte, mime string) (string, error) {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".metis")
	}
	dir := filepath.Join(home, "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Windows PowerShell hands us base64-encoded PNG bytes; decode
	// before writing so the file is a real image not a base64
	// string. Non-base64 mimes pass through unchanged.
	raw := data
	if mime == "image/png-base64" {
		decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(data)))
		if err != nil {
			return "", err
		}
		raw = decoded
		mime = "image/png"
	}
	ext := ".png"
	if mime == "image/jpeg" {
		ext = ".jpg"
	}
	path := filepath.Join(dir, fmt.Sprintf("clipboard-%d%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
