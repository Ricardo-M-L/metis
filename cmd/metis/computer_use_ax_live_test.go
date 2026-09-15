package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const (
	cuLiveAXSnapshotTool = "mcp__computer-use__native_ax_snapshot"
	cuLiveAXPressTool    = "mcp__computer-use__native_ax_press"
	cuLiveAXSetValueTool = "mcp__computer-use__native_ax_set_value"
	cuLiveAXOutcomeText  = "Computer Use execution evidence: "
)

// TestComputerUseLiveAXCLI is deliberately separate from the read-only live
// acceptance. The caller must launch/focus the disposable fixture and authorize
// Accessibility beforehand. This test never launches another app or prompts for
// OS/app access. It sends a real model request and allows mutations only through
// AX references returned by the fixture's managed helper.
//
// METIS_CU_AX_CLI_LIVE_TEST=1 METIS_CU_LIVE_OAUTH=1 \
// METIS_CU_TEST_CLI=/absolute/metis METIS_CU_TEST_HELPER=/absolute/metis-cu \
// METIS_CU_AX_FIXTURE_PID=<explicit-fixture-pid> \
// go test ./cmd/metis -run '^TestComputerUseLiveAXCLI$' -count=1 -timeout=330s -v
func TestComputerUseLiveAXCLI(t *testing.T) {
	if os.Getenv("METIS_CU_AX_CLI_LIVE_TEST") != "1" {
		t.Skip("real-model disposable AX fixture acceptance is opt-in")
	}
	if goruntime.GOOS != "darwin" {
		t.Skip("native AX live acceptance requires macOS")
	}
	if os.Getenv("METIS_CU_LIVE_OAUTH") != "1" {
		t.Skip("isolated OAuth acceptance requires explicit METIS_CU_LIVE_OAUTH=1")
	}
	started := time.Now()
	deadline := started.Add(300 * time.Second)
	cli, helper := os.Getenv("METIS_CU_TEST_CLI"), os.Getenv("METIS_CU_TEST_HELPER")
	if !filepath.IsAbs(cli) || !filepath.IsAbs(helper) {
		t.Fatal("explicit absolute built CLI/helper paths required")
	}
	env := newCULiveAXEnvironment(t, deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	runCULiveAXCommand(t, ctx, env, cli, "cu", "install", "--from", helper, "--json")
	runCULiveAXCommand(t, ctx, env, cli, "cu", "enable", "--json")
	output := runCULiveAXCommand(t, ctx, env, cli, "run", "--no-auth-wizard", "--no-markdown", env.Prompt)
	if strings.Contains(output, "cleanup failed") {
		t.Fatal("native cleanup was not confirmed")
	}
	data, files, err := findCULiveAXPromptSession(env.MetisHome, env.Prompt)
	t.Logf("private session directory file types: %+v", files)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := inspectCULiveAXSession(data, env.FixturePID, env.Marker)
	if err != nil {
		t.Logf("AX tool evidence: %+v", evidence)
		t.Fatal(err) // inspector errors contain fixed descriptions, never raw session data
	}
	t.Logf("real CLI provider=openai-codex: AX snapshots=%d; set_value sent/passed=%d; press sent/unknown=%d; recovered discovery misses=%d; final input and single increment confirmed; elapsed=%s", evidence.Snapshots, evidence.SetValues, evidence.Presses, evidence.DiscoveryMisses, time.Since(started).Round(time.Millisecond))
}

// Shared only by the opt-in CLI/Desktop acceptance tests. Never log this value:
// secrets are retained solely for redaction, not for prompts or configuration.
type cuLiveAXEnvironment struct {
	Home, MetisHome, Prompt, Model, Marker string
	FixturePID                             int
	Env, secrets                           []string
}

func newCULiveAXEnvironment(t *testing.T, deadline time.Time) cuLiveAXEnvironment {
	t.Helper()
	if os.Getenv("METIS_CU_LIVE_OAUTH") != "1" {
		t.Fatal("isolated OAuth acceptance requires explicit METIS_CU_LIVE_OAUTH=1")
	}
	fixturePID, err := strconv.Atoi(os.Getenv("METIS_CU_AX_FIXTURE_PID"))
	if err != nil || fixturePID <= 0 {
		t.Fatal("METIS_CU_AX_FIXTURE_PID must identify the explicitly launched disposable fixture")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 300*time.Second {
		t.Fatal("AX live acceptance deadline must be within 300 seconds")
	}
	originalHome, err := auth.ResolveCredentialHome("")
	if err != nil {
		t.Fatal("cannot resolve existing OAuth credential home")
	}
	originalPath := filepath.Join(originalHome, ".credentials", "llm-oauth.json")
	// GetOAuth can migrate the source store. Inspect bytes instead and copy only
	// the selected complete, unexpired entry; never log in or refresh either store.
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal("cannot read existing private OAuth store; acceptance does not migrate or log in")
	}
	originalHash := sha256.Sum256(original)
	t.Cleanup(func() {
		after, err := os.ReadFile(originalPath)
		if err != nil || sha256.Sum256(after) != originalHash {
			t.Error("original OAuth credential store changed during AX acceptance")
		}
	})
	credential, err := decodeCULiveOAuth(original, time.Now(), deadline)
	if err != nil {
		t.Fatal(err)
	}
	// The existing credential home is now established and hashed. This scoped
	// loader reads user provider settings without Load's legacy-home migration;
	// unrelated project provider/MCP/hook configuration is never activated.
	providers, err := config.LoadProviderSetForWorkspace(false)
	if err != nil {
		t.Fatal("cannot read selected OpenAI Codex model configuration")
	}
	privateRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot canonicalize isolated acceptance directory")
	}
	env := cuLiveAXEnvironment{
		Home: filepath.Join(privateRoot, "home"), FixturePID: fixturePID,
		Model:   providers.OpenAICodex.Model,
		secrets: []string{credential.AccessToken, credential.RefreshToken, credential.AccountID},
	}
	env.MetisHome = filepath.Join(env.Home, ".metis")
	privateDir := filepath.Join(env.MetisHome, ".credentials")
	grantDir := filepath.Join(env.Home, ".metis-cu")
	for _, dir := range []string{env.Home, env.MetisHome, privateDir, grantDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal("cannot create private AX acceptance directories")
		}
	}
	minimal := config.Config{}
	minimal.Provider.Default = "openai-codex"
	minimal.Provider.OpenAICodex = providers.OpenAICodex
	minimal.Permission.Mode = "bypassPermissions"
	minimal.Tools.Allowed = []string{"ToolSearch", "Skill", cuLiveAXSnapshotTool, cuLiveAXPressTool, cuLiveAXSetValueTool}
	minimal.Tools.Bash.Sandbox.Mode = "permissions"
	minimal.Tools.Bash.Sandbox.Network = "block"
	minimal.Session.Dir = filepath.Join(env.MetisHome, "sessions")
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(minimal); err != nil {
		t.Fatal("cannot encode isolated configuration")
	}
	for _, secret := range env.secrets {
		if secret != "" && strings.Contains(encoded.String(), secret) {
			t.Fatal("refusing credential-bearing acceptance configuration")
		}
	}
	if err := os.WriteFile(filepath.Join(env.MetisHome, "config.toml"), encoded.Bytes(), 0o600); err != nil {
		t.Fatal("cannot stage isolated configuration")
	}
	if err := os.WriteFile(filepath.Join(grantDir, "granted.json"), []byte(`{"Metis CU AX Fixture":"full"}`), 0o600); err != nil {
		t.Fatal("cannot stage fixture-only app grant")
	}
	payload, err := json.Marshal(struct {
		FormatVersion int                             `json:"format_version"`
		Credentials   map[string]auth.OAuthCredential `json:"credentials"`
	}{1, map[string]auth.OAuthCredential{"openai-codex": *credential}})
	if err != nil {
		t.Fatal("cannot encode selected isolated OAuth credential")
	}
	isolatedPath := filepath.Join(privateDir, "llm-oauth.json")
	if err := os.WriteFile(isolatedPath, payload, 0o600); err != nil {
		t.Fatal("cannot stage selected isolated OAuth credential")
	}
	isolatedHash := sha256.Sum256(payload)
	// Registered after TempDir so this hash check runs before private cleanup.
	t.Cleanup(func() {
		after, err := os.ReadFile(isolatedPath)
		if err != nil || sha256.Sum256(after) != isolatedHash {
			t.Error("isolated OAuth store changed; AX acceptance must not refresh credentials")
		}
	})
	env.Env = isolatedCULiveAXEnv(os.Environ(), env.Home, env.MetisHome)
	var marker [16]byte
	if _, err := rand.Read(marker[:]); err != nil {
		t.Fatal("cannot allocate fresh fixture marker")
	}
	env.Marker = "metis-ax-live-" + hex.EncodeToString(marker[:])
	env.Prompt = fmt.Sprintf("This is an authorized disposable native AX acceptance test. The already-foreground application must be Metis CU AX Fixture, bundle dev.metis.cu.ax-fixture, PID %d. Its app grant is already configured. Use ToolSearch and Skill if needed to discover the managed Computer Use tools and inspect their schemas. Only native_ax_snapshot, native_ax_set_value, and native_ax_press are available for UI work. First obtain your own bounded native_ax_snapshot (max_nodes 100, max_depth 8), verify the exact application name, bundle and PID, and remember the initial Fixture count N. If identity differs or a required control is absent, stop without changing anything. From actual fresh snapshot references, set Fixture plain input to exactly %q, then press Increment fixture counter exactly once. Decide each action from the observed control names and refs. Snapshots expire after 30 seconds and every action consumes them: obtain a separate fresh snapshot after each action and before using the next ref. Do not repeat either mutation or act on secure fields. A sent press with unknown verification means it was delivered: verify through a new snapshot, never press it again. Finish only when a final native_ax_snapshot proves the plain input equals the marker and Fixture count is initial N plus exactly one. Report the actual dispatch/verification and observed result briefly. Do not use Bash, screenshots, coordinate input, clipboard, browsers, other applications, or any other tools. If the fixture loses focus, stop and report it.", fixturePID, env.Marker)
	return env
}

