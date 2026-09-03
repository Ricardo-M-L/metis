package webui

import (
	"net/http"
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
	for _, want := range []string{".trace-turn-header", ".trace-inspector", ".trace-timeline", ".chat-area", ".session-delete-dialog", ".k-thinking", ".think-leading", ".think-head:focus-visible", ".composer-add-menu", ".composer-action-dialog", ".sb-update", ".update-dialog", ".msg-metrics", ".plugin-ecosystem-grid", ".plugin-ecosystem-card", ".plugin-component-row", "button.sb-settings {\n  width: auto;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}
	code, ctype, body = get("/artifacts.css")
	if code != 200 || !strings.Contains(ctype, "css") || !strings.Contains(body, ".artifact-preview-shell") || !strings.Contains(body, ".artifact-card") || !strings.Contains(body, ".artifact-preview-state[hidden]") {
		t.Fatalf("artifacts.css: code=%d type=%q", code, ctype)
	}

	// scripts, each carrying its own domain content
	scriptChecks := map[string][]string{
		"app.js":       {"escHtml", "escAttr", "DOMContentLoaded", "detectProject", "contextMeter", "initDesktopPreferences", "renderStatusPopover", "renderStatusSnapshot", "openRailSearch", "applyLanguage", "data-i18n-label", "data-i18n-title", "syncApprovalChip(approvalMode)", "requestNative", "checkDesktopUpdate", "openDesktopUpdateDialog", "installDesktopUpdate", "From idea to done", "从想法，到完成", "METIS Desktop"},
		"sessions.js":  {"loadSessions", "loadMoreSessions", "renderSessions", "resumeSession", "archiveSession", "restoreSession", "openSessionDeleteDialog", "confirmSessionDeletion", "closeSessionDeleteDialog", `role="alertdialog"`, "requestAnimationFrame(() => cancel.focus())", "method: 'DELETE'", "setSessionPreference", "workspaceLabel", "sessionItemKeydown", "loadWorkspaces", "addWorkspace", "requestNative('choose-workspace')", "openWorkspace", "renameWorkspace", "removeWorkspace", "moveWorkspace", "moveSession", "showSessionDetail", "Plan session completed"},
		"chat.js":      {"connectEvents", "acceptLiveEvent", "sendMessage", "handleTextDelta", "showReconnectBanner", "endStreamingMessage", "openAttachmentPicker", "initAttachmentDrop", "pasteClipboardFilePaths", "pasteAllClipboardFilePaths", "/api/clipboard/files", "selectionStart", "COMPOSER_COMMANDS", "COMPOSER_ADD_ACTIONS", "toggleComposerAddMenu", "getBoundingClientRect().top - 12", "runComposerAddAction", "openComposerActionDialog", "/api/compact", "/api/goals", "/api/feedback", "submitBusyInput", "drainQueuedTurns", "filterSettings", "loadProviders", "saveCustomProvider", "deleteProvider", "validateProvider", "probeProvider", "loadEffort", "loadPresets", "loadPlugins", "loadPluginCatalog", "refreshPluginCatalog", "installPlugin", "removePlugin", "openPluginActionDialog", "pluginEcosystemGrid", "choosePluginEcosystem", "renderPluginEcosystems", "Ecosystem compatibility", "生态兼容层", "/api/plugins/catalog", "/api/plugins/install", "/api/plugins/remove", "loadRouting", "chooseLanguage", "ROUTING_ZH", "appendThinkingRow", "thinkRowKeydown", "REDACTED_THINKING_PLACEHOLDER", "MESSAGE_ACTION_ICONS", "messageActionsMarkup", "restoreHistoryMessageMetadata", "turnMetrics", "msg-metrics"},
		"trace.js":     {"loadTrace", "renderTrace", "switchView", "selectTraceRow", "renderTraceInspector", "toggleFoldTurns", "partialArgs", "mergeTraceEvents", "traceNextCursor", "closeTraceInspector(false)", "thinking_redacted", "k-thinking"},
		"artifacts.js": {"renderArtifactPresentation", "loadArtifactsForSession", "openArtifactsPanel", "previewArtifactByID", "safeArtifactURL", "confirmArtifactDeletion", "/api/artifacts"},
	}
	_, _, chatBody := get("/chat.js")
	if strings.Contains(chatBody, "DeepSeek Harness extensions are npm/Cordis packages, not a public METIS-compatible marketplace") {
		t.Fatal("plugin settings still reduces the DeepSeek ecosystem to the retired no-marketplace notice")
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
	if strings.Contains(sessionsScript, ">Open in new window</button>") {
		t.Fatal("workspace context menu still contains the retired Open in new window action")
	}
	for _, want := range []string{"removedWorkspaceIDs", "data.removedIds", ">Remove from list</button>"} {
		if !strings.Contains(sessionsScript, want) {
			t.Fatalf("sessions.js missing workspace removal behavior %q", want)
		}
	}
	if strings.Contains(sessionsScript, `removeWorkspace('${escOnclick(ws.id)}')"${active ? ' disabled' : ''}`) {
		t.Fatal("active workspace removal is still disabled")
	}
	_, _, chatScript := get("/chat.js")
	for _, want := range []string{"MESSAGE_TIME_ZONE", "resolvedOptions().timeZone", "year: 'numeric'", "UTC${sign}"} {
		if !strings.Contains(chatScript, want) {
			t.Fatalf("chat.js missing full timestamp/timezone behavior %q", want)
		}
	}

	// index.html: markup stays, logic is external.
	code, _, body = get("/")
	if code != 200 {
		t.Fatalf("index: code=%d", code)
	}
	for _, want := range []string{
		`href="/favicon.svg"`,
		`<link rel="stylesheet" href="style.css?v=`,
		`<link rel="stylesheet" href="artifacts.css?v=`,
		`<script src="app.js?v=`,
		`<script src="sessions.js?v=`,
		`<script src="chat.js?v=`,
		`<script src="artifacts.js?v=`,
		`<script src="trace.js?v=`,
		"viewTabs", "tabTrace", "tabArtifacts", "artifactsPanel", "artifactPreviewFrame", "tracePanel", "traceSearch",
		"btnFoldTurns", "btnFoldCalls", "traceTimeline", "traceInspector",
		"archivedSessionsBtn", "sessionViewBtn", "sessionViewMenu", "attachmentInput", "contextMeter", "Session log",
		"statusPopover", "commandMenu", "composerAddMenu", "attachmentBtn", "queuedTurns", "details-closed", "workspaceAddBtn", "desktopUpdateBtn", "Model Providers", "Agent Presets", "Plugins", "Smart Routing", "effortBtn", "data-i18n", "data-i18n-label", "data-i18n-title",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	if strings.Contains(body, "<style>") || strings.Contains(body, "function loadTrace") {
		t.Fatal("index.html must not inline styles or logic anymore")
	}
	if !strings.Contains(body, `id="artifactPreviewFrame" title="Artifact preview" sandbox referrerpolicy="no-referrer"`) ||
		strings.Contains(body, "allow-scripts") || strings.Contains(body, "allow-same-origin") {
		t.Fatal("artifact preview iframe must keep an empty sandbox capability set")
	}
	if strings.Contains(body, "&#25506;&#32034;&#26410;&#33267;&#20043;&#22659;") || strings.Contains(body, "&#39044;&#35272;&#29256;") {
		t.Fatal("index.html still contains the retired DeepSeek-style welcome copy")
	}
}

func TestDesktopChromeOmitsRetiredBrandAndSessionMetadata(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	index := get("/")
	for _, retired := range []string{`class="welcome-badge"`, `id="topbarTitle"`, `id="presetName"`} {
		if strings.Contains(index, retired) {
			t.Fatalf("Desktop chrome still renders retired element %q", retired)
		}
	}
	if !strings.Contains(index, `class="topbar-btn session-log-btn"`) {
		t.Fatal("removing session metadata also removed the session-log action")
	}

	for _, asset := range []string{"app.js", "sessions.js", "chat.js", "style.css"} {
		body := get("/" + asset)
		for _, retired := range []string{"welcome-badge", "topbarTitle", "presetName"} {
			if strings.Contains(body, retired) {
				t.Fatalf("%s still references retired Desktop chrome %q", asset, retired)
			}
		}
	}
}

func TestContextMeterDistinguishesSmallAndInactiveContexts(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /app.js = %d", rr.Code)
	}
	app := rr.Body.String()
	for _, want := range []string{
		"const viewingNoSession = !selectedSessionId;",
		"if (viewingNoSession)",
		"const viewingInactiveSession =",
		"meter.textContent = dict.context + ' —';",
		"used > 0 && fraction < 0.01",
		"'<1%'",
	} {
		if !strings.Contains(app, want) {
			t.Fatalf("app.js missing context-meter behavior %q", want)
		}
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
		"/artifact", "/artifacts",
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

func TestUserMessageActionsAndConversationSpacingFollowTurnHierarchy(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	style := get("/style.css")
	lastRule := func(selector string) string {
		start := strings.LastIndex(style, selector+" {")
		if start < 0 {
			t.Fatalf("style.css missing canonical %s rule", selector)
		}
		end := strings.Index(style[start:], "\n}")
		if end < 0 {
			t.Fatalf("cannot isolate canonical %s rule", selector)
		}
		return style[start : start+end]
	}

	userRule := lastRule(".message-user")
	for _, want := range []string{"flex-direction: column;", "align-items: flex-end;", "margin-bottom: 8px;"} {
		if !strings.Contains(userRule, want) {
			t.Fatalf("user messages do not keep their actions below the bubble or close to the reply; missing %q in %q", want, userRule)
		}
	}
	assistantRule := lastRule(".message-assistant")
	if !strings.Contains(assistantRule, "margin-bottom: 32px;") {
		t.Fatalf("assistant replies do not separate completed turns; rule=%q", assistantRule)
	}

	chat := get("/chat.js")
	userStart := strings.Index(chat, "if (role === 'user') {")
	assistantStart := strings.Index(chat[userStart:], "} else {")
	if userStart < 0 || assistantStart < 0 {
		t.Fatal("cannot isolate user message markup")
	}
	userMarkup := chat[userStart : userStart+assistantStart]
	if strings.Index(userMarkup, "message-bubble") >= strings.Index(userMarkup, "messageActionsMarkup('user'") {
		t.Fatal("user message actions must be emitted after the bubble")
	}
}

func TestComposerSuppressesRootHorizontalScrollAndExplainsPermissionModes(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	style := get("/style.css")
	chatAreaStart := strings.Index(style, ".chat-area {\n")
	if chatAreaStart < 0 {
		t.Fatal("style.css missing .chat-area rule")
	}
	chatAreaEnd := strings.Index(style[chatAreaStart:], "\n}")
	if chatAreaEnd < 0 {
		t.Fatal("cannot isolate .chat-area rule")
	}
	chatAreaRule := style[chatAreaStart : chatAreaStart+chatAreaEnd]
	if !strings.Contains(chatAreaRule, "overflow-x: hidden;") {
		t.Fatal("chat transcript can expose a horizontal scrollbar above the composer")
	}

	chat := get("/chat.js")
	for _, want := range []string{
		"dontAsk: 'Never prompt: allow pre-approved and read-only work, deny the rest'",
		"bypassPermissions: 'Auto-approve ordinary tool calls; critical destructive checks remain'",
		"fullAccess: 'Run without approval prompts or process sandboxing; unrestricted file and network access'",
		"dontAsk: '从不弹窗：只执行已允许和只读操作，其余直接拒绝'",
		"bypassPermissions: '普通工具调用自动执行，严重破坏性操作仍会拦截'",
		"fullAccess: '不弹出审批且关闭进程沙箱，可访问任意文件和网络'",
		"if (mode === 'fullAccess' && approvalMode !== 'fullAccess')",
		"confirmFullAccessMode(trigger)",
		"role=\"alertdialog\"",
		"Full Access is saved as the default permission mode until you change it.",
		"\\u5b8c\\u5168\\u8bbf\\u95ee\\u4f1a\\u4fdd\\u5b58\\u4e3a\\u9ed8\\u8ba4\\u6743\\u9650\\u6a21\\u5f0f",
		"if (key === 'permission.mode')",
		"await setPermissionMode(value, el)",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing accurate permission explanation %q", want)
		}
	}
	if strings.Contains(chat, "window.confirm") {
		t.Fatal("permission mode changes still rely on window.confirm, which is unreliable in the embedded WebView")
	}
	for _, want := range []string{".full-access-confirm-warning {", ".composer-action-confirm.danger"} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing in-app Full Access confirmation styling %q", want)
		}
	}
}

func TestComposerEnterRespectsIMEAndFocusHasNoOuterRing(t *testing.T) {
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
	handlerStart := strings.Index(chat, "function handleKeydown(e) {")
	if handlerStart < 0 {
		t.Fatal("chat.js missing handleKeydown")
	}
	handler := chat[handlerStart:]
	guard := strings.Index(handler, "if (e.isComposing || e.keyCode === 229) return;")
	menuHandling := strings.Index(handler, "const menuOpen =")
	if guard < 0 || menuHandling < 0 || guard > menuHandling {
		t.Fatal("handleKeydown must ignore Enter and navigation keys while an IME composition is active")
	}

	style := get("/style.css")
	if strings.Contains(style, ".input-box:focus-within") {
		t.Fatal("composer focus must not draw an outer border or ring")
	}
}

func TestResolvedPermissionCardUsesFinalStateAndRemovesActions(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chat.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /chat.js = %d", rr.Code)
	}

	chat := rr.Body.String()
	for _, want := range []string{
		`data-tool="${escAttr(d.tool || 'tool')}"`,
		`name.textContent = tool + (approve ? ' allowed' : ' denied');`,
		`actions.remove();`,
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("resolved permission card is missing final-state behavior %q", want)
		}
	}
}

