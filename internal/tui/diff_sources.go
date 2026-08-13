package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	udiff "github.com/aymanbagabas/go-udiff"
)

const (
	diffCollectionTimeout = 10 * time.Second
	diffMaxCommandBytes   = 4 << 20
	diffMaxTotalBytes     = 8 << 20
	diffMaxFileBytes      = 1 << 20
)

var (
	errDiffOutputLimit = errors.New("diff output exceeded the display limit")
	errDiffFileLimit   = errors.New("file exceeds the diff display limit")
)

type diffResultMsg struct {
	requestID uint64
	loop      *agent.Loop
	sessionID string
	sources   []screen.DiffSource
}

type diffCollection struct {
	files     []screen.DiffFile
	status    string
	truncated bool
}

type cappedDiffWriter struct {
	buf       bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *cappedDiffWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining <= 0 {
		w.exceeded = true
		return n, nil
	}
	keep := n
	if keep > w.remaining {
		keep = w.remaining
		w.exceeded = true
	}
	_, _ = w.buf.Write(p[:keep])
	w.remaining -= keep
	return n, nil
}

func runBoundedDiffCommand(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, errDiffOutputLimit
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	writer := &cappedDiffWriter{remaining: limit}
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return writer.buf.Bytes(), ctxErr
	}
	if writer.exceeded {
		return writer.buf.Bytes(), errDiffOutputLimit
	}
	return writer.buf.Bytes(), err
}

func diffContextStatus(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "diff collection timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "diff collection canceled"
	}
	return "diff collection failed: " + err.Error()
}

func diffCommandFailure(command string, output []byte, err error) diffCollection {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return diffCollection{status: diffContextStatus(err)}
	}
	if errors.Is(err, errDiffOutputLimit) {
		return diffCollection{status: command + ": output exceeded the display limit", truncated: true}
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return diffCollection{status: command + ": " + detail}
}

// openDiffViewer starts the canonical /diff collection. No Git or filesystem
// work runs on Bubble Tea's Update goroutine; only the typed result handler
// below is allowed to install the completed screen.
func (m *Model) openDiffViewer() tea.Cmd {
	if m == nil {
		return nil
	}
	if m.diffPending {
		m.messages = append(m.messages, Message{Role: "info", Content: "(diff collection already in progress)", Timestamp: time.Now()})
		return nil
	}
	m.diffSeq++
	requestID := m.diffSeq
	loop := m.loop
	sessionID := m.sessionID
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	m.diffPending = true
	m.messages = append(m.messages, Message{Role: "info", Content: "(collecting uncommitted and per-turn changes...)", Timestamp: time.Now()})
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, diffCollectionTimeout)
		defer cancel()
		return diffResultMsg{
			requestID: requestID,
			loop:      loop,
			sessionID: sessionID,
			sources:   collectDiffSources(ctx, loop),
		}
	}
}

func collectDiffSources(ctx context.Context, loop *agent.Loop) []screen.DiffSource {
	working := buildWorkingTreeDiffFilesContext(ctx)
	sources := []screen.DiffSource{{
		Label:    "Working tree",
		Subtitle: "all uncommitted changes",
		Files:    working.files,
		Error:    safeArchiveLabel(working.status),
	}}
	if working.truncated {
		sources[0].Subtitle += " · truncated at resource limit"
	}
	if loop != nil && ctx.Err() == nil {
		sources = append(sources, buildModelTurnDiffSourcesContext(ctx, loop)...)
	}
	return sources
}

func (m *Model) handleDiffResult(msg diffResultMsg) {
	if m == nil || !m.diffPending || msg.requestID != m.diffSeq {
		return
	}
	m.diffPending = false
	if msg.loop != m.loop || msg.sessionID != m.sessionID {
		m.messages = append(m.messages, Message{Role: "warning", Content: "diff result ignored because the active session changed", Timestamp: time.Now()})
		return
	}
	viewer := screen.NewDiffViewerScreenWithSources(msg.sources)
	viewer.Resize(m.width, m.height)
	m.activeScreen = viewer
}

// buildWorkingTreeDiffFiles preserves the established HEAD-based collector,
// with an unborn-repository path for the first commit. Before HEAD exists,
// every cached/untracked file is an addition relative to the empty tree.
func buildWorkingTreeDiffFiles() ([]screen.DiffFile, error) {
	result := buildWorkingTreeDiffFilesContext(context.Background())
	if result.status != "" {
		return result.files, errors.New(result.status)
	}
	return result.files, nil
}

