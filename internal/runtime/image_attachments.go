package runtime

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// BindImageAttachmentStore is the embedded-server assembly equivalent of the
// AgentLoopOptions binding. It uses the supplied store, not a trace singleton
// or an inferred directory. Call only before the loop starts running.
func BindImageAttachmentStore(loop *agent.Loop, store *session.Store) {
	if loop == nil {
		return
	}
	if store == nil {
		loop.ImageAttachments = nil
		return
	}
	loop.ImageAttachments = store
}