func TestFileEditRowsRenderExpandableDiffForLiveAndHistory(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	chat := get("/chat.js")
	for _, want := range []string{
		"const FILE_DIFF_MAX_LINES",
		"function parseFileEditInput(name, input)",
		"function buildFileLineDiff(before, after)",
		"function renderFileEditCard(chip, name, input)",
		"args.old_string",
		"renderFileEditCard(chip, name, chip.getAttribute('data-args') || '')",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing file-edit diff behavior %q", want)
		}
	}

	style := get("/style.css")
	for _, want := range []string{
		".tc-file-diff {",
		".tc-diff-line[data-kind='add']",
		".tc-diff-line[data-kind='delete']",
		".tc-diff-code",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing file-edit diff style %q", want)
		}
	}
}

func TestCompactionPresentationUsesOnePersistentRow(t *testing.T) {
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
		"let compactionStatusEl = null;",
		"function upsertCompactionRow(",
		"upsertCompactionRow(uiText('Compacting context…', '正在压缩上下文…')",
		"compactionStatusEl.classList.add('complete');",
		"async function restoreCompactionHistory(sessionId = currentSessionId, shouldApply = () => true)",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing persistent compaction presentation %q", want)
		}
	}

	sessions := get("/sessions.js")
	if !strings.Contains(sessions, "await restoreCompactionHistory(id, isLatest);") {
		t.Fatal("resuming a saved session does not restore its compaction disclosure rows")
	}

	trace := get("/trace.js")
	for _, want := range []string{
		"compaction_progress:",
		"function coalesceTraceCompactions(events)",
		"const displayEvents = coalesceTraceCompactions(events);",
		"/^context compacted\\b/i.test(String(ev.text || ''))",
	} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace.js missing compaction coalescing contract %q", want)
		}
	}
}

