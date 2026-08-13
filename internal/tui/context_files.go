package tui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// projectContextSourcePattern matches the labeled instruction blocks added by
// runtime.AssembleSystemPrompt and runtime.CollectSubdirHints. Those files are
// genuinely present in the provider request even though the model did not load
// them with a Read tool call.
var projectContextSourcePattern = regexp.MustCompile(`<project_context\s+source="([^"]+)">`)

type contextFileEntry struct {
	path       string
	sources    map[string]bool
	lastSeenAt int
}

// renderContextFiles reports concrete files represented in the current model
// request: project instruction files plus successful Read/Edit/Write tool calls
// still present in Loop.History. It deliberately does not walk the workspace;
// /files should describe conversation context, while @-mention owns discovery.
func renderContextFiles(r *REPL) string {
	if r == nil || r.Loop == nil {
		return "Files in current context\n\nNo files are currently loaded."
	}

	history := r.Loop.History()
	entries := contextFilesFromRequest(r.Loop.System, r.Loop.SystemSections, history)
	if len(entries) == 0 {
		return "Files in current context\n\nNo files are currently loaded."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Files in current context (%d)\n", len(entries))
	for _, entry := range entries {
		labels := make([]string, 0, len(entry.sources))
		for _, label := range []string{"project instructions", "read", "edited", "written"} {
			if entry.sources[label] {
				labels = append(labels, label)
			}
		}
		// File paths originate in model-visible prompt/tool material and are
		// therefore untrusted terminal labels. Keep the exact path in the
		// collector for dedupe/provenance, but share the archive-label boundary
		// used by /stats and /think-back before rendering it.
		fmt.Fprintf(&b, "  %s  (%s)\n", safeArchiveLabel(entry.path), strings.Join(labels, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func contextFilesFromRequest(system string, sections []llm.SystemSection, history []llm.Message) []contextFileEntry {
	entries := make(map[string]*contextFileEntry)
	sequence := 0
	add := func(path, source string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		entry := entries[path]
		if entry == nil {
			entry = &contextFileEntry{path: path, sources: make(map[string]bool)}
			entries[path] = entry
		}
		entry.sources[source] = true
		entry.lastSeenAt = sequence
		sequence++
	}
	addProjectSources := func(text string) {
		for _, match := range projectContextSourcePattern.FindAllStringSubmatch(text, -1) {
			if len(match) == 2 {
				add(match[1], "project instructions")
			}
		}
	}

	addProjectSources(system)
	for _, section := range sections {
		addProjectSources(section.Body)
	}

	// A Read path is only loaded after its corresponding tool result succeeds.
	// Edit/Write inputs themselves carry the exact old/new or written content
	// placed in the model request, so they count even when a synthetic fixture
	// omits its later result block.
	results := make(map[string]bool)
	failed := make(map[string]bool)
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				results[block.ToolUseID] = true
				failed[block.ToolUseID] = block.IsError
			}
		}
	}
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "text" {
				addProjectSources(block.Text)
				continue
			}
			if block.Type != "tool_use" || (results[block.ToolUseID] && failed[block.ToolUseID]) {
				continue
			}
			path := contextFilePath(block.ToolInput)
			switch block.ToolName {
			case "Read":
				if results[block.ToolUseID] {
					add(path, "read")
				}
			case "Edit", "NotebookEdit":
				add(path, "edited")
			case "Write":
				add(path, "written")
			}
		}
	}

	out := make([]contextFileEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, *entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		leftProject := out[i].sources["project instructions"]
		rightProject := out[j].sources["project instructions"]
		if leftProject != rightProject {
			return leftProject
		}
		return out[i].lastSeenAt > out[j].lastSeenAt
	})
	return out
}

func contextFilePath(input map[string]any) string {
	for _, key := range []string{"path", "file_path"} {
		if value, ok := input[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
