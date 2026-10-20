package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

type CLIProxyGUIConfig struct {
	Database, SQLiteExecutable, InferenceEndpoint, LedgerProvider string
}

// CLIProxyGUI reads the desktop collector's existing SQLite records without
// consuming the gateway's destructive telemetry queue or changing its config.
// These records establish token usage only. They never establish actual USD.
type CLIProxyGUI struct{ database, sqlite, provider, endpoint string }

var usageClientID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func NewCLIProxyGUI(config CLIProxyGUIConfig) (*CLIProxyGUI, error) {
	u, err := url.Parse(config.InferenceEndpoint)
	if err != nil {
		return nil, failure("invalid_inference_endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
		return nil, failure("cliproxy_requires_loopback_endpoint")
	}
	endpoint, err := EndpointSHA256(config.InferenceEndpoint)
	if err != nil {
		return nil, err
	}
	if config.LedgerProvider == "" || !filepath.IsAbs(config.Database) || !filepath.IsAbs(config.SQLiteExecutable) {
		return nil, failure("invalid_cliproxy_configuration")
	}
	return &CLIProxyGUI{database: config.Database, sqlite: config.SQLiteExecutable, provider: config.LedgerProvider, endpoint: endpoint}, nil
}

type boundedEvidence struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *boundedEvidence) Write(data []byte) (int, error) {
	if w.Len()+len(data) > EvidenceLimit {
		w.cancel()
		return 0, failure("evidence_size_limit")
	}
	return w.Buffer.Write(data)
}

func (s *CLIProxyGUI) Lookup(parent context.Context, request Request) (Observation, error) {
	if err := bind(request, s.provider, s.endpoint); err != nil {
		return Observation{}, err
	}
	// The gateway generates its own request ID and may not return it in HTTP
	// headers. The transport supplies the reserved 128-bit physical request ID
	// as an exact User-Agent token; the GUI persists it with that request.
	id := request.ProviderRequestID
	if len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return Observation{}, failure("invalid_request_id")
	}
	if !usageClientID.MatchString(request.ClientRequestID) {
		return Observation{}, failure("invalid_client_request_id")
	}
	marker := "metis-usage/" + request.ClientRequestID
	query := `SELECT event_key,request_id,provider,model,alias,user_agent,failed,canceled,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens FROM usage_events WHERE instr(' ' || coalesce(user_agent,'') || ' ', ' ` + marker + ` ') > 0 LIMIT 2;`
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, s.sqlite, "-init", os.DevNull, "-batch", "-readonly", "-json", s.database, query)
	// The helper reads no user startup file or auth environment and accepts no
	// SQL from the model. Only the exact escaped request identifier is selected.
	command.Env = []string{"HOME=" + filepath.Dir(s.database), "SQLITE_HISTORY=" + os.DevNull}
	command.Stderr = io.Discard
	output := &boundedEvidence{cancel: cancel}
	command.Stdout = output
	if err := command.Run(); err != nil {
		if parent.Err() != nil {
			return Observation{}, parent.Err()
		}
		return Observation{}, failure("cliproxy_read_failed")
	}
	raw := output.Bytes()
	if len(bytes.TrimSpace(raw)) == 0 {
		return Observation{}, ErrPending
	}
	var rows []struct {
		EventKey   string `json:"event_key"`
		RequestID  string `json:"request_id"`
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		Alias      string `json:"alias"`
		UserAgent  string `json:"user_agent"`
		Failed     *int   `json:"failed"`
		Canceled   *int   `json:"canceled"`
		Input      *int   `json:"input_tokens"`
		Output     *int   `json:"output_tokens"`
		CacheRead  *int   `json:"cache_read_tokens"`
		CacheWrite *int   `json:"cache_creation_tokens"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return Observation{}, failure("invalid_cliproxy_record")
	}
	if len(rows) == 0 {
		return Observation{}, ErrPending
	}
	if len(rows) != 1 {
		return Observation{}, failure("ambiguous_cliproxy_request")
	}
	r := rows[0]
	markers := 0
	for _, token := range strings.Fields(r.UserAgent) {
		if token == marker {
			markers++
		}
	}
	if r.EventKey == "" || r.RequestID == "" || (id != "" && r.RequestID != id) || markers != 1 || (r.Model != request.Model && r.Alias != request.Model) {
		return Observation{}, failure("cliproxy_scope_mismatch")
	}
	if r.Failed == nil || r.Canceled == nil || r.Input == nil || r.Output == nil || r.CacheRead == nil || r.CacheWrite == nil {
		return Observation{}, ErrPending
	}
	if *r.Failed != 0 || *r.Canceled != 0 {
		return Observation{}, ErrPending
	}
	if r.Provider != "codex" && r.Provider != "openai" {
		return Observation{}, failure("unsupported_cliproxy_token_semantics")
	}
	// The collector stores input inclusive of cache. Do not count those tokens
	// twice in METIS's exclusive token categories.
	if *r.CacheRead < 0 || *r.CacheWrite < 0 || *r.Input < *r.CacheRead || *r.Input-*r.CacheRead < *r.CacheWrite || *r.Output < 0 {
		return Observation{}, failure("invalid_token_counts")
	}
	request.ProviderRequestID = r.RequestID
	return observation(request, "cliproxy-gui/persisted-usage", projectcoord.TokenUsage{Input: *r.Input - *r.CacheRead - *r.CacheWrite, Output: *r.Output, CacheRead: *r.CacheRead, CacheWrite: *r.CacheWrite}, nil, raw)
}