func TestSessionRenameUsesStyledDialogAndExpandControlIsTextOnly(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	sessions := get("/sessions.js")
	for _, want := range []string{
		"let sessionRenameDialog = null;",
		"function openSessionRenameDialog(",
		"async function submitSessionRename(",
		`class="session-rename-dialog" role="dialog"`,
		`class="session-rename-input"`,
		"input.select();",
		"/api/sessions/rename",
	} {
		if !strings.Contains(sessions, want) {
			t.Fatalf("sessions.js missing rename-dialog contract %q", want)
		}
	}
	if strings.Contains(sessions, "prompt('Rename session'") {
		t.Fatal("session rename still relies on the browser prompt instead of the Desktop dialog")
	}

	style := get("/style.css")
	for _, want := range []string{
		".session-rename-overlay {",
		".session-rename-dialog {",
		".session-rename-input {",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}
	expandStart := strings.LastIndex(style, ".session-expand {")
	if expandStart < 0 {
		t.Fatal("style.css missing canonical .session-expand rule")
	}
	expandEnd := strings.Index(style[expandStart:], "\n}")
	if expandEnd < 0 {
		t.Fatal("cannot isolate canonical .session-expand rule")
	}
	expandRule := style[expandStart : expandStart+expandEnd]
	for _, want := range []string{"border: 0;", "background: transparent;", "box-shadow: none;"} {
		if !strings.Contains(expandRule, want) {
			t.Fatalf("session expand/collapse control is not text-only at line %d; missing %q in %q", strings.Count(style[:expandStart], "\n")+1, want, expandRule)
		}
	}
}

