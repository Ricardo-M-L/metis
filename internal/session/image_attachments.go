package session

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

// RecordImageAttachments durably assigns tool-produced images to their calling
// root session. Child/Fork transcripts remain separate from the root model
// conversation, so these log-only records preserve recall without inserting
// child messages into that conversation. Bare hashes cannot create authority.
func (s *Store) RecordImageAttachments(sessionID string, blocks []llm.ContentBlock) error {
	var images []llm.ContentBlock
	var visit func([]llm.ContentBlock)
	visit = func(parts []llm.ContentBlock) {
		for _, part := range parts {
			if part.Type == "image" && part.Data != "" {
				part.ToolResultBlocks = nil
				images = append(images, part)
			}
			visit(part.ToolResultBlocks)
		}
	}
	visit(blocks)
	if len(images) == 0 {
		return nil
	}
	if s == nil || !validTraceSessionID(sessionID) {
		return fmt.Errorf("invalid image attachment session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.path(sessionID))
	if err != nil {
		return fmt.Errorf("image attachment session: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("image attachment session ledger is not a regular file")
	}
	if info, err := os.Lstat(s.attachmentDir(sessionID)); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid attachment directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	message := llm.Message{Role: llm.RoleUser, Content: images}
	// externalizeEntry commits/checksums/fsyncs each blob before the reference
	// is appended. A failed ledger fsync rolls the entry back; orphan blobs do
	// not grant read permission and a retry can safely reuse them.
	return s.appendEntryLocked(sessionID, Entry{Type: "image_attachment", Message: &message}, true)
}

// ReadImageAttachment reads a canonical SHA-256 image ID only after proving
// that this session's own conversation ledger contains the image. In
// particular, a bare blob, a textual mention of a hash, or another session's
// image does not grant access. Old message and history_replace records remain
// authoritative after compaction removes their images from active context.
func (s *Store) ReadImageAttachment(sessionID, attachmentID string) ([]byte, error) {
	decodedID, err := hex.DecodeString(attachmentID)
	if s == nil || !validTraceSessionID(sessionID) || err != nil || len(decodedID) != 32 || strings.ToLower(attachmentID) != attachmentID {
		return nil, fmt.Errorf("invalid image attachment identity")
	}
	path := s.path(sessionID)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("image attachment session: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("image attachment session ledger is not a regular file")
	}
	if limit := resumePhysicalMaxBytes(); limit > 0 && info.Size() > limit {
		return nil, &ResumeTooLargeError{Path: path, Bytes: info.Size(), CapBytes: limit, Scope: ResumeSizePhysicalLedger}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("image attachment session: %w", err)
	}
	defer f.Close()

	var referenced, hasHeader bool
	var inline []byte
	var visit func([]llm.ContentBlock)
	visit = func(blocks []llm.ContentBlock) {
		for _, block := range blocks {
			if block.Type == "image" {
				if block.Data != "" {
					// Legacy ledgers may still store inline bytes. Their decoded
					// content, rather than an unverified metadata field, defines ID.
					if len(block.Data) <= base64.StdEncoding.EncodedLen(blob.MaxBytes) {
						if data, decodeErr := base64.StdEncoding.DecodeString(block.Data); decodeErr == nil && len(data) <= blob.MaxBytes && blob.Sum(data) == attachmentID {
							referenced, inline = true, data
						}
					}
				} else if block.AttachmentHash == attachmentID {
					referenced = true
				}
			}
			visit(block.ToolResultBlocks)
		}
	}
	// Follow Load's torn-tail rule while checking the complete ledger, even
	// after a match. A malformed middle record must not be hidden by a match.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), resumeScannerMaxBytes())
	line, badLine := 0, 0
	for scanner.Scan() {
		line++
		if badLine > 0 {
			return nil, fmt.Errorf("decode image attachment session entry at line %d", badLine)
		}
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			badLine = line
			continue
		}
		switch entry.Type {
		case "header":
			if entry.Header != nil {
				hasHeader = true
			}
		case "message", "image_attachment":
			if entry.Message != nil {
				visit(entry.Message.Content)
			}
		case "history_replace":
			for _, message := range entry.Messages {
				visit(message.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan image attachment session: %w", err)
	}
	if !hasHeader {
		return nil, fmt.Errorf("image attachment session header missing")
	}
	if !referenced {
		return nil, fmt.Errorf("image attachment is not referenced by the current session")
	}
	if inline != nil {
		return inline, nil
	}
	// ReadTraceImage bounds allocation, rejects symlink attachment dirs and
	// verifies the blob's content hash. No remote URL is ever fetched.
	data, err := s.ReadTraceImage(sessionID, attachmentID)
	if err != nil {
		return nil, fmt.Errorf("read image attachment: %w", err)
	}
	return data, nil
}
