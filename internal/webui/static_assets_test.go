package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The frontend is split into static assets served from the embedded FS:
// index.html references an external stylesheet and four ordered scripts.
// Each must be reachable over HTTP and carry its own content.
func TestStaticAssetsServed(t *testing.T) {
	s, _ := testServer(t)

	get := func(path string) (int, string, string) {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr.Code, rr.Header().Get("Content-Type"), rr.Body.String()
	}

	// favicon
	code, ctype, body := get("/favicon.svg")
	if code != 200 || !strings.Contains(ctype, "svg") || !strings.Contains(body, "<svg") {
		t.Fatalf("favicon: code=%d type=%q body=%q", code, ctype, body[:min(40, len(body))])
	}

	// stylesheet
	code, ctype, body = get("/style.css")
	if code != 200 || !strings.Contains(ctype, "css") {
		t.Fatalf("style.css: code=%d type=%q", code, ctype)
	}
	for _, want := range []string{".trace-turn-header", ".trace-inspector", ".trace-timeline", ".chat-area", ".session-delete-dialog", ".k-thinking", ".think-leading", ".think-head:focus-visible", ".composer-add-menu", ".composer-action-dialog"} {
		if !strings.Contains(body, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}

	// scripts, each carrying its own domain content
	scriptChecks := map[string][]string{
		"app.js":      {"escHtml", "escAttr", "DOMContentLoaded", "detectProject", "contextMeter", "initDesktopPreferences", "renderStatusPopover", "renderStatusSnapshot", "openRailSearch", "applyLanguage", "data-i18n-label", "data-i18n-title", "syncApprovalChip(approvalMode)", "From idea to done", "从想法，到完成", "METIS Desktop"},
		"sessions.js": {"loadSessions", "loadMoreSessions", "renderSessions", "resumeSession", "archiveSession", "restoreSession", "openSessionDeleteDialog", "confirmSessionDeletion", "closeSessionDeleteDialog", `role="alertdialog"`, "requestAnimationFrame(() => cancel.focus())", "method: 'DELETE'", "setSessionPreference", "workspaceLabel", "sessionItemKeydown", "loadWorkspaces", "addWorkspace", "openWorkspace", "renameWorkspace", "removeWorkspace", "moveWorkspace", "moveSession", "showSessionDetail", "Plan session completed"},
		"chat.js":     {"connectEvents", "acceptLiveEvent", "sendMessage", "handleTextDelta", "showReconnectBanner", "endStreamingMessage", "openAttachmentPicker", "initAttachmentDrop", "pasteClipboardFilePaths", "pasteAllClipboardFilePaths", "/api/clipboard/files", "selectionStart", "COMPOSER_COMMANDS", "COMPOSER_ADD_ACTIONS", "toggleComposerAddMenu", "getBoundingClientRect().top - 12", "runComposerAddAction", "openComposerActionDialog", "/api/compact", "/api/goals", "/api/feedback", "submitBusyInput", "drainQueuedTurns", "filterSettings", "loadProviders", "saveCustomProvider", "deleteProvider", "validateProvider", "probeProvider", "loadEffort", "loadPresets", "loadPlugins", "loadPluginCatalog", "refreshPluginCatalog", "installPlugin", "removePlugin", "openPluginActionDialog", "/api/plugins/catalog", "/api/plugins/install", "/api/plugins/remove", "loadRouting", "chooseLanguage", "ROUTING_ZH", "appendThinkingRow", "thinkRowKeydown", "REDACTED_THINKING_PLACEHOLDER"},
		"trace.js":    {"loadTrace", "renderTrace", "switchView", "selectTraceRow", "renderTraceInspector", "toggleFoldTurns", "partialArgs", "mergeTraceEvents", "traceNextCursor", "closeTraceInspector(false)", "thinking_redacted", "k-thinking"},
	}
	for file, wants := range scriptChecks {
		code, ctype, body := get("/" + file)
		if code != 200 {
			t.Fatalf("%s: code=%d", file, code)
		}
		if !strings.Contains(ctype, "javascript") && !strings.Contains(ctype, "text/plain") {
			t.Fatalf("%s: unexpected content type %q", file, ctype)
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", file, want)
			}
		}
	}
	_, _, sessionsScript := get("/sessions.js")
	deleteMenuCall := "openSessionDeleteDialog('${escOnclick(s.id)}','',this)"
	if got := strings.Count(sessionsScript, deleteMenuCall); got != 2 {
		t.Fatalf("sessions.js delete menu entries = %d, want active + archived", got)
	}

	// index.html: markup stays, logic is external.
	code, _, body = get("/")
	if code != 200 {
		t.Fatalf("index: code=%d", code)
	}
	for _, want := range []string{
		`href="/favicon.svg"`,
		`<link rel="stylesheet" href="style.css?v=`,
		`<script src="app.js?v=`,
		`<script src="sessions.js?v=`,
		`<script src="chat.js?v=`,
		`<script src="trace.js?v=`,
		"viewTabs", "tabTrace", "tracePanel", "traceSearch",
		"btnFoldTurns", "btnFoldCalls", "traceTimeline", "traceInspector",
		"archivedSessionsBtn", "sessionViewBtn", "sessionViewMenu", "attachmentInput", "contextMeter", "Session log",
		"statusPopover", "commandMenu", "composerAddMenu", "attachmentBtn", "queuedTurns", "details-closed", "workspaceAddBtn", "Model Providers", "Agent Presets", "Plugins", "Smart Routing", "effortBtn", "presetName", "data-i18n", "data-i18n-label", "data-i18n-title",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	if strings.Contains(body, "<style>") || strings.Contains(body, "function loadTrace") {
		t.Fatal("index.html must not inline styles or logic anymore")
	}
	if strings.Contains(body, "&#25506;&#32034;&#26410;&#33267;&#20043;&#22659;") || strings.Contains(body, "&#39044;&#35272;&#29256;") {
		t.Fatal("index.html still contains the retired DeepSeek-style welcome copy")
	}
}

func TestComposerAddMenuPreservesAttachmentAndKeepsSlashCommandsIndependent(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}
	index := get("/")
	if strings.Contains(index, `id="attachmentBtn" type="button" title="Attach images" aria-label="Attach images" onclick="openAttachmentPicker()"`) {
		t.Fatal("plus button still bypasses the add menu and opens only the image picker")
	}
	for _, want := range []string{`id="composerAddMenu"`, `id="attachmentInput"`, `aria-controls="composerAddMenu"`} {
		if !strings.Contains(index, want) {
			t.Fatalf("index missing separated composer control %q", want)
		}
	}
	chat := get("/chat.js")
	for _, command := range []string{
		"/compact", "/export", "/feedback", "/goal", "/permission", "/plan", "/model", "/skills", "/plugins",
		"/clear-history", "/retry", "/undo", "/save", "/thinking", "/theme", "/cost", "/tools", "/doctor",
	} {
		if !strings.Contains(chat, `{ name: '`+command+`'`) {
			t.Fatalf("slash catalog missing %s", command)
		}
	}
	if !strings.Contains(chat, "case 'attachment': openAttachmentPicker();") {
		t.Fatal("add menu no longer routes its attachment item through the original picker")
	}
	if got := strings.Count(chat, "{ name: '/"); got < 55 {
		t.Fatalf("Desktop command catalog has only %d commands, want at least 55 real mapped commands", got)
	}
	for _, want := range []string{
		`escHtml(c.name.slice(1))`,
		`input.value = name + ' ';`,
		`commandMatches = matches;`,
		`saveSettingValue('ui.thinking_display', value)`,
		`const PLUGIN_CATALOG_PAGE_SIZE = 60;`,
		`pluginCatalogCache.needsSync && !pluginCatalogSyncAttempted`,
		`showExportResult(data.path)`,
		`fetch('/api/exports/open'`,
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("Desktop interaction regression guard missing %q", want)
		}
	}
	if strings.Contains(chat, "commandMatches = matches.slice(") {
		t.Fatal("command palette still truncates matches before the scroll container can expose them")
	}
	style := get("/style.css")
	menuStart := strings.LastIndex(style, ".command-menu {")
	if menuStart < 0 {
		t.Fatal("style.css missing command menu")
	}
	menuEnd := strings.Index(style[menuStart:], "\n}")
	if menuEnd < 0 {
		t.Fatal("cannot isolate command menu CSS")
	}
	menuRule := style[menuStart : menuStart+menuEnd]
	if !strings.Contains(menuRule, "max-height:") || !strings.Contains(menuRule, "overflow-y: auto;") {
		t.Fatal("command catalog must use a bounded internal scroll area")
	}
	chooseStart := strings.Index(chat, "function chooseComposerCommand(name)")
	chooseEnd := -1
	if chooseStart >= 0 {
		chooseEnd = strings.Index(chat[chooseStart:], "\n}\n\nasync function executeComposerCommand")
	}
	if chooseStart < 0 || chooseEnd < 0 {
		t.Fatal("cannot locate command-selection implementation")
	}
	if strings.Contains(chat[chooseStart:chooseStart+chooseEnd], "executeComposerCommand") {
		t.Fatal("choosing a command still executes immediately instead of inserting it into the composer")
	}
	addStart := strings.Index(chat, "const COMPOSER_ADD_ACTIONS = [")
	addEnd := -1
	if addStart >= 0 {
		addEnd = strings.Index(chat[addStart:], "];\nlet commandMatches")
	}
	if addStart < 0 || addEnd < 0 {
		t.Fatal("cannot locate compact composer add catalog")
	}
	if got := strings.Count(chat[addStart:addStart+addEnd], " action: '"); got != 6 {
		t.Fatalf("composer add menu has %d actions, want six focused entries", got)
	}
}

