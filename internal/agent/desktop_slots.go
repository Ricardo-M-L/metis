package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// These variables are set only by Desktop's isolated-turn launcher. They are
// intentionally inert for normal CLI processes, which retain their configured
// per-process roster behavior.
const (
	desktopSubagentSlotDirEnv = "METIS_DESKTOP_SUBAGENT_SLOT_DIR"
	desktopSubagentSlotsEnv   = "METIS_DESKTOP_SUBAGENT_SLOTS"
)

// AcquireDesktopSubagentSlot takes one cross-process child-agent permit. The
// Desktop root scheduler already owns up to six root permits; it provisions
// six of these child permits, giving the high-performance profile a hard
// twelve-agent ceiling across all worker processes.
//
// When Desktop has not supplied the environment the function is a no-op. A
// caller always receives a non-nil, idempotent release function on success.
func AcquireDesktopSubagentSlot(ctx context.Context) (func(), error) {
	dir := strings.TrimSpace(os.Getenv(desktopSubagentSlotDirEnv))
	if dir == "" {
		return func() {}, nil
	}
	slots, err := strconv.Atoi(strings.TrimSpace(os.Getenv(desktopSubagentSlotsEnv)))
	if err != nil || slots < 1 || slots > 64 {
		return nil, fmt.Errorf("desktop sub-agent slots: invalid capacity")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("desktop sub-agent slots: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("desktop sub-agent slots: root is not a directory")
	}
	for {
		for i := 0; i < slots; i++ {
			path := filepath.Join(dir, fmt.Sprintf("slot-%02d.lock", i))
			release, acquired, err := tryAcquireDesktopSlot(path)
			if err != nil {
				return nil, err
			}
			if acquired {
				return release, nil
			}
		}
		timer := time.NewTimer(75 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