func isolatedCULiveAXEnv(parent []string, home, metisHome string) []string {
	// Keep basic process/TLS/proxy settings, not arbitrary API keys, provider
	// overrides, user configuration roots, MCPs, plugins, or helper grant overrides.
	env := []string{}
	for _, item := range parent {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch name {
		case "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			env = append(env, item)
		}
	}
	return append(env, "HOME="+home, "METIS_HOME="+metisHome, "METIS_AUTO_MEMORY=0", "METIS_NO_UPDATE_CHECK=1")
}

func runCULiveAXCommand(t *testing.T, ctx context.Context, env cuLiveAXEnvironment, binary string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = env.Home, env.Env
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated AX acceptance process failed: %v (%d output bytes suppressed)", err, len(output))
	}
	return redactCULiveOutput(string(output), env.secrets)
}

type cuLiveAXEvidence struct {
	Calls, Results, Snapshots, SetValues, Presses int
	DiscoveryMisses                               int
	FinalVerified                                 bool
}

type cuLiveAXNode struct {
	Role       string   `json:"role"`
	Name       string   `json:"name"`
	Value      string   `json:"value"`
	ElementRef string   `json:"element_ref"`
	Secure     bool     `json:"secure"`
	Enabled    bool     `json:"enabled"`
	Editable   bool     `json:"editable"`
	Actions    []string `json:"actions"`
}

