package tui

// think_back.go implements Metis's local, deterministic counterpart to
// Claude Code's `/think-back` year-in-review. It intentionally works from the
// typed session store: no network request, model call, auth file, config
// credential, tool input, tool result, prompt excerpt, session id, or working
// directory is ever rendered back to the transcript.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const maxThinkBackThemeBytes = 32 << 10

type thinkBackReport struct {
	year int

	sessions         int
	topLevelSessions int
	subAgentSessions int
	branches         int
	projects         map[string]struct{}

	messages          int
	userPrompts       int
	assistantMessages int
	toolCalls         int
	toolErrors        int
	thinkingBlocks    int

	modelMix map[string]int
	toolMix  map[string]int
	themeMix map[string]int
	months   [12]int
	days     map[string]time.Time

	firstStart time.Time
	lastStart  time.Time

	deepestMessages int
	deepestTools    int
	skippedSessions int
}

type thinkBackTheme struct {
	label    string
	keywords []string
	tools    []string
}

type thinkBackSessionFile struct {
	store *session.Store
	entry os.DirEntry
}

// Themes are deliberately categorical. We inspect user-authored text only to
// increment these fixed buckets; the source text itself never leaves the
// collector. Tool-name fallback is used only when a session has no textual
// match, keeping ubiquitous Read/Grep calls from drowning out user intent.
var thinkBackThemes = []thinkBackTheme{
	{
		label: "Building & feature work",
		keywords: []string{
			"implement", "build", "create", "add feature", "new feature", "develop", "scaffold",
			"实现", "新增", "创建", "开发", "搭建",
		},
		tools: []string{"edit", "write", "notebookedit", "applypatch"},
	},
	{
		label: "Debugging & reliability",
		keywords: []string{
			"debug", "fix", "bug", "error", "failure", "failing", "test", "diagnose", "regression", "root cause", "troubleshoot",
			"修复", "排查", "错误", "失败", "测试", "故障", "根因",
		},
	},
	{
		label: "Research & analysis",
		keywords: []string{
			"analyze", "analysis", "investigate", "compare", "research", "inspect", "understand", "explain", "audit",
			"分析", "调研", "对比", "检查", "看看", "调查", "审计", "解释",
		},
		tools: []string{"read", "grep", "glob", "webfetch", "websearch"},
	},
	{
		label: "Refactoring & quality",
		keywords: []string{
			"refactor", "optimize", "performance", "cleanup", "clean up", "simplify", "quality", "best practice",
			"重构", "优化", "性能", "清理", "质量", "最佳实践",
		},
	},
	{
		label: "Documentation & learning",
		keywords: []string{
			"documentation", "docs", "readme", "guide", "learn", "translate", "summary", "tutorial",
			"文档", "说明", "学习", "翻译", "总结", "教程",
		},
	},
	{
		label: "Release & operations",
		keywords: []string{
			"deploy", "release", "docker", "kubernetes", "helm", "pipeline", "install", "upgrade", "configuration", "configure",
			"发布", "部署", "配置", "安装", "升级", "运维",
		},
	},
	{
		label: "Version control & review",
		keywords: []string{
			"git", "commit", "pull request", "code review", "merge", "branch", "review pr",
			"代码审查", "提交", "合并", "分支",
		},
		tools: []string{"git"},
	},
	{
		label: "Delegation & collaboration",
		keywords: []string{
			"subagent", "sub-agent", "agent team", "teammate", "parallel", "collaborate", "delegate",
			"子代理", "子智能体", "团队", "协作", "并行", "委派",
		},
		tools: []string{"agent", "fork", "messageteammate"},
	},
}

func newThinkBackReport(year int) thinkBackReport {
	return thinkBackReport{
		year:     year,
		projects: make(map[string]struct{}),
		modelMix: make(map[string]int),
		toolMix:  make(map[string]int),
		themeMix: make(map[string]int),
		days:     make(map[string]time.Time),
	}
}

