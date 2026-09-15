package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/computeruse"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	metisruntime "github.com/Ricardo-M-L/metis/internal/runtime"
	xdraw "golang.org/x/image/draw"
)

// TestComputerUseVisualLiveProvider verifies real helper zoom(region) -> MCP image
// -> Metis provider adapter -> model recognition. It is NOT a CLI autonomous
// visual-loop, permission-dialog, click, or AX acceptance test. No fake image is
// supplied to the helper or model. The ONLY native tool call is zoom,
// exactly once; no tools are offered to the model (a stricter allowlist than
// ToolSearch/Skill/zoom). Nothing is inferred from model prose alone.
//
// First explicitly launch the disposable native-visual-fixture.swift app with
// bundle dev.metis.cu.visual-fixture, on ONE nonmirrored display, in foreground.
// Its METIS_CU_VISUAL_STATE_DIR must be an existing canonical 0700 temp directory.
// Screen Recording must already be authorized; this test never requests it.
// The test will not launch/focus the fixture or any other app. Escape or the
// fixture's five-minute timer exits it. Then run ONLY this test:
//
// METIS_CU_VISUAL_LIVE_TEST=1 METIS_CU_VISUAL_UPLOAD_FIXTURE=1 \
// METIS_CU_TEST_HELPER=/absolute/built/metis-cu \
// METIS_CU_VISUAL_FIXTURE=/absolute/Fixture.app/Contents/MacOS/fixture \
// METIS_CU_VISUAL_FIXTURE_PID=<explicit-pid> \
// METIS_CU_VISUAL_STATE_DIR=/absolute/private/tempdir \
// METIS_CU_LIVE_PROVIDER=openai-codex METIS_CU_LIVE_OAUTH=1 \
// go test ./cmd/metis -run '^TestComputerUseVisualLiveProvider$' -count=1 -v
//
// Foreground checks alone have a TOCTOU gap. Therefore the real capture stays
// local until EVERY decoded pixel equals the fixture's known raster, including
// the fixed crop's opaque background. Retina normalization is replicated only for
// comparison. The unmodified, real MCP image is what reaches the provider.
// Unexpected display geometry, color conversion, cursor/overlay, one mismatched
// pixel, or any uncertain attestation fails closed BEFORE any model API call.
func TestComputerUseVisualLiveProvider(t *testing.T) {
	if os.Getenv("METIS_CU_VISUAL_LIVE_TEST") != "1" || os.Getenv("METIS_CU_VISUAL_UPLOAD_FIXTURE") != "1" {
		t.Skip("real native screenshot and fixture-only API upload each require explicit opt-in")
	}
	if goruntime.GOOS != "darwin" {
		t.Skip("disposable native visual fixture requires macOS")
	}
	helper, fixture := os.Getenv("METIS_CU_TEST_HELPER"), os.Getenv("METIS_CU_VISUAL_FIXTURE")
	stateDir := os.Getenv("METIS_CU_VISUAL_STATE_DIR")
	pid, err := strconv.Atoi(os.Getenv("METIS_CU_VISUAL_FIXTURE_PID"))
	if err != nil || pid <= 0 || !filepath.IsAbs(helper) || !filepath.IsAbs(fixture) || !filepath.IsAbs(stateDir) {
		t.Fatal("explicit absolute helper, fixture, private state directory and positive fixture PID required")
	}
	if info, err := os.Lstat(stateDir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatal("fixture state directory must be a private 0700 directory, not a symlink")
	}
	canonical, err := filepath.EvalSymlinks(stateDir)
	if err != nil || canonical != stateDir {
		t.Fatal("fixture state directory must be canonical")
	}
	deadline := time.Now().Add(150 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	attest := func() cuVisualAttestation {
		t.Helper()
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer probeCancel()
		cmd := exec.CommandContext(probeCtx, fixture, "--attest", strconv.Itoa(pid))
		// The inspector needs no credentials or user-specific CLI environment.
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		data, err := cmd.Output()
		var report cuVisualAttestation
		if err != nil || len(data) > 4096 || json.Unmarshal(data, &report) != nil || !report.valid(pid) {
			t.Fatal("fixture attestation refused: require exact PID/bundle, foreground, opaque entire sole display and existing Screen Recording access; no image uploaded")
		}
		return report
	}
	initial := attest()
	challengePath := filepath.Join(stateDir, "challenge-"+strconv.Itoa(pid))
	info, err := os.Lstat(challengePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 8 {
		t.Fatal("fixture challenge must be the explicit process's private eight-byte regular file")
	}
	challengeBytes, err := os.ReadFile(challengePath)
	if err != nil || !cuVisualCodeValid(string(challengeBytes)) {
		t.Fatal("fixture challenge unavailable or invalid")
	}
	challenge := string(challengeBytes) // local oracle only, never prompt/metadata/logs

	// Load only the selected provider before replacing HOME. Never read session
	// history, source MCP registrations, plugin directories, or app grant stores.
	providers, err := config.LoadProviderSetForWorkspace(false)
	if err != nil {
		t.Fatal("cannot load selected provider configuration")
	}
	cfg := &config.Config{Provider: providers}
	provider := cfg.Provider.Default
	if selected := os.Getenv("METIS_CU_LIVE_PROVIDER"); selected != "" {
		provider = selected
	}
	var key string
	var credential *auth.OAuthCredential
	if provider == "openai-codex" {
		if os.Getenv("METIS_CU_LIVE_OAUTH") != "1" {
			t.Skip("isolated OAuth requires METIS_CU_LIVE_OAUTH=1")
		}
		originalHome, err := auth.ResolveCredentialHome("")
		if err != nil {
			t.Fatal("cannot resolve existing OAuth credential home")
		}
		originalPath := filepath.Join(originalHome, ".credentials", "llm-oauth.json")
		original, err := os.ReadFile(originalPath)
		if err != nil {
			t.Fatal("cannot read existing OAuth credential; acceptance does not migrate or log in")
		}
		originalHash := sha256.Sum256(original)
		t.Cleanup(func() {
			after, err := os.ReadFile(originalPath)
			if err != nil || sha256.Sum256(after) != originalHash {
				t.Error("source OAuth store changed during visual acceptance")
			}
		})
		credential, err = decodeCULiveOAuth(original, time.Now(), deadline)
		if err != nil {
			t.Fatal(err) // existing decoder emits fixed, credential-free errors
		}
	} else {
		// ResolveAPIKey may migrate a legacy persistent key store. Restrict this
		// acceptance to the selected explicit env/inline key, without that API.
		var keyEnv string
		switch provider {
		case "openai":
			keyEnv, key = cfg.Provider.OpenAI.APIKeyEnv, cfg.Provider.OpenAI.APIKey
		case "anthropic":
			keyEnv, key = cfg.Provider.Anthropic.APIKeyEnv, cfg.Provider.Anthropic.APIKey
		default:
			raw := cfg.Provider.Custom[provider]
			keyEnv, key = raw.APIKeyEnv, raw.APIKey
		}
		if value := os.Getenv(keyEnv); value != "" {
			key = value
		}
		if strings.TrimSpace(key) == "" {
			t.Fatal("selected provider requires an existing explicit env/inline API key; key-store migration is disabled")
		}
	}

	privateHome := t.TempDir()
	if err := os.Chmod(privateHome, 0o700); err != nil {
		t.Fatal("cannot secure temporary HOME")
	}
	metisHome := filepath.Join(privateHome, ".metis")
	cuHome := filepath.Join(privateHome, ".metis-cu")
	for _, dir := range []string{metisHome, cuHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal("cannot create private acceptance directories")
		}
	}
	// No inherited debug/dump/credential-home override may escape the isolated
	// directory. testing restores the environment and removes it on every exit.
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "METIS_") {
			t.Setenv(name, "")
		}
	}
	t.Setenv("HOME", privateHome)
	t.Setenv("METIS_HOME", metisHome)
	t.Setenv("METIS_DUMP_PROMPTS", "0")
	t.Setenv("DUMP_PROMPTS", "0")
	t.Setenv("METIS_AUTO_MEMORY", "0")
	t.Setenv("METIS_NO_UPDATE_CHECK", "1")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "1")
	// Stage model credentials only AFTER the screenshot helper has exited. The
	// helper therefore never inherits this key or sees the isolated OAuth copy.
	stageCredentials := func() {
		t.Setenv("METIS_CU_VALIDATION_API_KEY", key)
		if credential != nil {
			credentialDir := filepath.Join(metisHome, ".credentials")
			if err := os.MkdirAll(credentialDir, 0o700); err != nil {
				t.Fatal("cannot create isolated OAuth store directory")
			}
			payload, err := json.Marshal(struct {
				FormatVersion int                             `json:"format_version"`
				Credentials   map[string]auth.OAuthCredential `json:"credentials"`
			}{1, map[string]auth.OAuthCredential{"openai-codex": *credential}})
			if err != nil {
				t.Fatal("cannot encode isolated OAuth credential")
			}
			isolatedPath := filepath.Join(credentialDir, "llm-oauth.json")
			if err := os.WriteFile(isolatedPath, payload, 0o600); err != nil {
				t.Fatal("cannot stage isolated OAuth credential")
			}
			isolatedHash := sha256.Sum256(payload)
			t.Cleanup(func() {
				after, err := os.ReadFile(isolatedPath)
				if err != nil || sha256.Sum256(after) != isolatedHash {
					t.Error("isolated OAuth credential changed; acceptance must not refresh")
				}
			})
		}
	}
	minimal := cuVisualProviderConfig(t, cfg, provider)
	// Positive caps equal the attested logical size; zero would restore defaults
	// and silently downsample. PNG is required for exact full-image validation.
	cuConfig := fmt.Sprintf("[screenshot]\nformat = \"png\"\nmax_width = %d\nmax_height = %d\n[gate]\ndefault_app_tier = \"read\"\n", initial.Width, initial.Height)
	if err := os.WriteFile(filepath.Join(cuHome, "config.toml"), []byte(cuConfig), 0o600); err != nil {
		t.Fatal("cannot stage isolated lossless screenshot config")
	}
	installed, err := computeruse.New(metisHome).InstallLocal(ctx, helper)
	if err != nil {
		t.Fatal("cannot verify and install explicit real helper into isolated HOME")
	}
	if installed.Description == nil || installed.Description.Permissions["screenRecording"] != "granted" {
		t.Fatal("installed helper lacks existing Screen Recording authorization; no capture or permission request made")
	}
	client, err := mcpsdk.NewStdioClientWithEnvAndDir(ctx, installed.Path, []string{"HOME=" + privateHome, "METIS_HOME=" + metisHome}, privateHome)
	if err != nil {
		t.Fatal("real helper MCP initialization failed; subprocess output suppressed")
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error("real helper cleanup failed; subprocess output suppressed")
		}
	})
	if attest() != initial {
		t.Fatal("fixture identity or geometry changed before capture; no screenshot requested")
	}
	// Exactly one real tool call, statically fixed name and display. Never retry
	// a capture on failure and never allow model-generated native tool calls.
	captureCtx, captureCancel := context.WithTimeout(ctx, 20*time.Second)
	region := cuVisualRegion(initial)
	raw, err := client.CallTool(captureCtx, "zoom", map[string]interface{}{
		"region": map[string]any{"x": region.Min.X, "y": region.Min.Y, "w": region.Dx(), "h": region.Dy()}, "factor": 1.0,
	})
	captureCancel()
	if err != nil {
		t.Fatal("real screenshot MCP call failed; native output suppressed")
	}
	if attest() != initial {
		t.Fatal("fixture identity or geometry changed during capture; image withheld")
	}
	imageBlock, err := cuVisualValidatedImage(raw, initial, challenge)
	if err != nil {
		t.Fatal(err) // every error is fixed text; no native/user pixels in logs
	}
	if err := client.Close(); err != nil {
		t.Fatal("real helper did not close cleanly; image withheld")
	}
	stageCredentials()

	// Provider construction deliberately occurs AFTER validation, and avoids
	// anonymous preconnect. No API call occurs for an unvalidated screenshot.
	built, err := metisruntime.BuildProviderWithoutPreconnect(minimal, provider, "")
	if err != nil {
		t.Fatal("METIS provider adapter construction failed; details suppressed")
	}
	response, err := built.Provider.Complete(ctx, llm.Request{
		Model: built.Model, SessionID: "cu-disposable-visual-acceptance", MaxTokens: 1024,
		System: "Read the image. Return only the eight-digit code, with no explanation or tools.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "text", Text: "Read the eight-digit code visible in the attached image. Return exactly eight digits."},
			imageBlock,
		}}},
	})
	if err != nil || response == nil {
		t.Fatal("METIS model request failed; API error and response suppressed")
	}
	var text strings.Builder
	for _, block := range response.Content {
		if block.Type == "tool_use" {
			t.Fatal("model requested an unavailable tool; no tool executed")
		}
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	matched := strings.TrimSpace(text.String()) == challenge
	t.Logf("real helper zoom(region, factor=1) -> MCP image -> METIS provider recognition: matched=%t; captures=1; not a CLI autonomous visual-loop test", matched)
	if !matched {
		t.Fatal("visual challenge did not match; expected code and model response suppressed")
	}
}