type cuLiveAXSnapshot struct {
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
	Nodes []cuLiveAXNode `json:"nodes"`
}

func decodeCULiveAXSnapshot(output string, fixturePID int) (cuLiveAXSnapshot, error) {
	var snapshot cuLiveAXSnapshot
	if json.Unmarshal([]byte(output), &snapshot) != nil {
		return snapshot, errors.New("paired AX snapshot is not valid JSON")
	}
	if fixturePID <= 0 || snapshot.App.PID != fixturePID || snapshot.App.BundleID != "dev.metis.cu.ax-fixture" || snapshot.App.Name != "Metis CU AX Fixture" || snapshot.App.InstanceID == "" {
		return snapshot, errors.New("paired AX snapshot is not the explicitly authorized fixture process")
	}
	if snapshot.SnapshotID == "" || snapshot.WindowRef == "" || snapshot.ExpiresAt.IsZero() || snapshot.Truncated || len(snapshot.Nodes) == 0 || len(snapshot.Nodes) > 200 {
		return snapshot, errors.New("paired AX snapshot is incomplete, truncated, or unbounded")
	}
	secure := false
	for _, node := range snapshot.Nodes {
		if node.Secure {
			secure = true
			if node.Name != "" || node.Value != "" || node.ElementRef != "" || node.Editable || len(node.Actions) != 0 {
				return snapshot, errors.New("paired AX snapshot exposed secure fixture content or capability")
			}
		}
	}
	if !secure {
		return snapshot, errors.New("paired AX snapshot does not include the redacted secure fixture control")
	}
	return snapshot, nil
}

func (s cuLiveAXSnapshot) control(name string) (cuLiveAXNode, bool) {
	var found cuLiveAXNode
	count := 0
	for _, node := range s.Nodes {
		if node.Name == name {
			found = node
			count++
		}
	}
	return found, count == 1 && !found.Secure && found.Enabled && found.ElementRef != ""
}

func (s cuLiveAXSnapshot) counter() (int, bool) {
	value, count := 0, 0
	for _, node := range s.Nodes {
		if suffix, ok := strings.CutPrefix(node.Name, "Fixture count "); ok {
			n, err := strconv.Atoi(suffix)
			if err != nil || n < 0 {
				return 0, false
			}
			value, count = n, count+1
		}
	}
	return value, count == 1
}

func validCULiveAXOutcome(output, verification string) bool {
	const separator = "\n\n" + cuLiveAXOutcomeText
	parts := strings.Split(output, separator)
	if len(parts) != 2 {
		return false
	}
	var native struct {
		Dispatched      bool   `json:"dispatched"`
		DispatchUnknown bool   `json:"dispatch_unknown"`
		Verified        bool   `json:"verified"`
		Reason          string `json:"reason"`
	}
	var evidence struct {
		Outcome *struct {
			Dispatch     string `json:"dispatch"`
			Verification string `json:"verification"`
			Reason       string `json:"reason"`
		} `json:"outcome"`
	}
	return json.Unmarshal([]byte(parts[0]), &native) == nil && json.Unmarshal([]byte(parts[1]), &evidence) == nil &&
		evidence.Outcome != nil && evidence.Outcome.Dispatch == "sent" && evidence.Outcome.Verification == verification && evidence.Outcome.Reason != "" &&
		native.Dispatched && !native.DispatchUnknown && native.Verified == (verification == "passed") && native.Reason == evidence.Outcome.Reason
}