// collectThinkBack attributes a session to the natural year in which its
// header says it was created. Messages currently have no timestamps, so using
// file mtime would incorrectly move an entire old transcript into this year
// after one resumed turn. A legacy zero CreatedAt falls back to file mtime.
//
// Branch JSONL files contain a copy of their parent's history. The inherited
// prefix recorded in ForkedFrom.MessageCount is removed before aggregation so
// /branch does not double-count the same work.
func collectThinkBack(store *session.Store, now time.Time) (thinkBackReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	report := newThinkBackReport(now.Year())
	if store == nil || strings.TrimSpace(store.Dir) == "" {
		return report, fmt.Errorf("session store unavailable")
	}

	files, err := listThinkBackSessionFiles(store)
	if err != nil {
		return report, err
	}
	loc := now.Location()
	yearStart := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, loc)
	yearEnd := yearStart.AddDate(1, 0, 0)

	for _, file := range files {
		entry := file.entry
		// Timing sidecars share the .jsonl extension but contain no session
		// header. They are healthy auxiliary data, not unreadable sessions.
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" || strings.HasSuffix(entry.Name(), ".timing.jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		header, messages, loadErr := file.store.Load(id)
		if loadErr != nil || header == nil {
			report.skippedSessions++
			continue
		}

		started := header.CreatedAt
		if started.IsZero() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				report.skippedSessions++
				continue
			}
			started = info.ModTime()
		}
		started = started.In(loc)
		if started.Before(yearStart) || !started.Before(yearEnd) || started.After(now) {
			continue
		}

		inherited := 0
		if header.ForkedFrom != nil {
			report.branches++
			inherited = thinkBackInheritedPrefix(
				filepath.Join(file.store.Dir, filepath.Base(entry.Name())),
				messages,
				header.ForkedFrom.MessageCount,
			)
		}
		messages = messages[inherited:]

		report.sessions++
		if header.SubAgentOf == "" {
			report.topLevelSessions++
		} else {
			report.subAgentSessions++
		}
		if workDir := strings.TrimSpace(header.WorkDir); workDir != "" {
			report.projects[filepath.Clean(workDir)] = struct{}{}
		}
		if model := strings.TrimSpace(header.Model); model != "" {
			report.modelMix[model]++
		}

		report.months[int(started.Month())-1]++
		dayKey := started.Format("2006-01-02")
		report.days[dayKey] = time.Date(started.Year(), started.Month(), started.Day(), 0, 0, 0, 0, loc)
		if report.firstStart.IsZero() || started.Before(report.firstStart) {
			report.firstStart = started
		}
		if report.lastStart.IsZero() || started.After(report.lastStart) {
			report.lastStart = started
		}

		sessionTools := make(map[string]struct{})
		sessionThemes := make(map[string]struct{})
		sessionToolCalls := 0
		for _, message := range messages {
			report.messages++
			if message.Role == llm.RoleAssistant {
				report.assistantMessages++
			}
			if message.Role == llm.RoleUser && isThinkBackUserPrompt(message) {
				report.userPrompts++
				for _, block := range message.Content {
					if block.Type == "text" {
						classifyThinkBackText(block.Text, sessionThemes)
					}
				}
			}
			for _, block := range message.Content {
				switch strings.ToLower(block.Type) {
				case "thinking", "thought", "reasoning":
					if strings.TrimSpace(block.Text) != "" {
						report.thinkingBlocks++
					}
				case "tool_use":
					report.toolCalls++
					sessionToolCalls++
					tool := strings.TrimSpace(block.ToolName)
					if tool != "" {
						report.toolMix[tool]++
						sessionTools[normalizeThinkBackTool(tool)] = struct{}{}
					}
				case "tool_result":
					if block.IsError {
						report.toolErrors++
					}
				}
			}
		}

		if len(sessionThemes) == 0 {
			classifyThinkBackTools(sessionTools, sessionThemes)
		}
		for theme := range sessionThemes {
			report.themeMix[theme]++
		}
		if len(messages) > report.deepestMessages ||
			(len(messages) == report.deepestMessages && sessionToolCalls > report.deepestTools) {
			report.deepestMessages = len(messages)
			report.deepestTools = sessionToolCalls
		}
	}
	return report, nil
}

func listThinkBackSessionFiles(store *session.Store) ([]thinkBackSessionFile, error) {
	dirs := []string{
		store.Dir,
		filepath.Join(store.Dir, agent.SubAgentTranscriptDirname),
	}
	var files []thinkBackSessionFile
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		dirStore := &session.Store{Dir: dir}
		for _, entry := range entries {
			files = append(files, thinkBackSessionFile{store: dirStore, entry: entry})
		}
	}
	return files, nil
}