func buildWorkingTreeDiffFilesContext(ctx context.Context) diffCollection {
	budget := diffMaxTotalBytes
	inside, err := runBoundedDiffCommand(ctx, "", min(budget, diffMaxCommandBytes), "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return diffCollection{status: diffContextStatus(ctxErr)}
		}
		message := strings.TrimSpace(string(inside))
		if message == "" {
			message = "not a Git working tree"
		}
		return diffCollection{status: "git: " + message}
	}
	budget -= len(inside)
	_, headErr := runBoundedDiffCommand(ctx, "", min(budget, diffMaxCommandBytes), "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		patch, diffErr := runBoundedDiffCommand(ctx, "", min(budget, diffMaxCommandBytes),
			"diff", "HEAD", "--no-color", "--no-ext-diff")
		if diffErr != nil {
			return diffCommandFailure("git diff", patch, diffErr)
		}
		budget -= len(patch)
		files := parseViewerPatch(string(patch))
		untracked, listErr := runBoundedDiffCommand(ctx, "", min(budget, diffMaxCommandBytes),
			"ls-files", "--others", "--exclude-standard", "-z")
		if listErr != nil {
			return diffCommandFailure("git ls-files", untracked, listErr)
		}
		budget -= len(untracked)
		truncated := false
		for _, path := range strings.Split(string(untracked), "\x00") {
			if path == "" {
				continue
			}
			file, status := buildWorkingTreeAdditionContext(ctx, path, &budget)
			if status == nil {
				files = append(files, file)
				continue
			}
			if errors.Is(status, errDiffFileLimit) || errors.Is(status, errDiffOutputLimit) {
				truncated = true
				continue
			}
			return diffCollection{files: files, status: status.Error(), truncated: truncated}
		}
		return diffCollection{files: files, truncated: truncated}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return diffCollection{status: diffContextStatus(ctxErr)}
	}
	out, err := runBoundedDiffCommand(ctx, "", min(budget, diffMaxCommandBytes),
		"ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return diffCommandFailure("git ls-files", out, err)
	}
	budget -= len(out)
	seen := make(map[string]bool)
	var files []screen.DiffFile
	truncated := false
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		file, status := buildWorkingTreeAdditionContext(ctx, path, &budget)
		if status == nil {
			files = append(files, file)
			continue
		}
		if errors.Is(status, errDiffFileLimit) || errors.Is(status, errDiffOutputLimit) {
			truncated = true
			continue
		}
		return diffCollection{files: files, status: status.Error(), truncated: truncated}
	}
	return diffCollection{files: files, truncated: truncated}
}

func buildWorkingTreeAddition(path string) (screen.DiffFile, bool) {
	budget := diffMaxTotalBytes
	file, err := buildWorkingTreeAdditionContext(context.Background(), path, &budget)
	return file, err == nil
}

func buildWorkingTreeAdditionContext(ctx context.Context, path string, budget *int) (screen.DiffFile, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return screen.DiffFile{}, ctxErr
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return screen.DiffFile{}, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return screen.DiffFile{}, fmt.Errorf("cannot read untracked file %q", path)
	}
	if info.Size() > diffMaxFileBytes {
		return screen.DiffFile{}, fmt.Errorf("%w: %s", errDiffFileLimit, path)
	}
	limit := min(*budget, diffMaxFileBytes)
	patch, diffErr := runBoundedDiffCommand(ctx, "", limit,
		"--no-pager", "diff", "--no-index", "--no-color", "--no-ext-diff",
		"--", os.DevNull, filepath.FromSlash(path))
	*budget -= len(patch)
	if diffErr != nil {
		if exit, ok := diffErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return screen.DiffFile{}, diffErr
		}
	}
	parsed := parseViewerPatch(string(patch))
	if len(parsed) == 0 {
		return screen.DiffFile{Path: path, Status: "A"}, nil
	}
	file := parsed[0]
	file.Path = path
	file.Status = "A"
	return file, nil
}

const maxDiffTurnSources = 8

// buildModelTurnDiffSources prefers shadow-git patches because they include
// Bash/deletion changes that structured tool inputs cannot describe. Turns
// without a materialized checkpoint fall back to persisted Edit/Write input.
func buildModelTurnDiffSources(loop *agent.Loop) []screen.DiffSource {
	return buildModelTurnDiffSourcesContext(context.Background(), loop)
}