// Replayed history_replace records may repeat existing call IDs. They do not
// represent a second native execution; conflicting duplicates, orphaned results,
// prose-only claims, and unsuccessful actions never count as acceptance evidence.
func inspectCULiveAXSession(data []byte, fixturePID int, marker string) (cuLiveAXEvidence, error) {
	var evidence cuLiveAXEvidence
	if fixturePID <= 0 || marker == "" {
		return evidence, errors.New("AX session verification requires explicit fixture PID and fresh marker")
	}
	if bytes.Contains(data, []byte("fixture-secret-never-export")) {
		return evidence, errors.New("secure fixture content leaked into the acceptance session")
	}
	pending := map[string]llm.ContentBlock{}
	calls, results := map[string]string{}, map[string]string{}
	seenSnapshots := map[string]bool{}
	var latest cuLiveAXSnapshot
	instanceID := ""
	baseline, latestCounter := -1, -1
	freshSnapshot := false
	setCalls, pressCalls := 0, 0
	observe := func(message llm.Message, replay bool) error {
		for _, block := range message.Content {
			if block.Type == "image" {
				return errors.New("AX-only acceptance session contains an image")
			}
			if block.Type != "tool_use" && block.Type != "tool_result" {
				continue
			}
			if block.ToolUseID == "" {
				return errors.New("AX acceptance contains a tool block without a pairing ID")
			}
			encoded, _ := json.Marshal(block)
			if block.Type == "tool_use" {
				if message.Role != llm.RoleAssistant {
					return errors.New("AX acceptance tool call was not emitted by the model")
				}
				if previous, exists := calls[block.ToolUseID]; exists {
					if !replay || previous != string(encoded) {
						return errors.New("AX acceptance contains conflicting duplicate tool calls")
					}
					continue
				}
				switch block.ToolName {
				case "ToolSearch", "Skill", cuLiveAXSnapshotTool:
				case cuLiveAXSetValueTool, cuLiveAXPressTool:
					if !freshSnapshot || len(pending) != 0 || block.ToolInput["snapshot_id"] != latest.SnapshotID {
						return errors.New("AX mutation was not grounded in the latest completed fixture snapshot")
					}
					control := "Increment fixture counter"
					if block.ToolName == cuLiveAXSetValueTool {
						control = "Fixture plain input"
						setCalls++
						if setCalls != 1 || block.ToolInput["value"] != marker {
							return errors.New("AX input mutation was repeated or did not use the fresh marker")
						}
					} else {
						pressCalls++
						if pressCalls != 1 {
							return errors.New("AX counter press was attempted more than once")
						}
					}
					node, ok := latest.control(control)
					if !ok || block.ToolInput["element_ref"] != node.ElementRef || (block.ToolName == cuLiveAXSetValueTool && (!node.Editable || (node.Role != "AXTextField" && node.Role != "AXTextArea"))) {
						return errors.New("AX mutation did not target the observed fixture control reference")
					}
					if block.ToolName == cuLiveAXPressTool && !slices.Contains(node.Actions, "AXPress") {
						return errors.New("AX press was not supported by the observed fixture control")
					}
					freshSnapshot = false // every native action consumes the snapshot
				default:
					return errors.New("AX acceptance model called a tool outside the explicit allowlist")
				}
				calls[block.ToolUseID], pending[block.ToolUseID] = string(encoded), block
				evidence.Calls++
				continue
			}
			if message.Role != llm.RoleUser && message.Role != llm.RoleTool {
				return errors.New("AX acceptance tool result has an invalid source role")
			}
			if previous, exists := results[block.ToolUseID]; exists {
				if !replay || previous != string(encoded) {
					return errors.New("AX acceptance contains conflicting duplicate tool results")
				}
				continue
			}
			call, paired := pending[block.ToolUseID]
			if !paired {
				return errors.New("AX acceptance contains an orphaned tool result")
			}
			delete(pending, block.ToolUseID)
			results[block.ToolUseID] = string(encoded)
			evidence.Results++
			// Exact-name discovery deliberately returns IsError on an all-miss
			// lookup (lazy_tools.go). A model may recover from that read-only
			// miss. Record it explicitly; only the fully verified subsequent
			// native task can pass. Never forgive errors on native operations.
			if block.IsError && call.ToolName == "ToolSearch" && strings.HasPrefix(block.ToolResult, "error: tool(s) [") && strings.HasSuffix(block.ToolResult, "] not found") {
				evidence.DiscoveryMisses++
				continue
			}
			if block.IsError || strings.TrimSpace(block.ToolResult) == "" {
				flags := []string{}
				lower := strings.ToLower(block.ToolResult)
				for _, token := range []string{"not found", "no matches", "invalid", "required", "permission", "denied", "expired", "timeout", "unknown", "not available", "not configured"} {
					if strings.Contains(lower, token) {
						flags = append(flags, token)
					}
				}
				// Tool name was allowlisted above; only fixed diagnostic flags
				// and byte counts are exposed, never the native/model payload.
				return fmt.Errorf("AX acceptance paired tool failed: tool=%s isError=%t bytes=%d classes=%v", call.ToolName, block.IsError, len(block.ToolResult), flags)
			}
			switch call.ToolName {
			case cuLiveAXSnapshotTool:
				snapshot, err := decodeCULiveAXSnapshot(block.ToolResult, fixturePID)
				if err != nil {
					return err
				}
				if seenSnapshots[snapshot.SnapshotID] || (instanceID != "" && instanceID != snapshot.App.InstanceID) {
					return errors.New("AX acceptance snapshot was reused or changed fixture process instance")
				}
				count, ok := snapshot.counter()
				if !ok {
					return errors.New("AX snapshot lacks one unambiguous fixture counter")
				}
				field, ok := snapshot.control("Fixture plain input")
				if !ok || !field.Editable {
					return errors.New("AX snapshot lacks the editable plain fixture input")
				}
				if baseline < 0 {
					baseline = count
					if field.Value == marker {
						return errors.New("AX fixture already contained the fresh marker before any mutation")
					}
				}
				if count < baseline || count > baseline+evidence.Presses {
					return errors.New("AX fixture counter changed without the single paired press")
				}
				if evidence.SetValues > 0 && field.Value != marker {
					return errors.New("fresh AX snapshot does not preserve the verified fixture input")
				}
				seenSnapshots[snapshot.SnapshotID] = true
				latest, latestCounter, instanceID, freshSnapshot = snapshot, count, snapshot.App.InstanceID, true
				evidence.Snapshots++
			case cuLiveAXSetValueTool:
				if !validCULiveAXOutcome(block.ToolResult, "passed") {
					return errors.New("paired AX set_value result lacks matching native dispatch and passed readback evidence")
				}
				evidence.SetValues++
			case cuLiveAXPressTool:
				if !validCULiveAXOutcome(block.ToolResult, "unknown") {
					return errors.New("paired AX press result lacks matching native dispatch and unknown verification evidence")
				}
				evidence.Presses++
			}
		}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var entry session.Entry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return evidence, errors.New("AX acceptance session is not valid JSONL")
		}
		switch entry.Type {
		case "message":
			if entry.Message != nil {
				if err := observe(*entry.Message, false); err != nil {
					return evidence, err
				}
			}
		case "history_replace":
			for _, message := range entry.Messages {
				if err := observe(message, true); err != nil {
					return evidence, err
				}
			}
		}
	}
	field, fieldFound := latest.control("Fixture plain input")
	if len(pending) != 0 || evidence.SetValues != 1 || evidence.Presses != 1 || evidence.Snapshots < 3 || !freshSnapshot || !fieldFound || field.Value != marker || latestCounter != baseline+1 {
		return evidence, errors.New("AX acceptance lacks completed paired mutations and a final snapshot proving the input and exactly one increment")
	}
	evidence.FinalVerified = true
	return evidence, nil
}