// thinkBackInheritedPrefix returns inheritedCount only when the branch's
// current logical history still begins with the exact messages copied at fork
// time. Store.Load applies history_replace snapshots, so blindly slicing by
// ForkedFrom.MessageCount after /clear, /undo, or compaction can discard new
// branch work. The original prefix remains in the append-only JSONL before the
// first replacement and is used solely for an in-memory equality check.
func thinkBackInheritedPrefix(path string, logical []llm.Message, inheritedCount int) int {
	if inheritedCount <= 0 || len(logical) < inheritedCount {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	original := make([]llm.Message, 0, inheritedCount)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var entry session.Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			return 0
		}
		switch entry.Type {
		case "message":
			if entry.Message != nil {
				original = append(original, *entry.Message)
				if len(original) == inheritedCount {
					for i := range original {
						left, leftErr := json.Marshal(original[i])
						right, rightErr := json.Marshal(logical[i])
						if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
							return 0
						}
					}
					return inheritedCount
				}
			}
		case "history_replace":
			// A valid branch writes its complete inherited prefix before any
			// replacement. Stopping here avoids mistaking later messages for it.
			return 0
		}
	}
	return 0
}

func isThinkBackUserPrompt(message llm.Message) bool {
	if message.Role != llm.RoleUser {
		return false
	}
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return false
		}
	}
	for _, block := range message.Content {
		if block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		if text == "" || isSyntheticThinkBackText(text) {
			continue
		}
		return true
	}
	return false
}

func isSyntheticThinkBackText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, prefix := range []string{
		"<system-reminder", "<job_notification", "<sub_agent_", "<peer_message", "<monitor_event", "<todo_reminder",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func classifyThinkBackText(text string, hits map[string]struct{}) {
	if len(text) > maxThinkBackThemeBytes {
		text = text[:maxThinkBackThemeBytes]
	}
	lower := strings.ToLower(text)
	for _, theme := range thinkBackThemes {
		for _, keyword := range theme.keywords {
			if strings.Contains(lower, keyword) {
				hits[theme.label] = struct{}{}
				break
			}
		}
	}
}

func classifyThinkBackTools(tools map[string]struct{}, hits map[string]struct{}) {
	for _, theme := range thinkBackThemes {
		for _, tool := range theme.tools {
			if _, ok := tools[tool]; ok {
				hits[theme.label] = struct{}{}
				break
			}
		}
	}
}

func normalizeThinkBackTool(tool string) string {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if strings.HasPrefix(tool, "mcp__") {
		return "mcp"
	}
	tool = strings.ReplaceAll(tool, "_", "")
	tool = strings.ReplaceAll(tool, "-", "")
	return tool
}

func cmdThinkBack(r *REPL, args string) string {
	if strings.TrimSpace(args) != "" {
		return "usage: /think-back"
	}
	store := &session.Store{Dir: filepath.Join(config.Home(), "sessions")}
	if r != nil && r.Session != nil {
		store = r.Session
	}
	now := time.Now()
	report, err := collectThinkBack(store, now)
	if err != nil {
		return "think-back: " + err.Error()
	}
	return renderThinkBack(report, now)
}

// cmdThoughts preserves the former /thinkback behavior under an unambiguous
// name. `/think-back` now owns the year-in-review meaning; the old helper stays
// behind this wrapper so existing trace logic remains one source of truth.
func cmdThoughts(r *REPL, args string) string {
	if strings.TrimSpace(args) != "" {
		return "usage: /thoughts"
	}
	return cmdLastThinking(r, "")
}

func renderThinkBack(report thinkBackReport, now time.Time) string {
	if report.sessions == 0 {
		return fmt.Sprintf("(no sessions created in %d yet)", report.year)
	}
	if now.IsZero() {
		now = time.Now()
	}
	loc := now.Location()
	yearStart := time.Date(report.year, time.January, 1, 0, 0, 0, 0, loc)
	through := now.In(loc)
	if through.Year() != report.year {
		through = time.Date(report.year, time.December, 31, 0, 0, 0, 0, loc)
	}

	rows := []infoRow{
		{Key: "window", Value: yearStart.Format("Jan 2") + " - " + through.Format("Jan 2")},
		{Key: "sessions", Value: fmt.Sprintf("%d total · %d top-level · %d sub-agent · %d branches", report.sessions, report.topLevelSessions, report.subAgentSessions, report.branches)},
		{Key: "conversation", Value: fmt.Sprintf("%d user prompts · %d assistant messages · %d new messages", report.userPrompts, report.assistantMessages, report.messages)},
		{Key: "tool work", Value: thinkBackToolSummary(report)},
		{Key: "thinking", Value: fmt.Sprintf("%d captured reasoning block(s)", report.thinkingBlocks)},
		{Key: "breadth", Value: fmt.Sprintf("%d active day(s) · %d month(s) · %d project(s) · %d model(s)", len(report.days), thinkBackActiveMonths(report.months), len(report.projects), len(report.modelMix))},
	}

	if models := thinkBackRankedSummary(report.modelMix, 3); models != "" {
		rows = append(rows, infoRow{Key: "models", Value: models})
	}
	if tools := thinkBackRankedSummary(report.toolMix, 5); tools != "" {
		rows = append(rows, infoRow{Key: "top tools", Value: tools})
	}
	if themes := thinkBackThemeSummary(report.themeMix, 3); themes != "" {
		rows = append(rows, infoRow{Key: "themes", Value: themes})
	}

	rows = append(rows,
		infoRow{Key: "first start", Value: report.firstStart.In(loc).Format("Jan 2")},
		infoRow{Key: "busiest month", Value: thinkBackBusiestMonth(report.months)},
		infoRow{Key: "longest streak", Value: fmt.Sprintf("%d consecutive active day(s)", thinkBackLongestStreak(report.days))},
		infoRow{Key: "deepest session", Value: fmt.Sprintf("%d new messages · %d tool calls", report.deepestMessages, report.deepestTools)},
	)
	if report.skippedSessions > 0 {
		rows = append(rows, infoRow{Key: "note", Value: fmt.Sprintf("%d unreadable session file(s) skipped", report.skippedSessions)})
	}
	return renderInfoBox(fmt.Sprintf("Think Back · %d", report.year), rows)
}

func thinkBackToolSummary(report thinkBackReport) string {
	if report.toolCalls == 0 {
		return "no tool calls captured"
	}
	succeeded := report.toolCalls - report.toolErrors
	if succeeded < 0 {
		succeeded = 0
	}
	percent := float64(succeeded) / float64(report.toolCalls) * 100
	return fmt.Sprintf("%d calls · %d errors · %.0f%% successful", report.toolCalls, report.toolErrors, percent)
}

func thinkBackActiveMonths(months [12]int) int {
	n := 0
	for _, count := range months {
		if count > 0 {
			n++
		}
	}
	return n
}

func thinkBackBusiestMonth(months [12]int) string {
	bestMonth, bestCount := time.January, 0
	for i, count := range months {
		if count > bestCount {
			bestMonth = time.Month(i + 1)
			bestCount = count
		}
	}
	if bestCount == 0 {
		return "none"
	}
	return fmt.Sprintf("%s · %d session(s)", bestMonth.String(), bestCount)
}

func thinkBackLongestStreak(days map[string]time.Time) int {
	if len(days) == 0 {
		return 0
	}
	ordered := make([]time.Time, 0, len(days))
	for _, day := range days {
		ordered = append(ordered, day)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Before(ordered[j]) })
	best, current := 1, 1
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].AddDate(0, 0, 1).Equal(ordered[i]) {
			current++
			if current > best {
				best = current
			}
		} else {
			current = 1
		}
	}
	return best
}

