package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// TestManagedComputerUseAXLive exercises the real local installation, managed
// MCP provenance, and OS sandbox against an already-running disposable fixture.
// It never launches/focuses applications or requests OS permission. Run ONLY this
// test after the fixture is foreground and Accessibility is already authorized:
//
// METIS_CU_AX_LIVE_TEST=1 METIS_CU_AX_LIVE_HELPER=/absolute/path/to/metis-cu \
// METIS_CU_AX_FIXTURE_PID=<fixture-pid> go test ./internal/runtime/mcp \
// -run '^TestManagedComputerUseAXLive$' -count=1 -v
//
// The full app grant is preseeded ONLY in a temporary HOME. This validates grant
// enforcement and sandbox access, NOT the app-approval or macOS permission UI.
func TestManagedComputerUseAXLive(t *testing.T) {
	if os.Getenv("METIS_CU_AX_LIVE_TEST") != "1" {
		t.Skip("managed disposable AX fixture acceptance is opt-in")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("native AX live acceptance requires macOS")
	}
	helper := os.Getenv("METIS_CU_AX_LIVE_HELPER")
	if !filepath.IsAbs(helper) {
		t.Fatal("METIS_CU_AX_LIVE_HELPER must be an explicit absolute helper path")
	}
	fixturePID, err := strconv.Atoi(os.Getenv("METIS_CU_AX_FIXTURE_PID"))
	if err != nil || fixturePID <= 0 {
		t.Fatal("METIS_CU_AX_FIXTURE_PID must identify the explicitly launched disposable fixture")
	}

	// Canonicalize our own new directory, never the user's real home. t.Setenv
	// changes only this nonparallel test process and is restored by testing.
	privateRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateHome := filepath.Join(privateRoot, "home")
	metisHome := filepath.Join(privateHome, ".metis")
	if err := os.MkdirAll(metisHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", privateHome)
	t.Setenv("METIS_HOME", metisHome)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	installed, err := computeruse.New(metisHome).InstallLocal(ctx, helper)
	if err != nil {
		t.Fatalf("install explicit helper into private METIS_HOME: %v", err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode: "permissions", Network: sandbox.NetworkBlock,
		MetisHome: metisHome, TempRoot: privateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close private sandbox: %v", err)
		}
	})
	launch := func() *mcpsdk.Client {
		t.Helper()
		reg := &Registry{}
		if err := SetManagedComputerUseServer(reg); err != nil {
			t.Fatal(err)
		}
		entry, err := prepareManagedComputerUseEntry(ctx, reg.Servers[0])
		if err != nil {
			t.Fatalf("prepare managed installation: %v", err)
		}
		env, profile := stdioLaunchEnvAndProfile(entry)
		if entry.Command != installed.Path || !entry.managedComputerUse || profile != mcpsdk.StdioSandboxProfileManagedComputerUse {
			t.Fatal("verified installation did not select the managed Computer Use profile")
		}
		client, err := mcpsdk.NewStdioClientWithEnvAndDirAndSandboxProfile(ctx, entry.Command, env, entry.WorkingDir, manager, profile, entry.Args...)
		if err != nil {
			t.Fatalf("launch real managed MCP through sandbox: %v", err)
		}
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Errorf("close managed helper: %v", err)
			}
		})
		return client
	}

	// No implicit Terminal/full override may permit even an AX read. Restart
	// after seeding the fixture-only grant because helpers cache grants per process.
	ungranted := launch()
	denied := managedAXLiveCall(t, ctx, ungranted, "native_ax_snapshot", map[string]interface{}{})
	managedAXLiveOutcome(t, denied, true, "not_sent", "not_requested")
	if !strings.Contains(denied.text(t), "explicit read-or-higher grant") {
		t.Fatal("ungranted snapshot failed for a reason other than explicit app-grant enforcement")
	}
	if err := ungranted.Close(); err != nil {
		t.Fatalf("stop ungranted helper before changing private grant: %v", err)
	}
	grantDir := filepath.Join(privateHome, ".metis-cu")
	if err := os.MkdirAll(grantDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grantDir, "granted.json"), []byte(`{"Metis CU AX Fixture":"full"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := launch()
	read := func() managedAXLiveSnapshot {
		t.Helper()
		result := managedAXLiveCall(t, ctx, client, "native_ax_snapshot", map[string]interface{}{"max_nodes": 100, "max_depth": 8})
		if result.IsError {
			t.Fatalf("fixture snapshot failed: %s", result.text(t))
		}
		var snapshot managedAXLiveSnapshot
		if err := json.Unmarshal([]byte(result.text(t)), &snapshot); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		if snapshot.App.PID != fixturePID || snapshot.App.BundleID != "dev.metis.cu.ax-fixture" || snapshot.App.Name != "Metis CU AX Fixture" || snapshot.App.InstanceID == "" {
			t.Fatal("snapshot is not the explicitly authorized fixture process; no action will be sent")
		}
		if snapshot.SnapshotID == "" || snapshot.WindowRef == "" || len(snapshot.Nodes) == 0 || len(snapshot.Nodes) > 100 || snapshot.Truncated || !snapshot.ExpiresAt.After(time.Now()) {
			t.Fatal("fixture snapshot is empty, unbounded, truncated, expired, or lacks scoped references")
		}
		secure := false
		for _, node := range snapshot.Nodes {
			if node.Secure {
				secure = true
				if node.Name != "" || node.Value != "" || node.ElementRef != "" || node.Editable || len(node.Actions) != 0 {
					t.Fatal("secure fixture node exposes content or an actionable reference")
				}
			}
		}
		if !secure {
			t.Fatal("secure fixture input is absent; cannot validate redaction")
		}
		return snapshot
	}
	act := func(snapshot managedAXLiveSnapshot, ref, action, value string) managedAXLiveResult {
		t.Helper()
		args := map[string]interface{}{"snapshot_id": snapshot.SnapshotID, "element_ref": ref}
		if action == "set_value" {
			args["value"] = value
		}
		return managedAXLiveCall(t, ctx, client, "native_ax_"+action, args)
	}
	initial := read()
	baseline := initial.counter(t)
	for _, node := range initial.Nodes {
		if node.Secure {
			if node.ID == "" {
				t.Fatal("secure fixture node has no internal ID to challenge reference isolation")
			}
			// Internal tree IDs are not capabilities. Secure nodes deliberately
			// have no public element_ref, so this must not reach the AX backend.
			managedAXLiveOutcome(t, act(initial, node.ID, "set_value", "must-not-replace-fixture-secret"), true, "not_sent", "not_requested")
		}
	}
	button := initial.find(t, "Increment fixture counter")
	managedAXLiveOutcome(t, act(initial, button.ElementRef, "press", ""), false, "sent", "unknown")
	// Never retry a delivered mutation: replay here must be refused as stale.
	managedAXLiveOutcome(t, act(initial, button.ElementRef, "press", ""), true, "not_sent", "not_requested")
	var updated managedAXLiveSnapshot
	for attempt := 0; attempt < 10; attempt++ {
		updated = read()
		if updated.counter(t) == baseline+1 {
			break
		}
		if attempt == 9 {
			t.Fatal("fresh AX evidence did not show exactly one fixture counter increment")
		}
		time.Sleep(30 * time.Millisecond)
	}
	field := updated.find(t, "Fixture plain input")
	const value = "managed fixture verified value"
	managedAXLiveOutcome(t, act(updated, field.ElementRef, "set_value", value), false, "sent", "passed")
	managedAXLiveOutcome(t, act(updated, field.ElementRef, "set_value", "must-not-replay"), true, "not_sent", "not_requested")
	confirmed := read()
	if confirmed.find(t, "Fixture plain input").Value != value || confirmed.counter(t) != baseline+1 {
		t.Fatal("fresh snapshot did not confirm set_value readback and single press effect")
	}
	// A refresh also invalidates the prior snapshot even without an action.
	_ = read()
	managedAXLiveOutcome(t, act(confirmed, confirmed.find(t, "Fixture plain input").ElementRef, "set_value", "must-not-use-refreshed-snapshot"), true, "not_sent", "not_requested")
	if read().find(t, "Fixture plain input").Value != value {
		t.Fatal("a rejected stale reference changed the fixture input")
	}
	t.Log("real private InstallLocal + managed MCP sandbox: no-grant read denied; fixture-only preseeded grant; secure content/ref withheld; press observed exactly once; set_value readback passed; consumed/refreshed references denied (not an OS/app-grant dialog test)")
}

type managedAXLiveResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError           bool `json:"isError"`
	StructuredContent struct {
		Outcome *struct {
			Dispatch     string `json:"dispatch"`
			Verification string `json:"verification"`
			Reason       string `json:"reason"`
		} `json:"outcome"`
	} `json:"structuredContent"`
}

func managedAXLiveCall(t *testing.T, parent context.Context, client *mcpsdk.Client, name string, args map[string]interface{}) managedAXLiveResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	raw, err := client.CallTool(ctx, name, args)
	if err != nil {
		t.Fatalf("%s MCP transport failed (do not retry mutations): %v", name, err)
	}
	if strings.Contains(string(raw), "fixture-secret-never-export") {
		t.Fatal("synthetic secure fixture value leaked into MCP result")
	}
	var result managedAXLiveResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode %s MCP result: %v", name, err)
	}
	return result
}

func (r managedAXLiveResult) text(t *testing.T) string {
	t.Helper()
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatal("native AX result must contain one text block and no screenshot")
	}
	return r.Content[0].Text
}

func managedAXLiveOutcome(t *testing.T, result managedAXLiveResult, isError bool, dispatch, verification string) {
	t.Helper()
	text := result.text(t)
	outcome := result.StructuredContent.Outcome
	if result.IsError != isError || outcome == nil || outcome.Dispatch != dispatch || outcome.Verification != verification || outcome.Reason == "" {
		t.Fatalf("unexpected native action evidence: isError=%v outcome=%+v; text=%s", result.IsError, outcome, text)
	}
}

type managedAXLiveNode struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Value      string   `json:"value"`
	ElementRef string   `json:"element_ref"`
	Secure     bool     `json:"secure"`
	Editable   bool     `json:"editable"`
	Actions    []string `json:"actions"`
}

type managedAXLiveSnapshot struct {
	SnapshotID string    `json:"snapshot_id"`
	WindowRef  string    `json:"window_ref"`
	ExpiresAt  time.Time `json:"expires_at"`
	Truncated  bool      `json:"truncated"`
	App        struct {
		Name       string `json:"name"`
		PID        int    `json:"pid"`
		BundleID   string `json:"bundle_id"`
		InstanceID string `json:"instance_id"`
	} `json:"app"`
	Nodes []managedAXLiveNode `json:"nodes"`
}

func (s managedAXLiveSnapshot) find(t *testing.T, name string) managedAXLiveNode {
	t.Helper()
	for _, node := range s.Nodes {
		if node.Name == name {
			if node.ElementRef == "" || node.Secure {
				t.Fatalf("nonsecure fixture node %q has no actionable reference", name)
			}
			return node
		}
	}
	t.Fatalf("fixture node %q absent from bounded snapshot", name)
	return managedAXLiveNode{}
}

func (s managedAXLiveSnapshot) counter(t *testing.T) int {
	t.Helper()
	for _, node := range s.Nodes {
		if suffix, ok := strings.CutPrefix(node.Name, "Fixture count "); ok {
			if count, err := strconv.Atoi(suffix); err == nil && count >= 0 {
				return count
			}
		}
	}
	t.Fatal("fixture counter absent from bounded snapshot")
	return 0
}