func cuVisualProviderConfig(t *testing.T, source *config.Config, name string) *config.Config {
	t.Helper()
	minimal := &config.Config{}
	minimal.Provider.Default = name
	minimal.Tools.Allowed = []string{"ToolSearch", "Skill", "mcp__computer-use__zoom"}
	switch name {
	case "openai-codex":
		minimal.Provider.OpenAICodex = source.Provider.OpenAICodex
	case "openai":
		minimal.Provider.OpenAI = source.Provider.OpenAI
		minimal.Provider.OpenAI.APIKey = ""
		minimal.Provider.OpenAI.APIKeyEnv = "METIS_CU_VALIDATION_API_KEY"
		minimal.Provider.OpenAI.HostedTools = nil
		minimal.Provider.OpenAI.PromptCacheKey = ""
	case "anthropic":
		minimal.Provider.Anthropic = source.Provider.Anthropic
		minimal.Provider.Anthropic.APIKey = ""
		minimal.Provider.Anthropic.APIKeyEnv = "METIS_CU_VALIDATION_API_KEY"
		minimal.Provider.Anthropic.AntiDistillation = false
		minimal.Provider.Anthropic.ClientSideDecoys = false
	default:
		raw, ok := source.Provider.Custom[name]
		if !ok {
			t.Fatal("selected visual provider is not an isolated API-key route")
		}
		switch raw.Transport {
		case "", "anthropic_messages", "openai_chat", "openai_responses":
		default:
			t.Fatal("visual acceptance supports only isolated Anthropic/OpenAI image transports")
		}
		raw.APIKey, raw.APIKeyEnv = "", "METIS_CU_VALIDATION_API_KEY"
		raw.HostedTools, raw.PromptCacheKey = nil, ""
		minimal.Provider.Custom = map[string]config.ProviderRaw{name: raw}
	}
	return minimal
}

