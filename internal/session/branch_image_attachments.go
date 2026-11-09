package session

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

var branchImageReferencePatterns = []*regexp.Regexp{
	regexp.MustCompile(`attachment_id\s*=\s*([a-f0-9]{64})\b`),
	regexp.MustCompile(`"attachment_id"\s*:\s*"([a-f0-9]{64})"`),
}

type branchImagePosition struct{ line, message int }

func (p branchImagePosition) beforeOrEqual(other branchImagePosition) bool {
	return p.line < other.line || (p.line == other.line && p.message <= other.message)
}

type branchImageMessage struct {
	key    string
	origin branchImagePosition
}

type branchImageAuthority struct {
	position  branchImagePosition
	mediaType string
	call      string
	logOnly   bool
	anchored  bool
}

const branchImageBoundaryHint = "metis.branch_image_boundary"

// branchImageAttachments copies only explicit references in the selected
// snapshot, backed by source image receipts at its proven history boundary.
// The lineage header and a bare textual hash never confer attachment access.
func (s *Store) branchImageAttachments(id string, messages []llm.Message) ([][]llm.ContentBlock, error) {
	wanted := make(map[string]bool)
	var references func([]llm.ContentBlock)
	references = func(blocks []llm.ContentBlock) {
		for _, block := range blocks {
			for _, body := range []string{block.Text, block.ToolResult} {
				for _, pattern := range branchImageReferencePatterns {
					for _, match := range pattern.FindAllStringSubmatch(body, -1) {
						wanted[match[1]] = true
					}
				}
			}
			if block.Type == "tool_use" && block.ToolName == "ViewImage" {
				if hash, ok := block.ToolInput["attachment_id"].(string); ok {
					wanted[hash] = true
				}
			}
			references(block.ToolResultBlocks)
		}
	}
	for _, message := range messages {
		references(message.Content)
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	target := make([]string, len(messages))
	for i, message := range messages {
		key, err := branchImageMessageKey(message)
		if err != nil {
			return nil, err
		}
		target[i] = key
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.path(id))
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("branch image attachments: invalid source ledger")
	}
	f, err := os.Open(s.path(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var state []branchImageMessage
	proofs := make(map[string][]branchImageAuthority)
	receipts := make(map[string]branchImagePosition)
	var authorize func([]llm.ContentBlock, branchImagePosition, bool)
	authorize = func(blocks []llm.ContentBlock, position branchImagePosition, logOnly bool) {
		for _, block := range blocks {
			if !logOnly && block.Type == "tool_result" && block.TraceCallID != "" {
				if old, ok := receipts[block.TraceCallID]; !ok || position.beforeOrEqual(old) {
					receipts[block.TraceCallID] = position
				}
			}
			if block.Type == "image" {
				hash := block.AttachmentHash
				if block.Data != "" && len(block.Data) <= base64.StdEncoding.EncodedLen(blob.MaxBytes) {
					if raw, decodeErr := base64.StdEncoding.DecodeString(block.Data); decodeErr == nil {
						hash = blob.Sum(raw)
					}
				}
				if wanted[hash] {
					proofs[hash] = append(proofs[hash], branchImageAuthority{position: position, mediaType: block.MediaType, call: block.TraceCallID, logOnly: logOnly, anchored: block.ProviderHint[branchImageBoundaryHint] == "true"})
				}
			}
			authorize(block.ToolResultBlocks, position, logOnly)
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), resumeScannerMaxBytes())
	line, badLine := 0, 0
	for scanner.Scan() {
		line++
		if badLine != 0 {
			return nil, fmt.Errorf("branch image attachments: corrupt source entry at line %d", badLine)
		}
		var entry Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			badLine = line
			continue // same torn-tail contract as Load
		}
		switch entry.Type {
		case "message":
			if entry.Message != nil {
				position := branchImagePosition{line, len(state)}
				authorize(entry.Message.Content, position, false)
				key, err := branchImageMessageKey(*entry.Message)
				if err != nil {
					return nil, err
				}
				state = append(state, branchImageMessage{key: key, origin: position})
			}
		case "history_replace":
			next := make([]branchImageMessage, len(entry.Messages))
			for i, message := range entry.Messages {
				position := branchImagePosition{line, i}
				key, err := branchImageMessageKey(message)
				if err != nil {
					return nil, err
				}
				next[i] = branchImageMessage{key: key, origin: position}
				if i < len(state) && state[i].key == key {
					// An unchanged checkpoint must not move an early --keep
					// boundary past images produced later in the parent.
					next[i].origin = state[i].origin
				}
				authorize(message.Content, next[i].origin, false)
			}
			state = next
		case "image_attachment":
			if entry.Message != nil {
				authorize(entry.Message.Content, branchImagePosition{line, -1}, true)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(proofs) == 0 {
		return nil, nil // foreign/bare references grant no authority
	}
	boundary := branchImagePosition{}
	prefixBoundaries := make([]branchImagePosition, len(target))
	for i, key := range target {
		if i >= len(state) || state[i].key != key {
			return nil, fmt.Errorf("branch image attachments: cannot establish an exact source-history prefix boundary")
		}
		if boundary.beforeOrEqual(state[i].origin) {
			boundary = state[i].origin
		}
		prefixBoundaries[i] = boundary
	}
	// A tool registration is fsynced before the completed batch's assistant
	// and results enter the ledger. Its root occurrence receipt, rather than
	// that physical write order, supplies the logical --keep boundary.
	authorities := make(map[string]branchImageAuthority)
	for hash, candidates := range proofs {
		var native *branchImageAuthority
		for i := range candidates {
			if !candidates[i].logOnly && (native == nil || candidates[i].position.beforeOrEqual(native.position)) {
				native = &candidates[i]
			}
		}
		uncertain := false
		for _, candidate := range candidates {
			if candidate.logOnly && !candidate.anchored {
				position, known := receipts[candidate.call]
				if candidate.call == "" && native != nil {
					position, known = native.position, true
				}
				if !known {
					uncertain = uncertain || candidate.position.beforeOrEqual(boundary)
					continue
				}
				if candidate.position.beforeOrEqual(position) {
					candidate.position = position
				}
			}
			if candidate.position.beforeOrEqual(boundary) {
				if old, ok := authorities[hash]; !ok || candidate.position.beforeOrEqual(old.position) {
					authorities[hash] = candidate
				}
			}
		}
		if _, known := authorities[hash]; !known && uncertain {
			return nil, fmt.Errorf("branch image attachment %s: no reliable root receipt boundary", hash)
		}
	}
	ids := make([]string, 0, len(authorities))
	for hash, authority := range authorities {
		if authority.position.beforeOrEqual(boundary) {
			ids = append(ids, hash)
		}
	}
	sort.Strings(ids)
	images := make([][]llm.ContentBlock, len(messages))
	for _, hash := range ids {
		raw, err := s.ReadImageAttachment(id, hash)
		if err != nil {
			return nil, fmt.Errorf("branch image attachment %s: %w", hash, err)
		}
		mediaType := authorities[hash].mediaType
		if mediaType == "" {
			mediaType = http.DetectContentType(raw)
		}
		for i, prefix := range prefixBoundaries {
			if authorities[hash].position.beforeOrEqual(prefix) {
				// Re-anchor to this branch's proven message boundary. Keeping a
				// parent call ID would require an omitted parent receipt, while
				// registering every image at the header would expose future images
				// to a subsequent branch with an earlier --keep cutoff.
				images[i] = append(images[i], llm.ContentBlock{Type: "image", AttachmentHash: hash, MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(raw), ProviderHint: map[string]string{branchImageBoundaryHint: "true"}})
				break
			}
		}
	}
	return images, nil
}

func branchImageMessageKey(message llm.Message) (string, error) {
	var normalize func([]llm.ContentBlock) ([]llm.ContentBlock, error)
	normalize = func(blocks []llm.ContentBlock) ([]llm.ContentBlock, error) {
		out := append([]llm.ContentBlock(nil), blocks...)
		for i := range out {
			block := &out[i]
			if block.Type == "image" && block.Data != "" {
				if len(block.Data) > base64.StdEncoding.EncodedLen(blob.MaxBytes) {
					return nil, fmt.Errorf("branch image attachments: image exceeds size limit")
				}
				raw, err := base64.StdEncoding.DecodeString(block.Data)
				if err != nil {
					return nil, fmt.Errorf("branch image attachments: invalid image bytes")
				}
				block.AttachmentHash, block.Data = blob.Sum(raw), ""
			}
			if block.Type == "tool_use" && block.ToolInput == nil {
				block.ToolInput = map[string]any{}
			}
			var err error
			block.ToolResultBlocks, err = normalize(block.ToolResultBlocks)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	var err error
	message.Content, err = normalize(message.Content)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(message)
	return string(encoded), err
}