// Timing and message-metrics JSONL sidecars share the session directory. Select
// one actual conversation by its original user prompt, never by whichever file
// happens to contain passing tool evidence. Only safe type/count metadata leaves
// this helper; filenames and raw session contents are not logged.
type cuLiveAXSessionFiles struct {
	Entries, JSONL, Candidates, PromptMatches, Timing, MessageMetrics, Directories, Other int
}

func findCULiveAXPromptSession(metisHome, prompt string) ([]byte, cuLiveAXSessionFiles, error) {
	var files cuLiveAXSessionFiles
	if strings.TrimSpace(prompt) == "" {
		return nil, files, errors.New("AX acceptance requires the exact user prompt to select its session")
	}
	dir := filepath.Join(metisHome, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, files, errors.New("cannot read private AX session directory")
	}
	files.Entries = len(entries)
	var matched []byte
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir():
			files.Directories++
			continue
		case !entry.Type().IsRegular():
			files.Other++
			continue
		case strings.HasSuffix(name, ".timing.jsonl"):
			files.JSONL++
			files.Timing++
			continue
		case strings.HasSuffix(name, ".message-metrics.jsonl"):
			files.JSONL++
			files.MessageMetrics++
			continue
		case !strings.HasSuffix(name, ".jsonl"):
			files.Other++
			continue
		}
		files.JSONL++
		files.Candidates++
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, files, errors.New("cannot read private AX session candidate")
		}
		found, err := cuLiveAXSessionHasPrompt(data, prompt)
		if err != nil {
			return nil, files, err
		}
		if found {
			files.PromptMatches++
			matched = data
		}
	}
	if files.PromptMatches != 1 {
		return nil, files, errors.New("private AX session directory must contain exactly one conversation with the acceptance user prompt")
	}
	return matched, files, nil
}