type cuVisualAttestation struct {
	PID                    int    `json:"pid"`
	BundleID               string `json:"bundle_id"`
	WindowID               uint32 `json:"window_id"`
	DisplayID              uint32 `json:"display_id"`
	DisplayCount           int    `json:"display_count"`
	X                      int    `json:"x"`
	Y                      int    `json:"y"`
	Width                  int    `json:"width"`
	Height                 int    `json:"height"`
	Scale                  int    `json:"scale"`
	Frontmost              bool   `json:"frontmost"`
	OpaqueFullDisplay      bool   `json:"opaque_full_display"`
	ScreenCapturePreflight bool   `json:"screen_capture_preflight"`
}

func (a cuVisualAttestation) valid(pid int) bool {
	return pid > 0 && a.PID == pid && a.BundleID == "dev.metis.cu.visual-fixture" &&
		a.WindowID != 0 && a.DisplayID != 0 && a.DisplayCount == 1 && a.X == 0 && a.Y == 0 &&
		a.Width >= 800 && a.Height >= 300 && a.Width <= 4096 && a.Height <= 4096 &&
		a.Width*a.Height <= 8_000_000 && (a.Scale == 1 || a.Scale == 2) &&
		a.Frontmost && a.OpaqueFullDisplay && a.ScreenCapturePreflight
}

