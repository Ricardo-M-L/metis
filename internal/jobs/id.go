package jobs

// id.go — short stable IDs for background jobs.
//
// Format: bg_<8 lower-hex>. The model writes JobOutput / JobStop with
// these IDs — we want them short enough to type once but unique
// enough across a few hundred jobs in a session. 8 hex chars =
// 4 billion, which is plenty.
//
// We use crypto/rand instead of time-based or counter IDs to keep the
// space flat (no clustering near process start) and to survive metis
// restarts without reuse anxiety. The cost is ~10 ns per call, fine
// since job spawns are rare events.

import (
	"crypto/rand"
	"encoding/hex"
)

// newJobID returns a fresh job ID. Format: `bg_<8 hex>`.
func newJobID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on supported platforms;
		// fall through to a constant rather than panic so a
		// pathological env (e.g. /dev/urandom missing inside a
		// container) doesn't kill the agent.
		return "bg_00000000"
	}
	return "bg_" + hex.EncodeToString(b[:])
}
