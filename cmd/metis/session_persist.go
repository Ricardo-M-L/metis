package main

import (
	"fmt"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

var writeFreshSessionHeader = rtpkg.WriteFreshHeaderWithPromptKind

func persistFreshSessionHeader(store *session.Store, sessionID, provider, model, system, promptKind, mode string) error {
	if err := writeFreshSessionHeader(store, sessionID, provider, model, system, promptKind, mode); err != nil {
		return fmt.Errorf("persist fresh session header for %s: %w", sessionID, err)
	}
	return nil
}

func persistSessionTitle(store *session.Store, sessionID, title string) error {
	if err := store.SetTitle(sessionID, title); err != nil {
		return fmt.Errorf("persist title for session %s: %w", sessionID, err)
	}
	return nil
}
