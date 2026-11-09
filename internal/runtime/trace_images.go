package runtime

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// SetTraceAttachmentStore binds the configured session store explicitly. The
// trace directory need not share a parent with the session directory.
func SetTraceAttachmentStore(store *session.Store) {
	if a := CurrentTraceAdapter(); a != nil {
		a.SetAttachmentStore(store)
	}
}

func (a *TraceAdapter) SetAttachmentStore(store *session.Store) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.attachmentStore = store
	a.mu.Unlock()
}

func collectTraceImages(sessionID string, blocks []llm.ContentBlock) []session.TraceImage {
	a := CurrentTraceAdapter()
	if a == nil {
		return session.TraceImagesFromBlocks(blocks)
	}
	a.mu.Lock()
	store := a.attachmentStore
	a.mu.Unlock()
	if store == nil {
		return session.TraceImagesFromBlocks(blocks)
	}
	images, _ := store.PersistTraceImages(sessionID, blocks)
	return images
}