func TestWorkspaceActionsUseStyledDialogsAndResumeRefreshesStatus(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	sessions := get("/sessions.js")
	for _, want := range []string{
		"let workspaceRenameDialog = null;",
		"let workspaceRemoveDialog = null;",
		"function openWorkspaceRenameDialog(",
		"async function submitWorkspaceRename(",
		"function openWorkspaceRemoveDialog(",
		"async function submitWorkspaceRemoval(",
		`class="workspace-dialog workspace-rename-dialog" role="dialog"`,
		`class="workspace-dialog workspace-remove-dialog" role="alertdialog"`,
		`workspace-rename-error" role="alert"`,
		`workspace-remove-error" role="alert"`,
		"await pollStatus(isLatest);",
	} {
		if !strings.Contains(sessions, want) {
			t.Fatalf("sessions.js missing workspace-dialog/status-refresh contract %q", want)
		}
	}
	if strings.Contains(sessions, "prompt('Rename workspace'") {
		t.Fatal("workspace rename still relies on window.prompt, which is unavailable in the embedded WebView")
	}
	if strings.Contains(sessions, "confirm('Remove \"") {
		t.Fatal("workspace removal still relies on window.confirm, which is unavailable in the embedded WebView")
	}

	style := get("/style.css")
	for _, want := range []string{
		".workspace-dialog-overlay {",
		".workspace-dialog {",
		".workspace-dialog-input {",
		".workspace-remove-confirm {",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing workspace dialog style %q", want)
		}
	}
}