func buildModelTurnDiffSourcesContext(ctx context.Context, loop *agent.Loop) []screen.DiffSource {
	if loop == nil {
		return nil
	}
	history := loop.History()
	prompts := turnPromptMap(history)
	byTurn := make(map[int]screen.DiffSource)
	for _, checkpointDiff := range loop.CheckpointTurnDiffsContext(ctx, diffMaxTotalBytes) {
		if ctx.Err() != nil {
			break
		}
		files := parseViewerPatch(checkpointDiff.Patch)
		if len(files) == 0 {
			continue
		}
		byTurn[checkpointDiff.Turn] = screen.DiffSource{
			Label:    fmt.Sprintf("Turn %d", checkpointDiff.Turn),
			Subtitle: diffSourceSubtitle("checkpoint interval (best effort)", prompts[checkpointDiff.Turn]),
			Files:    files,
		}
	}
	for _, fallback := range buildTurnDiffSources(history) {
		if ctx.Err() != nil {
			break
		}
		turn, ok := diffSourceTurn(fallback.Label)
		if !ok {
			continue
		}
		if _, exists := byTurn[turn]; !exists {
			byTurn[turn] = fallback
		}
	}
	turns := make([]int, 0, len(byTurn))
	for turn := range byTurn {
		turns = append(turns, turn)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(turns)))
	if len(turns) > maxDiffTurnSources {
		turns = turns[:maxDiffTurnSources]
	}
	out := make([]screen.DiffSource, 0, len(turns))
	for _, turn := range turns {
		out = append(out, byTurn[turn])
	}
	return out
}

type turnPatchSource struct {
	index   int
	prompt  string
	patches map[string][]string
	status  map[string]string
}

// buildTurnDiffSources groups successful file mutations by user turn and
// reconstructs their patch hunks from the tool inputs already retained in
// provider history. Edit records are exact old/new patches. Write records are
// additions when the path was first seen; overwrites are surfaced as the new
// content because the tool protocol does not persist their prior full body.
func buildTurnDiffSources(history []llm.Message) []screen.DiffSource {
	turns := make([]turnPatchSource, 0)
	var current *turnPatchSource
	turnIndex := 0
	seenPaths := make(map[string]bool)
	succeeded := successfulToolUses(history)

	for _, message := range history {
		if message.Role == llm.RoleUser && visibleTurnPrompt(message) != "" {
			turnIndex++
			turns = append(turns, turnPatchSource{
				index: turnIndex, prompt: visibleTurnPrompt(message),
				patches: make(map[string][]string), status: make(map[string]string),
			})
			current = &turns[len(turns)-1]
			continue
		}
		for _, block := range message.Content {
			if block.Type != "tool_use" {
				continue
			}
			path := contextFilePath(block.ToolInput)
			if path == "" {
				continue
			}
			if block.ToolName == "Read" {
				if succeeded[block.ToolUseID] {
					seenPaths[path] = true
				}
				continue
			}
			if current == nil || !succeeded[block.ToolUseID] {
				continue
			}
			switch block.ToolName {
			case "Edit":
				oldText, newText := editTexts(block.ToolInput)
				patch := unifiedPatch(path, oldText, newText)
				if patch != "" {
					current.patches[path] = append(current.patches[path], patch)
					current.status[path] = "M"
				}
				seenPaths[path] = true
			case "Write":
				content, _ := block.ToolInput["content"].(string)
				if content == "" {
					continue
				}
				isNew := !seenPaths[path]
				patch := unifiedPatch(path, "", content)
				if patch != "" {
					current.patches[path] = append(current.patches[path], patch)
					if isNew {
						current.status[path] = "A"
					} else {
						current.status[path] = "M"
					}
				}
				seenPaths[path] = true
			}
		}
	}

	sources := make([]screen.DiffSource, 0, len(turns))
	for index := len(turns) - 1; index >= 0; index-- {
		turn := turns[index]
		if len(turn.patches) == 0 {
			continue
		}
		paths := make([]string, 0, len(turn.patches))
		for path := range turn.patches {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		filesByPath := make(map[string]screen.DiffFile, len(paths))
		for _, path := range paths {
			for _, patch := range turn.patches[path] {
				parsed := parseViewerPatch(patch)
				for _, file := range parsed {
					combined := filesByPath[path]
					combined.Path = path
					combined.Status = turn.status[path]
					combined.Hunks = append(combined.Hunks, file.Hunks...)
					filesByPath[path] = combined
				}
			}
		}
		files := make([]screen.DiffFile, 0, len(filesByPath))
		for _, path := range paths {
			if file, ok := filesByPath[path]; ok {
				files = append(files, file)
			}
		}
		if len(files) == 0 {
			continue
		}
		sources = append(sources, screen.DiffSource{
			Label:    fmt.Sprintf("Turn %d", turn.index),
			Subtitle: diffSourceSubtitle("reconstructed intent from tool input (best effort)", turn.prompt),
			Files:    files,
		})
	}
	return sources
}

func successfulToolUses(history []llm.Message) map[string]bool {
	succeeded := make(map[string]bool)
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID != "" && !block.IsError {
				succeeded[block.ToolUseID] = true
			}
		}
	}
	return succeeded
}

