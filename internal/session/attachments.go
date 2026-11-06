package session

import (
	"encoding/base64"
	"fmt"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func (s *Store) attachmentDir(id string) string { return s.path(id) + ".attachments" }

// mapAttachments copies the complete projection; no caller-owned history is mutated.
func (s *Store) mapAttachments(id string, messages []llm.Message, restore bool) ([]llm.Message, error) {
	out := append([]llm.Message(nil), messages...)
	var visit func(llm.ContentBlock) (llm.ContentBlock, error)
	visit = func(b llm.ContentBlock) (llm.ContentBlock, error) {
		if b.Type == "image" {
			if restore && b.AttachmentHash != "" {
				data, err := blob.Read(s.attachmentDir(id), b.AttachmentHash)
				if err != nil {
					return b, err
				}
				b.Data = base64.StdEncoding.EncodeToString(data)
			} else if !restore && b.Data != "" {
				data, err := base64.StdEncoding.DecodeString(b.Data)
				if err != nil {
					return b, fmt.Errorf("session image: invalid base64: %w", err)
				}
				hash, err := blob.Put(s.attachmentDir(id), data)
				if err != nil {
					return b, err
				}
				b.AttachmentHash, b.Data = hash, ""
			} else if b.AttachmentHash != "" {
				if _, err := blob.Read(s.attachmentDir(id), b.AttachmentHash); err != nil {
					return b, err
				}
			}
		}
		if b.ToolResultBlocks != nil {
			parts := make([]llm.ContentBlock, len(b.ToolResultBlocks))
			for i, part := range b.ToolResultBlocks {
				var err error
				parts[i], err = visit(part)
				if err != nil {
					return b, err
				}
			}
			b.ToolResultBlocks = parts
		}
		return b, nil
	}
	for i, m := range messages {
		out[i].Content = make([]llm.ContentBlock, len(m.Content))
		for j, b := range m.Content {
			var err error
			out[i].Content[j], err = visit(b)
			if err != nil {
				return nil, fmt.Errorf("session %s attachment (%s): %w", filepath.Base(id), b.Type, err)
			}
		}
	}
	return out, nil
}

func (s *Store) externalizeEntry(id string, e Entry) (Entry, error) {
	if e.Message != nil {
		msgs, err := s.mapAttachments(id, []llm.Message{*e.Message}, false)
		if err != nil {
			return e, err
		}
		e.Message = &msgs[0]
	}
	if e.Messages != nil {
		msgs, err := s.mapAttachments(id, e.Messages, false)
		if err != nil {
			return e, err
		}
		e.Messages = msgs
	}
	return e, nil
}