func cuVisualCodeValid(code string) bool {
	if len(code) != 8 {
		return false
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func cuVisualValidatedImage(raw []byte, a cuVisualAttestation, challenge string) (llm.ContentBlock, error) {
	refuse := func(message string) (llm.ContentBlock, error) { return llm.ContentBlock{}, errors.New(message) }
	if !a.valid(a.PID) || !cuVisualCodeValid(challenge) || len(raw) > 12*1024*1024 {
		return refuse("local visual validation inputs invalid; image withheld")
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &result) != nil || result.IsError || len(result.Content) != 2 {
		return refuse("real screenshot did not return the expected successful MCP content; image withheld")
	}
	var block llm.ContentBlock
	textCount := 0
	for _, content := range result.Content {
		switch content.Type {
		case "text":
			textCount++ // Native summary is deliberately never put in the prompt.
		case "image":
			if block.Type != "" || content.MIMEType != "image/png" {
				return refuse("real screenshot is not exactly one lossless PNG; image withheld")
			}
			block = llm.ContentBlock{Type: "image", MediaType: content.MIMEType, Data: content.Data}
		default:
			return refuse("unexpected MCP screenshot content; image withheld")
		}
	}
	if block.Type == "" || textCount != 1 {
		return refuse("missing real MCP screenshot image; image withheld")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(block.Data)
	if err != nil || len(payload) > 8*1024*1024 || !cuVisualPNGHasOnlyPixels(payload) {
		return refuse("real screenshot PNG payload invalid; image withheld")
	}
	dimensions, err := png.DecodeConfig(bytes.NewReader(payload))
	region := cuVisualRegion(a)
	if err != nil || dimensions.Width != region.Dx() || dimensions.Height != region.Dy() {
		return refuse("real capture geometry differs from the fixed fixture region; image withheld")
	}
	captured, err := png.Decode(bytes.NewReader(payload))
	if err != nil || !cuVisualPixelsMatch(captured, a, challenge) {
		if err == nil {
			// Only aggregate geometry/count diagnostics, never pixels, challenge
			// digits, or a saved screenshot. Upload remains strictly blocked.
			expected := cuVisualExpectedView(cuVisualRaster(a.Width, a.Height, 1, challenge), captured.Bounds(), a)
			mismatches := 0
			box := image.Rectangle{}
			for y := 0; y < captured.Bounds().Dy(); y++ {
				for x := 0; x < captured.Bounds().Dx(); x++ {
					r, g, b, alpha := captured.At(x, y).RGBA()
					er, eg, eb, ea := expected.At(x, y).RGBA()
					if r != er || g != eg || b != eb || alpha != ea {
						mismatches++
						box = box.Union(image.Rect(x, y, x+1, y+1))
					}
				}
			}
			return refuse(fmt.Sprintf("real screenshot differs from fixture; image withheld; raw-oracle mismatches=%d bounding-box=%v", mismatches, box))
		}
		return refuse("real screenshot cannot be decoded; image withheld")
	}
	return block, nil // exact original helper payload, never the expected raster
}

// The real helper's Go PNG encoder emits only these chunks. Reject metadata
// and trailing data as well as wrong pixels, so opaque PNG text/profile data
// cannot bypass the local image check while the original payload is forwarded.
func cuVisualPNGHasOnlyPixels(payload []byte) bool {
	if len(payload) < 8 || string(payload[:8]) != "\x89PNG\r\n\x1a\n" {
		return false
	}
	header, data := false, false
	for pos := 8; pos+12 <= len(payload); {
		size := uint64(binary.BigEndian.Uint32(payload[pos : pos+4]))
		if size+12 > uint64(len(payload)-pos) {
			return false
		}
		end := pos + int(size) + 12
		switch string(payload[pos+4 : pos+8]) {
		case "IHDR":
			if header || pos != 8 || size != 13 {
				return false
			}
			header = true
		case "IDAT":
			if !header {
				return false
			}
			data = true
		case "IEND":
			return header && data && size == 0 && end == len(payload)
		default:
			return false
		}
		pos = end
	}
	return false
}

// Only the comparison oracle is generated. It is never used as a screenshot,
// saved to a file, or forwarded to a provider. Require equality of every pixel;
// a border check or lossy tolerance could leak an overlay/desktop through it.
func cuVisualRegion(a cuVisualAttestation) image.Rectangle {
	const cell, margin = 12, 24
	left, top := (a.Width-(8*7-2)*cell)/2, (a.Height-7*cell)/2
	return image.Rect(left-margin, top-margin, left+(8*7-2)*cell+margin, top+7*cell+margin)
}

func cuVisualExpectedView(full image.Image, bounds image.Rectangle, a cuVisualAttestation) image.Image {
	if bounds == full.Bounds() {
		return full
	}
	region := cuVisualRegion(a)
	if bounds != image.Rect(0, 0, region.Dx(), region.Dy()) {
		return nil
	}
	dst := image.NewRGBA(bounds)
	xdraw.Copy(dst, image.Point{}, full, region, xdraw.Src, nil)
	return dst
}

func cuVisualPixelsMatch(captured image.Image, a cuVisualAttestation, challenge string) bool {
	if captured == nil || !a.valid(a.PID) || !cuVisualCodeValid(challenge) {
		return false
	}
	expected := cuVisualRaster(a.Width, a.Height, 1, challenge)
	view := cuVisualExpectedView(expected, captured.Bounds(), a)
	if view == nil {
		return false
	}
	if cuVisualImagesEqual(captured, view) {
		return true
	}
	if a.Scale == 1 {
		return false
	}
	// Native Screenshot may normalize a 2x capture using this same scaler.
	physicalGray := cuVisualRaster(a.Width, a.Height, a.Scale, challenge)
	// Match the native screenshot's RGBA source type as well as its scaler;
	// Gray and RGBA resampling paths can round edge pixels differently.
	physical := image.NewRGBA(physicalGray.Bounds())
	xdraw.Copy(physical, image.Point{}, physicalGray, physicalGray.Bounds(), xdraw.Src, nil)
	normalized := image.NewRGBA(expected.Bounds())
	xdraw.CatmullRom.Scale(normalized, normalized.Bounds(), physical, physical.Bounds(), xdraw.Over, nil)
	return cuVisualImagesEqual(captured, cuVisualExpectedView(normalized, captured.Bounds(), a))
}

func cuVisualImagesEqual(left, right image.Image) bool {
	if left.Bounds() != right.Bounds() {
		return false
	}
	for y := left.Bounds().Min.Y; y < left.Bounds().Max.Y; y++ {
		for x := left.Bounds().Min.X; x < left.Bounds().Max.X; x++ {
			lr, lg, lb, la := left.At(x, y).RGBA()
			rr, rg, rb, ra := right.At(x, y).RGBA()
			if lr != rr || lg != rg || lb != rb || la != ra {
				return false
			}
		}
	}
	return true
}

var cuVisualDigits = [10][7]string{
	{"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	{"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	{"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	{"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	{"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	{"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	{"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	{"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	{"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	{"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
}

func cuVisualRaster(width, height, scale int, challenge string) *image.Gray {
	const cell = 12
	oracle := image.NewGray(image.Rect(0, 0, width*scale, height*scale))
	left, top := (width-(8*7-2)*cell)/2, (height-7*cell)/2
	for index, digit := range challenge {
		for row, line := range cuVisualDigits[digit-'0'] {
			for column, bit := range line {
				if bit != '1' {
					continue
				}
				for y := (top + row*cell) * scale; y < (top+(row+1)*cell)*scale; y++ {
					for x := (left + (index*7+column)*cell) * scale; x < (left+(index*7+column+1)*cell)*scale; x++ {
						oracle.SetGray(x, y, color.Gray{Y: 255})
					}
				}
			}
		}
	}
	return oracle
}

// These are local safety-guard tests, not fabricated native/live evidence.
func TestComputerUseVisualPixelGuardFailClosed(t *testing.T) {
	a := cuVisualAttestation{PID: 7, BundleID: "dev.metis.cu.visual-fixture", WindowID: 1,
		DisplayID: 1, DisplayCount: 1, Width: 800, Height: 600, Scale: 1,
		Frontmost: true, OpaqueFullDisplay: true, ScreenCapturePreflight: true}
	const challenge = "12345678"
	oracle := cuVisualRaster(a.Width, a.Height, 1, challenge)
	if !cuVisualPixelsMatch(oracle, a, challenge) {
		t.Fatal("local expected-pixel oracle is inconsistent")
	}
	region := cuVisualRegion(a)
	crop := cuVisualExpectedView(oracle, image.Rect(0, 0, region.Dx(), region.Dy()), a).(*image.RGBA)
	if !cuVisualPixelsMatch(crop, a, challenge) {
		t.Fatal("fixed real-capture region must support the same exact-pixel check")
	}
	crop.SetRGBA(0, 0, color.RGBA{R: 1, A: 255})
	if cuVisualPixelsMatch(crop, a, challenge) {
		t.Fatal("one contaminated crop pixel must prevent upload")
	}
	for _, point := range []image.Point{{0, 0}, {799, 599}, {400, 300}} {
		previous := oracle.GrayAt(point.X, point.Y)
		oracle.SetGray(point.X, point.Y, color.Gray{Y: previous.Y ^ 1})
		if cuVisualPixelsMatch(oracle, a, challenge) {
			t.Fatal("one changed pixel must prevent model upload")
		}
		oracle.SetGray(point.X, point.Y, previous)
	}
	if cuVisualPixelsMatch(oracle, a, "12345679") {
		t.Fatal("wrong challenge must prevent model upload")
	}
	for _, mutate := range []func(*cuVisualAttestation){
		func(a *cuVisualAttestation) { a.BundleID = "other.app" },
		func(a *cuVisualAttestation) { a.DisplayCount = 2 },
		func(a *cuVisualAttestation) { a.Frontmost = false },
		func(a *cuVisualAttestation) { a.OpaqueFullDisplay = false },
		func(a *cuVisualAttestation) { a.ScreenCapturePreflight = false },
		func(a *cuVisualAttestation) { a.X = 1 },
		func(a *cuVisualAttestation) { a.Scale = 3 },
	} {
		invalid := a
		mutate(&invalid)
		if cuVisualPixelsMatch(oracle, invalid, challenge) {
			t.Fatal("uncertain fixture attestation must prevent model upload")
		}
	}
	// Exercise the complete MCP envelope validator too. Local synthetic inputs
	// are only guard unit tests, never evidence of a passing native capture.
	envelope := func(img image.Image) []byte {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "local unit test"},
			map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(encoded.Bytes())},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cleanCrop := cuVisualExpectedView(oracle, image.Rect(0, 0, region.Dx(), region.Dy()), a)
	if _, err := cuVisualValidatedImage(envelope(cleanCrop), a, challenge); err != nil {
		t.Fatalf("valid local MCP image refused: %v", err)
	}
	if _, err := cuVisualValidatedImage(envelope(crop), a, challenge); err == nil {
		t.Fatal("complete envelope validation must not allow a contaminated capture")
	}
}