func TestTurnStatusMatchesWholeAgentTurnLifecycle(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	chat := get("/chat.js")
	for _, want := range []string{
		`<div class="turn-status" role="status" aria-live="polite">Deep diving...<span class="ts-clock"></span></div>`,
		`onLive('turn_end', endStreamingMessage);`,
		`onLive('loop_done', finishUserTurn);`,
		`function finishUserTurn()`,
		`area.appendChild(turnStatusEl);`,
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing turn-status lifecycle contract %q", want)
		}
	}
	if strings.Contains(chat, `class="ts-pill"`) {
		t.Fatal("chat.js still wraps TurnStatus in the retired grey pill")
	}

	streamEnd := strings.Index(chat, "function endStreamingMessage()")
	turnEnd := strings.Index(chat, "function finishUserTurn()")
	if streamEnd < 0 || turnEnd <= streamEnd {
		t.Fatal("could not isolate assistant-message and whole-turn completion functions")
	}
	assistantCompletion := chat[streamEnd:turnEnd]
	if strings.Contains(assistantCompletion, "endTurnStatus()") || strings.Contains(assistantCompletion, "showTurnStatsLine()") {
		t.Fatal("assistant turn_end still removes the whole-agent TurnStatus")
	}
	statusStart := strings.Index(chat, "function beginTurnStatus()")
	statusEnd := strings.Index(chat, "function endTurnStatus()")
	if statusStart < 0 || statusEnd <= statusStart || !strings.Contains(chat[statusStart:statusEnd], "autoScroll()") {
		t.Fatal("beginTurnStatus does not reveal the running status at the transcript tail")
	}

	style := get("/style.css")
	if strings.Contains(style, ".turn-status .ts-pill") {
		t.Fatal("style.css still contains the retired grey TurnStatus pill")
	}
}

func TestExpandedThinkingRowCannotShrinkIntoFollowingMessage(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/style.css", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /style.css = %d", rr.Code)
	}
	style := rr.Body.String()
	rowStart := strings.LastIndex(style, ".think-row {")
	if rowStart < 0 {
		t.Fatal("style.css missing canonical .think-row rule")
	}
	headOffset := strings.Index(style[rowStart:], ".think-head {")
	if headOffset < 0 {
		t.Fatal("style.css missing .think-head after canonical .think-row rule")
	}
	rowRule := style[rowStart : rowStart+headOffset]
	if !strings.Contains(rowRule, "flex: 0 0 auto;") {
		t.Fatal("canonical .think-row can shrink inside the flex scroll container and overlap the next message")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
