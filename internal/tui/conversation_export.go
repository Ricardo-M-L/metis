package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/security"
)

// conversationText renders the provider-neutral history as the same kind of
// readable, glyph-led transcript Claude Code writes from /export. It is
// deliberately built from the visible message blocks instead of serializing
// the session record: headers contain the system prompt, permission grants,
// provider details and other resume-only state that must not enter a file a
// user may share.
func conversationText(messages []llm.Message) string {
	toolNames := make(map[string]string)
	parts := make([]string, 0, len(messages))

	for _, message := range messages {
		if message.Role == llm.RoleSystem {
			continue
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				text := cleanExportText(block.Text)
				if text == "" {
					continue
				}
				switch message.Role {
				case llm.RoleUser:
					parts = append(parts, prefixExportBlock("❯ ", "  ", text))
				case llm.RoleAssistant:
					parts = append(parts, prefixExportBlock("⏺ ", "  ", text))
				case llm.RoleTool:
					parts = append(parts, prefixExportBlock("  ⎿  ", "     ", text))
				}
			case "tool_use":
				if message.Role != llm.RoleAssistant {
					continue
				}
				name := cleanToolName(block.ToolName)
				if name == "" {
					name = "Tool"
				}
				if block.ToolUseID != "" {
					toolNames[block.ToolUseID] = name
				}
				parts = append(parts, "⏺ "+name+formatExportToolInput(block.ToolInput))
			case "tool_result":
				text := cleanExportText(block.ToolResult)
				if text == "" && len(block.ToolResultBlocks) == 0 {
					text = "(no output)"
				}
				label := toolNames[block.ToolUseID]
				if label != "" {
					label += ": "
				}
				if block.IsError {
					label += "Error: "
				}
				if text != "" {
					parts = append(parts, prefixExportBlock("  ⎿  ", "     ", label+text))
				}
				for _, resultBlock := range block.ToolResultBlocks {
					switch resultBlock.Type {
					case "text":
						nested := cleanExportText(resultBlock.Text)
						if nested != "" && nested != text {
							parts = append(parts, prefixExportBlock("     ", "     ", nested))
						}
					case "image":
						parts = append(parts, "     [image omitted from text export]")
					}
				}
			case "image":
				// Never write the base64 Data field. A placeholder retains the
				// conversational fact that an image was supplied.
				marker := "❯ "
				if message.Role == llm.RoleAssistant || message.Role == llm.RoleTool {
					marker = "  ⎿  "
				}
				parts = append(parts, marker+"[image omitted from text export]")
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func prefixExportBlock(firstPrefix, continuationPrefix, text string) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
			b.WriteString(continuationPrefix)
		} else {
			b.WriteString(firstPrefix)
		}
		b.WriteString(strings.TrimRight(line, " \t\r"))
	}
	return strings.TrimRight(b.String(), "\n")
}