func cuLiveAXSessionHasPrompt(data []byte, prompt string) (bool, error) {
	found := false
	observe := func(message llm.Message) {
		if message.Role != llm.RoleUser {
			return
		}
		for _, block := range message.Content {
			if block.Type == "text" && !block.Synthetic && strings.TrimSpace(block.Text) == strings.TrimSpace(prompt) {
				found = true
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var entry session.Entry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return found, nil
		} else if err != nil {
			return false, errors.New("private AX session candidate is not complete valid JSONL")
		}
		switch entry.Type {
		case "message":
			if entry.Message != nil {
				observe(*entry.Message)
			}
		case "history_replace":
			for _, message := range entry.Messages {
				observe(message)
			}
		}
	}
}

func TestComputerUseLiveAXEnvironmentFiltersInheritedState(t *testing.T) {
	got := isolatedCULiveAXEnv([]string{
		"HOME=/real/home", "METIS_HOME=/real/metis", "METIS_API_KEY=fake-secret",
		"METIS_CU_ACCESS=full", "OPENAI_API_KEY=fake-openai", "XDG_CONFIG_HOME=/real/config",
		"PATH=/bin", "LANG=en_US.UTF-8", "HTTPS_PROXY=http://example.invalid:8080",
	}, "/private/home", "/private/home/.metis")
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"/real/", "fake-secret", "fake-openai", "METIS_CU_ACCESS", "XDG_CONFIG_HOME"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("isolated environment retained unrelated configuration or credentials")
		}
	}
	for _, required := range []string{"HOME=/private/home", "METIS_HOME=/private/home/.metis", "PATH=/bin", "HTTPS_PROXY=http://example.invalid:8080", "METIS_AUTO_MEMORY=0", "METIS_NO_UPDATE_CHECK=1"} {
		if !strings.Contains(joined, required) {
			t.Fatal("isolated environment lost an explicit required process setting")
		}
	}
}

