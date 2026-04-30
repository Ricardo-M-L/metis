// Package imessage implements channels.Adapter for macOS iMessage
// via AppleScript (the only supported public path — Apple has no
// public Messages API). Works only on macOS; other OSes report
// Configured() = false.
//
// Approach: shell out to `osascript` to drive Messages.app to send
// to a recipient. Latency is ~500ms-1s per message because Messages
// has to be foreground-able; metis is already shelling out for
// clipboard so adding one more isn't a new pattern.
package imessage

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

type Adapter struct {
	// No config — uses the user's signed-in Messages account.
	// Adapter exists if you're on macOS.
}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string     { return "imessage" }
func (a *Adapter) Configured() bool { return runtime.GOOS == "darwin" }

// Send drives Messages.app via AppleScript. target is the recipient
// phone number or email (anything Messages.app would accept in the
// "To:" field). Note: AppleScript escaping is fragile — we sanitize
// the message to drop double-quotes and backslashes since those are
// the only chars that break the script.
func (a *Adapter) Send(ctx context.Context, target string, msg channels.Message) error {
	if !a.Configured() {
		return fmt.Errorf("imessage: only supported on macOS")
	}
	text := msg.Text
	if msg.Title != "" {
		text = msg.Title + "\n\n" + msg.Text
	}
	// AppleScript-safe escape — replace double quotes with single
	// quotes (loses minimal information but avoids script breakage).
	text = strings.ReplaceAll(text, `"`, `'`)
	text = strings.ReplaceAll(text, `\`, `/`)

	script := fmt.Sprintf(`tell application "Messages"
		set targetService to 1st service whose service type = iMessage
		set targetBuddy to buddy %q of targetService
		send %q to targetBuddy
	end tell`, target, text)
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("imessage: osascript failed: %w", err)
	}
	return nil
}