var internalExportSections = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<system-reminder(?:\s[^>]*)?>.*?</system-reminder\s*>`),
	regexp.MustCompile(`(?is)<memory-context(?:\s[^>]*)?>.*?</memory-context\s*>`),
	regexp.MustCompile(`(?is)<auto-retrieve(?:\s[^>]*)?>.*?</auto-retrieve\s*>`),
	regexp.MustCompile(`(?is)<peer_message(?:\s[^>]*)?>.*?</peer_message\s*>`),
	regexp.MustCompile(`(?is)<task-context(?:\s[^>]*)?>.*?</task-context\s*>`),
	regexp.MustCompile(`(?is)<project-context(?:\s[^>]*)?>.*?</project-context\s*>`),
}

var unterminatedInternalSection = regexp.MustCompile(`(?is)<(?:system-reminder|memory-context|auto-retrieve|peer_message|task-context|project-context)(?:\s[^>]*)?>.*\z`)

// namedSecretAssignment catches generic credentials that do not have a
// provider-specific prefix. security.Redact handles high-confidence token
// shapes; this second pass handles env/JSON-style assignments while keeping
// the field name useful in the transcript.
var namedSecretAssignment = regexp.MustCompile(`(?i)\b((?:[a-z][a-z0-9_.-]*[_-])?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|private[_-]?key|secret|password|passwd)|authorization|cookie)(["']?\s*[:=]\s*["']?)([^\s"',;}\]]{4,})`)

var incompletePrivateKey = regexp.MustCompile(`(?is)-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----|$)`)

func cleanExportText(text string) string {
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = ansi.Strip(text)
	for _, re := range internalExportSections {
		text = re.ReplaceAllString(text, "")
	}
	// An unterminated internal block is safer to remove through end-of-text
	// than to publish. It may follow visible tool output, so anchoring this
	// check at the start of the block would leak interrupted reminders that
	// were appended after an otherwise ordinary line.
	text = unterminatedInternalSection.ReplaceAllString(text, "")
	text = incompletePrivateKey.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	text = security.Redact(text)
	text = namedSecretAssignment.ReplaceAllString(text, "$1$2[REDACTED]")
	text = strings.ReplaceAll(text, "\x00", "")
	return strings.TrimSpace(text)
}

func cleanToolName(name string) string {
	name = cleanExportText(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func formatExportToolInput(input map[string]any) string {
	if len(input) == 0 {
		return "()"
	}
	safe := sanitizeExportMap(input)
	if len(safe) == 0 {
		return "()"
	}
	raw, err := json.Marshal(safe)
	if err != nil {
		return "([arguments omitted])"
	}
	return "(" + string(raw) + ")"
}

func sanitizeExportMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
		switch normalized {
		case "provider_hint", "system", "system_prompt", "metadata", "tool_use_id", "session_id", "parent_session_id":
			continue
		case "data", "base64", "image_data", "bytes":
			out[key] = "[binary data omitted]"
			continue
		}
		if sensitiveExportField(normalized) {
			out[key] = "[REDACTED]"
			continue
		}
		out[key] = sanitizeExportValue(value)
	}
	return out
}

func sanitizeExportValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return cleanExportText(v)
	case map[string]any:
		return sanitizeExportMap(v)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, sanitizeExportValue(item))
		}
		return out
	case []byte:
		return "[binary data omitted]"
	case bool,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return v
	default:
		// Tool inputs normally come from JSON as map[string]any / []any, but
		// internal callers can supply typed maps, slices, arrays or structs.
		// Normalize those values through JSON before recursing so nested
		// credentials cannot bypass the field-name and text redactors merely
		// because their concrete Go type is map[string]string or []string.
		raw, err := json.Marshal(v)
		if err != nil {
			return "[value omitted]"
		}
		var normalized any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			return "[value omitted]"
		}
		switch normalized.(type) {
		case string, map[string]any, []any:
			return sanitizeExportValue(normalized)
		default:
			return normalized
		}
	}
}

func sensitiveExportField(key string) bool {
	for _, suffix := range []string{
		"api_key", "apikey", "token", "secret", "password", "passwd",
		"private_key", "authorization", "cookie", "credential",
	} {
		if key == suffix || strings.HasSuffix(key, "_"+suffix) {
			return true
		}
	}
	return false
}

func firstExportPrompt(messages []llm.Message) string {
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type != "text" {
				continue
			}
			text := cleanExportText(block.Text)
			if text == "" {
				continue
			}
			first, _, _ := strings.Cut(text, "\n")
			first = strings.TrimSpace(first)
			if utf8.RuneCountInString(first) > 50 {
				runes := []rune(first)
				first = string(runes[:49]) + "…"
			}
			return first
		}
	}
	return ""
}

func exportFilename(messages []llm.Message, now time.Time) string {
	timestamp := now.Format("2006-01-02-150405")
	slug := sanitizeExportFilename(firstExportPrompt(messages))
	if slug == "" {
		return "conversation-" + timestamp + ".txt"
	}
	return timestamp + "-" + slug + ".txt"
}

func sanitizeExportFilename(text string) string {
	text = strings.ToLower(text)
	var b strings.Builder
	lastDash := false
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func ensureTextFilename(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".txt") {
		return name
	}
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	return name + ".txt"
}

func exportConversationToFile(messages []llm.Message, requested string, now time.Time) (string, error) {
	requested = strings.TrimSpace(requested)
	var path string
	if requested == "" {
		dir := filepath.Join(config.Home(), "exports")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		path = filepath.Join(dir, exportFilename(messages, now))
	} else {
		path = ensureTextFilename(requested)
		if !filepath.IsAbs(path) {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			path = absolute
		}
	}
	if err := writePrivateExport(path, []byte(conversationText(messages))); err != nil {
		return "", err
	}
	return path, nil
}

// writePrivateExport applies 0600 even when replacing an existing file.
// os.WriteFile's mode argument only affects newly created files, which could
// otherwise leave a transcript world-readable when the chosen path was 0644.
func writePrivateExport(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func exportSuccess(path string) string {
	return fmt.Sprintf("Conversation exported to: %s", path)
}

func exportFailure(err error) string {
	return fmt.Sprintf("Failed to export conversation: %v", err)
}