func TestComputerUseLiveAXSelectsOnlyUniquePromptSession(t *testing.T) {
	metisHome := t.TempDir()
	dir := filepath.Join(metisHome, "sessions")
	if err := os.MkdirAll(filepath.Join(dir, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	const prompt = "synthetic acceptance prompt with fresh marker"
	userPrompt := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: prompt}}}
	conversation := encodeCULiveAXMessages(t, append([]llm.Message{userPrompt}, syntheticCULiveAXMessages(t)...))
	for name, data := range map[string][]byte{
		"actual.jsonl":                 conversation,
		"actual.timing.jsonl":          []byte("{\"step\":1}\n"),
		"actual.message-metrics.jsonl": []byte("{\"tokens\":100}\n"),
		"empty-session.jsonl":          []byte("{\"type\":\"header\"}\n"),
		"assistant-prose.jsonl":        encodeCULiveAXMessages(t, []llm.Message{{Role: llm.RoleAssistant, Content: userPrompt.Content}}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, files, err := findCULiveAXPromptSession(metisHome, prompt)
	if err != nil || !bytes.Equal(data, conversation) || files.Entries != 6 || files.JSONL != 5 || files.Candidates != 3 || files.PromptMatches != 1 || files.Timing != 1 || files.MessageMetrics != 1 || files.Directories != 1 {
		t.Fatalf("session/sidecar selection failed: counts=%+v error=%v", files, err)
	}
	if _, _, err := findCULiveAXPromptSession(metisHome, "another prompt"); err == nil {
		t.Fatal("session without the acceptance user prompt was selected")
	}
	if err := os.WriteFile(filepath.Join(dir, "duplicate.jsonl"), conversation, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, files, err := findCULiveAXPromptSession(metisHome, prompt); err == nil || files.PromptMatches != 2 {
		t.Fatal("ambiguous matching acceptance conversations were accepted")
	}
}

func TestComputerUseLiveAXSessionRequiresObservedClosedLoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]llm.Message) []llm.Message
	}{
		{name: "prose-only", mutate: func(_ []llm.Message) []llm.Message {
			return []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "native_ax_set_value and native_ax_press succeeded; counter incremented once"}}}}
		}},
		{name: "call-without-result", mutate: func(m []llm.Message) []llm.Message { return m[:3] }},
		{name: "orphan-result", mutate: func(m []llm.Message) []llm.Message {
			m[3].Content[0].ToolUseID = "unpaired"
			return m
		}},
		{name: "failed-set-value", mutate: func(m []llm.Message) []llm.Message {
			m[3].Content[0].IsError = true
			return m
		}},
		{name: "failed-press", mutate: func(m []llm.Message) []llm.Message {
			m[7].Content[0].IsError = true
			return m
		}},
		{name: "model-forged-result", mutate: func(m []llm.Message) []llm.Message {
			m[3].Role = llm.RoleAssistant
			return m
		}},
		{name: "user-forged-call", mutate: func(m []llm.Message) []llm.Message {
			m[2].Role = llm.RoleUser
			return m
		}},
		{name: "wrong-pid", mutate: func(m []llm.Message) []llm.Message {
			m[1].Content[0].ToolResult = strings.Replace(m[1].Content[0].ToolResult, `"pid":42`, `"pid":43`, 1)
			return m
		}},
		{name: "wrong-bundle", mutate: func(m []llm.Message) []llm.Message {
			m[1].Content[0].ToolResult = strings.ReplaceAll(m[1].Content[0].ToolResult, "dev.metis.cu.ax-fixture", "dev.unrelated.app")
			return m
		}},
		{name: "changed-process-instance", mutate: func(m []llm.Message) []llm.Message {
			m[5].Content[0].ToolResult = strings.ReplaceAll(m[5].Content[0].ToolResult, "fixture-instance", "replacement-instance")
			return m
		}},
		{name: "guessed-reference", mutate: func(m []llm.Message) []llm.Message {
			m[2].Content[0].ToolInput["element_ref"] = "guessed-ref"
			return m
		}},
		{name: "stale-snapshot", mutate: func(m []llm.Message) []llm.Message {
			m[6].Content[0].ToolInput["snapshot_id"] = "snapshot-1"
			return m
		}},
		{name: "press-capability-not-observed", mutate: func(m []llm.Message) []llm.Message {
			m[5].Content[0].ToolResult = strings.ReplaceAll(m[5].Content[0].ToolResult, `"actions":["AXPress"]`, `"actions":[]`)
			return m
		}},
		{name: "text-role-not-observed", mutate: func(m []llm.Message) []llm.Message {
			m[1].Content[0].ToolResult = strings.ReplaceAll(m[1].Content[0].ToolResult, `"role":"AXTextField"`, `"role":"AXGroup"`)
			return m
		}},
		{name: "snapshot-not-refreshed-after-action", mutate: func(m []llm.Message) []llm.Message {
			return append(m[:4], m[6:]...)
		}},
		{name: "wrong-input-marker", mutate: func(m []llm.Message) []llm.Message {
			m[2].Content[0].ToolInput["value"] = "wrong-marker"
			return m
		}},
		{name: "no-dispatch-evidence", mutate: func(m []llm.Message) []llm.Message {
			m[3].Content[0].ToolResult = `{"dispatched":true,"verified":true,"reason":"value_readback_matched"}`
			return m
		}},
		{name: "unknown-dispatch", mutate: func(m []llm.Message) []llm.Message {
			m[3].Content[0].ToolResult = strings.ReplaceAll(m[3].Content[0].ToolResult, `"dispatch":"sent"`, `"dispatch":"unknown"`)
			return m
		}},
		{name: "unverified-set-value", mutate: func(m []llm.Message) []llm.Message {
			m[3].Content[0].ToolResult = strings.ReplaceAll(m[3].Content[0].ToolResult, `"verification":"passed"`, `"verification":"unknown"`)
			return m
		}},
		{name: "fabricated-press-verification", mutate: func(m []llm.Message) []llm.Message {
			m[7].Content[0].ToolResult = strings.ReplaceAll(m[7].Content[0].ToolResult, `"verification":"unknown"`, `"verification":"passed"`)
			return m
		}},
		{name: "native-acknowledgement-mismatch", mutate: func(m []llm.Message) []llm.Message {
			m[7].Content[0].ToolResult = strings.ReplaceAll(m[7].Content[0].ToolResult, `"dispatched":true`, `"dispatched":false`)
			return m
		}},
		{name: "duplicate-call-id-outside-replay", mutate: func(m []llm.Message) []llm.Message {
			return append(m, m[6], m[7])
		}},
		{name: "second-press-new-id", mutate: func(m []llm.Message) []llm.Message {
			second := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "second-press", ToolName: cuLiveAXPressTool, ToolInput: map[string]any{"snapshot_id": "snapshot-3", "element_ref": "button-3"}}}}
			return append(m, second)
		}},
		{name: "missing-final-snapshot", mutate: func(m []llm.Message) []llm.Message { return m[:8] }},
		{name: "final-counter-unchanged", mutate: func(m []llm.Message) []llm.Message {
			m[9].Content[0].ToolResult = strings.ReplaceAll(m[9].Content[0].ToolResult, "Fixture count 8", "Fixture count 7")
			return m
		}},
		{name: "counter-incremented-twice", mutate: func(m []llm.Message) []llm.Message {
			m[9].Content[0].ToolResult = strings.ReplaceAll(m[9].Content[0].ToolResult, "Fixture count 8", "Fixture count 9")
			return m
		}},
		{name: "final-input-wrong", mutate: func(m []llm.Message) []llm.Message {
			m[9].Content[0].ToolResult = strings.ReplaceAll(m[9].Content[0].ToolResult, "synthetic-fresh-marker", "wrong-value")
			return m
		}},
		{name: "secure-value-leaked", mutate: func(m []llm.Message) []llm.Message {
			m[1].Content[0].ToolResult = strings.ReplaceAll(m[1].Content[0].ToolResult, `"secure":true`, `"secure":true,"value":"fixture-secret-never-export"`)
			return m
		}},
		{name: "secure-reference-exposed", mutate: func(m []llm.Message) []llm.Message {
			m[1].Content[0].ToolResult = strings.ReplaceAll(m[1].Content[0].ToolResult, `"secure":true`, `"secure":true,"element_ref":"must-not-expose"`)
			return m
		}},
		{name: "tool-outside-allowlist", mutate: func(m []llm.Message) []llm.Message {
			m[2].Content[0].ToolName = "Bash"
			return m
		}},
		{name: "image-in-ax-session", mutate: func(m []llm.Message) []llm.Message {
			m[9].Content = append(m[9].Content, llm.ContentBlock{Type: "image", Data: "synthetic-image"})
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := tc.mutate(syntheticCULiveAXMessages(t))
			_, err := inspectCULiveAXSession(encodeCULiveAXMessages(t, messages), 42, "synthetic-fresh-marker")
			if err == nil {
				t.Fatal("incomplete or invalid synthetic AX evidence was accepted")
			}
			for _, private := range []string{"synthetic-fresh-marker", "fixture-secret-never-export", "fixture-instance", "must-not-expose"} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("AX inspection error disclosed private session data")
				}
			}
		})
	}
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete-replayed-%t", replay), func(t *testing.T) {
			messages := syntheticCULiveAXMessages(t)
			data := encodeCULiveAXMessages(t, messages)
			if replay {
				var replacement bytes.Buffer
				if err := json.NewEncoder(&replacement).Encode(session.Entry{Type: "history_replace", Messages: messages}); err != nil {
					t.Fatal(err)
				}
				data = append(data, replacement.Bytes()...)
			}
			got, err := inspectCULiveAXSession(data, 42, "synthetic-fresh-marker")
			if err != nil || !got.FinalVerified || got.Calls != 5 || got.Results != 5 || got.Snapshots != 3 || got.Presses != 1 || got.SetValues != 1 {
				t.Fatalf("synthetic closed loop was not counted once: %+v, error=%v", got, err)
			}
		})
	}
}