func TestSessionResumeIgnoresStaleAsyncResponses(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /sessions.js = %d", rr.Code)
	}
	sessions := rr.Body.String()
	for _, want := range []string{
		"let resumeSessionGeneration = 0;",
		"function invalidateSessionAsyncLoads()",
		"const generation = ++resumeSessionGeneration;",
		"const isLatest = () => generation === resumeSessionGeneration;",
		"if (!isLatest()) return;",
		"loadTrace(false, id, isLatest);",
		"await restoreCompactionHistory(id, isLatest);",
		"await loadEffort(isLatest);",
		"await pollStatus(isLatest);",
		"await loadArtifactsForSession(id, { rebuildCards: true });",
		"bar.removeAttribute('aria-label');",
		"if (isLatest()) showError('Unable to resume this session.');",
	} {
		if !strings.Contains(sessions, want) {
			t.Fatalf("sessions.js missing stale-resume guard %q", want)
		}
	}
	if got := strings.Count(sessions, "if (!isLatest()) return;"); got < 7 {
		t.Fatalf("resumeSession has %d stale-response guards, want at least 7 async boundaries", got)
	}
	for asset, want := range map[string]string{
		"/app.js":   "generation !== statusRequestGeneration || !shouldApply()",
		"/chat.js":  "if (!shouldApply() || requestedSessionId !== String(currentSessionId || '')) return;",
		"/trace.js": "async function loadTrace(loadOlder = false, sessionId = currentSessionId, shouldApply = () => true)",
	} {
		rr = httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, asset, nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("%s missing guarded session async helper %q", asset, want)
		}
	}
}