type thinkBackCount struct {
	label string
	count int
	order int
}

func thinkBackRankedSummary(counts map[string]int, limit int) string {
	merged := make(map[string]int)
	for raw, count := range counts {
		if count <= 0 {
			continue
		}
		label := safeThinkBackLabel(raw)
		if label == "" {
			continue
		}
		merged[label] += count
	}
	items := make([]thinkBackCount, 0, len(merged))
	for label, count := range merged {
		items = append(items, thinkBackCount{label: label, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].label < items[j].label
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s × %d", item.label, item.count))
	}
	return strings.Join(parts, " · ")
}

func thinkBackThemeSummary(counts map[string]int, limit int) string {
	order := make(map[string]int, len(thinkBackThemes))
	for i, theme := range thinkBackThemes {
		order[theme.label] = i
	}
	items := make([]thinkBackCount, 0, len(counts))
	for label, count := range counts {
		if count > 0 {
			items = append(items, thinkBackCount{label: label, count: count, order: order[label]})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		if items[i].order != items[j].order {
			return items[i].order < items[j].order
		}
		return items[i].label < items[j].label
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s (%d sessions)", item.label, item.count))
	}
	return strings.Join(parts, " · ")
}

// safeThinkBackLabel is the only path by which a session-derived string may
// reach output. Known secret patterns are replaced wholesale rather than
// showing a partly-redacted model/tool name, controls are rejected, and the
// label is bounded so a malformed transcript cannot flood the review.
func safeThinkBackLabel(raw string) string {
	return safeArchiveLabel(raw)
}