func turnPromptMap(history []llm.Message) map[int]string {
	prompts := make(map[int]string)
	turn := 0
	for _, message := range history {
		if message.Role != llm.RoleUser {
			continue
		}
		if prompt := visibleTurnPrompt(message); prompt != "" {
			turn++
			prompts[turn] = prompt
		}
	}
	return prompts
}

func diffSourceTurn(label string) (int, bool) {
	const prefix = "Turn "
	if !strings.HasPrefix(label, prefix) {
		return 0, false
	}
	turn, err := strconv.Atoi(strings.TrimPrefix(label, prefix))
	return turn, err == nil && turn > 0
}

func visibleTurnPrompt(message llm.Message) string {
	for _, block := range message.Content {
		if block.Type != "text" {
			continue
		}
		text := stripInternalReviewPrompt(block.Text)
		if text != "" {
			return strings.Join(strings.Fields(text), " ")
		}
	}
	return ""
}

func editTexts(input map[string]any) (string, string) {
	oldText, _ := input["old"].(string)
	newText, _ := input["new"].(string)
	if oldText == "" && newText == "" {
		oldText, _ = input["old_string"].(string)
		newText, _ = input["new_string"].(string)
	}
	return oldText, newText
}

func unifiedPatch(path, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	edits := udiff.Strings(oldText, newText)
	if len(edits) == 0 {
		return ""
	}
	unified, err := udiff.ToUnifiedDiff(path, path, oldText, edits, 1)
	if err != nil || len(unified.Hunks) == 0 {
		return ""
	}
	return "diff --git a/" + path + " b/" + path + "\n" + unified.String()
}

// parseViewerPatch preserves paths containing spaces or non-ASCII bytes. The
// legacy parser reads the diff --git header with strings.Fields, which is fine
// for ordinary paths but ambiguous for `a/new file.go b/new file.go`. Split
// into file blocks and take the authoritative +++/--- path before parsing the
// hunks with the shared implementation.
func parseViewerPatch(patch string) []screen.DiffFile {
	starts := make([]int, 0)
	for offset := 0; offset < len(patch); {
		index := strings.Index(patch[offset:], "diff --git ")
		if index < 0 {
			break
		}
		absolute := offset + index
		if absolute == 0 || patch[absolute-1] == '\n' {
			starts = append(starts, absolute)
		}
		offset = absolute + len("diff --git ")
	}
	if len(starts) == 0 {
		return parseGitDiff(patch)
	}
	var files []screen.DiffFile
	for index, start := range starts {
		end := len(patch)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		block := patch[start:end]
		parsed := parseGitDiff(block)
		path := patchBlockPath(block)
		for _, file := range parsed {
			if path != "" {
				file.Path = path
			}
			files = append(files, file)
		}
	}
	return files
}

func patchBlockPath(block string) string {
	var oldPath string
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "rename to "):
			return decodeGitPatchPath(strings.TrimPrefix(line, "rename to "), "")
		case strings.HasPrefix(line, "+++ ") && !strings.HasPrefix(line, "+++ /dev/null"):
			return decodeGitPatchPath(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "--- ") && !strings.HasPrefix(line, "--- /dev/null"):
			oldPath = decodeGitPatchPath(strings.TrimPrefix(line, "--- "), "a/")
		}
	}
	return oldPath
}

func decodeGitPatchPath(raw, prefix string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		if decoded, err := strconv.Unquote(raw); err == nil {
			raw = decoded
		}
	}
	return strings.TrimPrefix(raw, prefix)
}

func truncateTurnSourcePrompt(prompt string) string {
	const max = 72
	runes := []rune(prompt)
	if len(runes) <= max {
		return prompt
	}
	return string(runes[:max-1]) + "…"
}

func diffSourceSubtitle(kind, prompt string) string {
	prompt = safeArchiveLabel(prompt)
	if prompt == "" {
		return kind
	}
	return truncateTurnSourcePrompt(kind + " · " + prompt)
}