func TestRunningTurnCanBeViewedInBackgroundAndStoppedBySession(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	chat := get("/chat.js")
	for _, want := range []string{
		"let runningSessionId = null;",
		"function detachRunningTurnView()",
		"body: JSON.stringify({ sessionId: runningSessionId })",
		"const turnSessionId = currentSessionId;",
		"currentSessionId === turnSessionId",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing background-turn contract %q", want)
		}
	}

	sessions := get("/sessions.js")
	for _, want := range []string{
		"let viewOnly = typeof turnRunning !== 'undefined' && turnRunning;",
		"'/api/sessions/' + encodeURIComponent(id)",
		"detachRunningTurnView();",
		"s.id === runningSessionId",
		"if (!viewOnly && res.status === 409)",
		"conflict.runningSessionId",
	} {
		if !strings.Contains(sessions, want) {
			t.Fatalf("sessions.js missing background-view contract %q", want)
		}
	}
	if strings.Contains(sessions, "Stop the current turn before switching sessions") {
		t.Fatal("session navigation still blocks while another turn is running")
	}
	app := get("/app.js")
	for _, want := range []string{
		"Math.min(100, Math.round(fraction * 100))",
		"const statusRunning = !!d.turnRunning",
		"setTurnRunning(statusRunning, statusSession)",
	} {
		if !strings.Contains(app, want) {
			t.Fatalf("app.js missing authoritative context/turn status contract %q", want)
		}
	}
	chat = get("/chat.js")
	for _, want := range []string{
		"function visibleTranscriptText(value)",
		"INTERNAL_TRANSCRIPT_SECTION_RE",
		"formatContent(visibleTranscriptText(streamingText))",
		"Math.min(100, Math.round(used / limit * 100))",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing internal-message redaction contract %q", want)
		}
	}
	index := get("/index.html")
	for _, want := range []string{"M8 2.25v7.2", "m5.35 7.15 2.65 2.7 2.65-2.7"} {
		if !strings.Contains(index, want) {
			t.Fatalf("update button is missing Codex-style download glyph path %q", want)
		}
	}
}

func TestChatAutoScrollYieldsWhenUserReadsEarlierOutput(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	chat := get("/chat.js")
	for _, want := range []string{
		"let followOutput = true;",
		"function isChatNearBottom(area)",
		"area.addEventListener('scroll', onChatScroll, { passive: true });",
		"area.addEventListener('wheel', pauseAutoScrollOnWheel, { passive: true });",
		"if (!followOutput)",
		"function resumeAutoScroll()",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing sticky-bottom contract %q", want)
		}
	}
	if got := strings.Count(chat, "area.scrollTop = area.scrollHeight;"); got != 1 {
		t.Fatalf("chat.js has %d unconditional bottom assignments; want only the guarded autoScroll assignment", got)
	}

	index := get("/")
	for _, want := range []string{"id=\"jumpLatestBtn\"", "onclick=\"resumeAutoScroll()\"", "data-i18n-label=\"jumpLatest\""} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html missing jump-to-latest contract %q", want)
		}
	}

	app := get("/app.js")
	for _, want := range []string{"initChatScroll();", "jumpLatest: 'Jump to latest'", "jumpLatest: '回到最新'"} {
		if !strings.Contains(app, want) {
			t.Fatalf("app.js missing scroll-follow integration %q", want)
		}
	}

	style := get("/style.css")
	for _, want := range []string{".jump-latest {", ".jump-latest[hidden] { display: none; }", "scroll-behavior: auto;"} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing scroll-follow presentation %q", want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
