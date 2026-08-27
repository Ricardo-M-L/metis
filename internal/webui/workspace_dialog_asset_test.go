package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The web UI has no JavaScript DOM test runner. Keep the complete click-to-
// dialog wiring under one deterministic asset test so a future refactor cannot
// silently leave the menu item closing its menu without opening the embedded
// WebView-safe confirmation dialog.
func TestWorkspaceRemoveMenuWiresStyledDialogBeforeRemovalRequest(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /sessions.js = %d", rr.Code)
	}
	script := rr.Body.String()

	clickHandler := `onclick="event.stopPropagation();removeWorkspace('${escOnclick(ws.id)}')"`
	if !strings.Contains(script, clickHandler) {
		t.Fatalf("workspace remove menu item is not wired to the dialog entry point: missing %q", clickHandler)
	}

	removeStart := strings.Index(script, "function removeWorkspace(id) {")
	if removeStart < 0 {
		t.Fatal("cannot find removeWorkspace")
	}
	removeEnd := strings.Index(script[removeStart:], "\n}")
	if removeEnd < 0 {
		t.Fatal("cannot isolate removeWorkspace")
	}
	removeBody := script[removeStart : removeStart+removeEnd]
	if !strings.Contains(removeBody, "openWorkspaceRemoveDialog(id, ws.name || '', openWorkspaceMenuBtn);") {
		t.Fatal("removeWorkspace no longer opens the styled confirmation dialog")
	}

	dialogStart := strings.Index(script, "function openWorkspaceRemoveDialog(id, name, trigger) {")
	if dialogStart < 0 {
		t.Fatal("cannot find openWorkspaceRemoveDialog")
	}
	dialogEnd := strings.Index(script[dialogStart:], "\n}\n\nfunction removeWorkspace")
	if dialogEnd < 0 {
		t.Fatal("cannot isolate openWorkspaceRemoveDialog")
	}
	dialogBody := script[dialogStart : dialogStart+dialogEnd]
	for _, want := range []string{
		"closeWorkspaceMenu(false);",
		"document.createElement('div')",
		`role="alertdialog"`,
		"document.body.appendChild(overlay);",
		"workspaceRemoveDialog = state;",
		"confirm.addEventListener('click', () => submitWorkspaceRemoval(state));",
	} {
		if !strings.Contains(dialogBody, want) {
			t.Fatalf("workspace remove dialog wiring missing %q", want)
		}
	}
	if strings.Index(dialogBody, "document.body.appendChild(overlay);") > strings.Index(dialogBody, "requestAnimationFrame(") {
		t.Fatal("workspace remove dialog is appended only after its deferred focus callback")
	}
}