func TestComputerUseLiveAXReportsRecoveredDiscoveryMiss(t *testing.T) {
	miss := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "discovery-miss", ToolName: "ToolSearch", ToolInput: map[string]any{"query": "select:native_ax_snapshot"}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "discovery-miss", IsError: true, ToolResult: "error: tool(s) [native_ax_snapshot] not found"}}},
	}
	if _, err := inspectCULiveAXSession(encodeCULiveAXMessages(t, miss), 42, "synthetic-fresh-marker"); err == nil {
		t.Fatal("a discovery miss alone must never pass task acceptance")
	}
	messages := append(miss, syntheticCULiveAXMessages(t)...)
	got, err := inspectCULiveAXSession(encodeCULiveAXMessages(t, messages), 42, "synthetic-fresh-marker")
	if err != nil || !got.FinalVerified || got.DiscoveryMisses != 1 || got.Presses != 1 || got.SetValues != 1 {
		t.Fatalf("recovered read-only discovery miss was not reported with a verified task: %+v %v", got, err)
	}
}

func encodeCULiveAXMessages(t *testing.T, messages []llm.Message) []byte {
	t.Helper()
	var data bytes.Buffer
	for _, message := range messages {
		if err := json.NewEncoder(&data).Encode(session.Entry{Type: "message", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	return data.Bytes()
}

func syntheticCULiveAXMessages(t *testing.T) []llm.Message {
	t.Helper()
	snapshot := func(index, count int, value string) string {
		wire := map[string]any{
			"snapshot_id": fmt.Sprintf("snapshot-%d", index), "window_ref": fmt.Sprintf("window-%d", index),
			"expires_at": "2026-09-13T01:00:30Z", "truncated": false,
			"app": map[string]any{"name": "Metis CU AX Fixture", "pid": 42, "bundle_id": "dev.metis.cu.ax-fixture", "instance_id": "fixture-instance"},
			"nodes": []map[string]any{
				{"name": "Fixture plain input", "role": "AXTextField", "value": value, "element_ref": fmt.Sprintf("field-%d", index), "editable": true, "enabled": true},
				{"name": "Increment fixture counter", "element_ref": fmt.Sprintf("button-%d", index), "enabled": true, "actions": []string{"AXPress"}},
				{"name": fmt.Sprintf("Fixture count %d", count)}, {"secure": true},
			},
		}
		data, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	call := func(id, name string, input map[string]any) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: input}}}
	}
	result := func(id, output string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: id, ToolResult: output}}}
	}
	return []llm.Message{
		call("snapshot-1", cuLiveAXSnapshotTool, map[string]any{}), result("snapshot-1", snapshot(1, 7, "initial fixture value")),
		call("set-1", cuLiveAXSetValueTool, map[string]any{"snapshot_id": "snapshot-1", "element_ref": "field-1", "value": "synthetic-fresh-marker"}),
		result("set-1", `{"dispatched":true,"verified":true,"reason":"value_readback_matched"}`+"\n\n"+cuLiveAXOutcomeText+`{"outcome":{"dispatch":"sent","verification":"passed","reason":"value_readback_matched"}}`),
		call("snapshot-2", cuLiveAXSnapshotTool, map[string]any{}), result("snapshot-2", snapshot(2, 7, "synthetic-fresh-marker")),
		call("press-1", cuLiveAXPressTool, map[string]any{"snapshot_id": "snapshot-2", "element_ref": "button-2"}),
		result("press-1", `{"dispatched":true,"verified":false,"reason":"native_press_dispatched"}`+"\n\n"+cuLiveAXOutcomeText+`{"outcome":{"dispatch":"sent","verification":"unknown","reason":"native_press_dispatched"}}`),
		call("snapshot-3", cuLiveAXSnapshotTool, map[string]any{}), result("snapshot-3", snapshot(3, 8, "synthetic-fresh-marker")),
	}
}
