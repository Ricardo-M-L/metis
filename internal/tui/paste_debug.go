package tui

// paste_debug.go — opt-in diagnostic logger for the cmd+V paste path.
// User report 2026-05-07: "输入框用 command+v 粘贴不进去". Several
// failure modes are possible:
//
//	1. Terminal doesn't honour bracketed-paste mode (Apple Terminal
//	   sometimes lags on the `\x1b[?2004h` SET request) — bubbletea
//	   never produces a PasteMsg, content arrives as KeyPressMsg run.
//	2. PasteMsg arrives but a guard (permActive / copyMode /
//	   activeScreen) silently drops it.
//	3. PasteMsg arrives empty (terminal sent the wrapper but no body).
//
// Setting `METIS_PASTE_DEBUG=1` writes one line per PasteMsg attempt
// to ~/.metis/paste-debug.log so the failure mode is visible. Off by
// default — the call site is a single function call with a constant
// fast-path return when the env var is unset, so cost is negligible.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

var (
	pasteDebugOnce    sync.Once
	pasteDebugEnabled bool
	pasteDebugPath    string
	pasteDebugMu      sync.Mutex
)

func pasteDebug(format string, args ...any) {
	pasteDebugOnce.Do(func() {
		pasteDebugEnabled = os.Getenv("METIS_PASTE_DEBUG") == "1"
		if home := config.Home(); home != "" {
			pasteDebugPath = filepath.Join(home, "paste-debug.log")
		}
	})
	if !pasteDebugEnabled || pasteDebugPath == "" {
		return
	}
	pasteDebugMu.Lock()
	defer pasteDebugMu.Unlock()
	f, err := os.OpenFile(pasteDebugPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	stamp := time.Now().Format("15:04:05.000")
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{stamp}, args...)...)
}
