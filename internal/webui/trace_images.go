package webui

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func traceImagesForView(s *Server, sid string, images []session.TraceImage) []session.TraceImage {
	if len(images) == 0 {
		return nil
	}
	out := append([]session.TraceImage(nil), images...)
	for i := range out {
		out[i].URL = ""
		if s == nil || s.store == nil {
			out[i].Available = false
			out[i].Limitation = "attachment storage is unavailable"
			continue
		}
		if out[i].AttachmentHash == "" {
			out[i].Available = false
			if out[i].Limitation == "" {
				out[i].Limitation = "image attachment reference is unavailable"
			}
			continue
		}
		if out[i].Available && out[i].Width > 0 && out[i].Height > 0 && out[i].Bytes > 0 && s.store.TraceImageExists(sid, out[i].AttachmentHash, out[i].Bytes) {
			out[i].URL = "/api/trace/image?session_id=" + url.QueryEscape(sid) + "&hash=" + url.QueryEscape(out[i].AttachmentHash)
			continue
		}
		data, err := s.store.ReadTraceImage(sid, out[i].AttachmentHash)
		if err != nil {
			_, data = s.historyTraceImageReference(sid, out[i].AttachmentHash)
		}
		verified, err := session.DescribeTraceImage(data, out[i].AttachmentHash)
		if err != nil {
			out[i].Available = false
			out[i].Limitation = "image attachment is missing or cannot be safely decoded"
			continue
		}
		out[i] = verified
		out[i].URL = "/api/trace/image?session_id=" + url.QueryEscape(sid) + "&hash=" + url.QueryEscape(out[i].AttachmentHash)
	}
	return out
}

// A legacy inline image can be served without rewriting history. Restored
// hash-backed Data must still pass ReadTraceImage's filesystem scope checks.
func (s *Server) historyTraceImageReference(sid, hash string) (bool, []byte) {
	if s == nil || s.store == nil {
		return false, nil
	}
	_, messages, err := s.store.Load(sid)
	if err != nil {
		return false, nil
	}
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type != "image" {
				continue
			}
			if block.AttachmentHash == hash {
				return true, nil
			}
			if block.AttachmentHash == "" && block.Data != "" && len(block.Data) <= base64.StdEncoding.EncodedLen(blob.MaxBytes) {
				if data, err := base64.StdEncoding.DecodeString(block.Data); err == nil && blob.Sum(data) == hash {
					return true, data
				}
			}
		}
	}
	return false, nil
}

// historyTraceImages is valid only for the exact history message being
// projected. Callers must not infer this association from turn or list indices.
func historyTraceImages(blocks []llm.ContentBlock) []session.TraceImage {
	images := session.TraceImagesFromBlocks(blocks)
	for i := range images {
		// Store.Load restores bytes and verifies each AttachmentHash. The
		// derived reference therefore points to that exact session's blob.
		if images[i].AttachmentHash != "" && images[i].Bytes > 0 && images[i].Width > 0 && images[i].Height > 0 {
			images[i].Available = true
			images[i].Limitation = ""
		}
	}
	return images
}

func (s *Server) handleTraceImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sid, hash := r.URL.Query().Get("session_id"), r.URL.Query().Get("hash")
	if !validSessionID(sid) || len(hash) != 64 || s.store == nil {
		writeError(w, http.StatusBadRequest, "invalid image reference")
		return
	}
	// Scope is established from an actual persisted reference before opening
	// the blob. Possession of a hash from a different session grants no access.
	allowed := false
	traceStore := s.traceStore
	if traceStore == nil {
		traceStore = rtpkg.CurrentTraceStore()
	}
	if traceStore != nil {
		_ = traceStore.Refresh(sid)
		for _, ev := range traceStore.Events(sid) {
			for _, img := range ev.Images {
				if img.AttachmentHash == hash {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
	}
	var inlineData []byte
	if !allowed {
		allowed, inlineData = s.historyTraceImageReference(sid, hash)
	}
	if !allowed {
		writeError(w, http.StatusNotFound, "image attachment unavailable")
		return
	}
	data, err := s.store.ReadTraceImage(sid, hash)
	if err != nil {
		if inlineData == nil {
			_, inlineData = s.historyTraceImageReference(sid, hash)
		}
		if inlineData == nil {
			writeError(w, http.StatusNotFound, "image attachment unavailable")
			return
		}
		data = inlineData
	}
	img, err := session.DescribeTraceImage(data, hash)
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported image attachment")
		return
	}
	w.Header().Set("Content-Type", img.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", `inline; filename="`+img.Name+`"`)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
