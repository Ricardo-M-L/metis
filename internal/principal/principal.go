// Package principal persists jobs independently of conversational sessions.
// A job has an immutable execution definition; changing a profile on disk does
// not change work that has already been assigned to that job.
package principal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// Version 2 moves settled history behind an immutable archive root. Version 1
// binaries must reject it rather than dropping the archived lifetime spend.
const SchemaVersion = 2

// HotRunHistoryLimit bounds recent settled summaries in the job manifest.
// Active, blocked and unsettled runs are never evicted to meet this limit.
const HotRunHistoryLimit = 64

var (
	ErrNotFound     = errors.New("persistent job not found")
	ErrInactive     = errors.New("persistent job is paused or retired")
	ErrBusy         = errors.New("persistent job already has an active execution")
	ErrLeaseLost    = errors.New("persistent job execution lease lost")
	ErrBudget       = errors.New("persistent job lifetime budget is exhausted")
	ErrUsageUnknown = usageUnresolvedError{}
)

type usageUnresolvedError struct{}

func (usageUnresolvedError) Error() string {
	return "persistent job has unresolved provider usage; reconcile its request receipts before dispatching more work"
}
func (usageUnresolvedError) UsageFailureCode() string { return "provider_usage_unresolved" }

type Status string

const (
	Active  Status = "active"
	Paused  Status = "paused"
	Retired Status = "retired"
)

// Profile contains execution-affecting profile fields, never an API key.
type Profile struct {
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Model           string   `json:"model"`
	Tools           []string `json:"tools,omitempty"`
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	PermissionMode  string   `json:"permission_mode"`
	Effort          string   `json:"effort,omitempty"`
	MaxTurns        int      `json:"max_turns,omitempty"`
	InitialPrompt   string   `json:"initial_prompt,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	OmitClaudeMd    bool     `json:"omit_claude_md,omitempty"`
	SystemPrompt    string   `json:"system_prompt,omitempty"`
	Source          string   `json:"source"`
}

type CredentialRef struct {
	Kind string `json:"kind"` // env or provider_account; values stay in the credential store
	Name string `json:"name"`
}

type Budget struct {
	MaxUSD          float64 `json:"max_usd,omitempty"`
	MaxInputTokens  int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64   `json:"max_output_tokens,omitempty"`
}

type Definition struct {
	Workspace            string          `json:"workspace"`
	Provider             string          `json:"provider"`
	ProviderConfigSHA256 string          `json:"provider_config_sha256"`
	Profile              Profile         `json:"profile"`
	Credentials          []CredentialRef `json:"credential_refs,omitempty"`
	Budget               Budget          `json:"budget"`
}

// Binding is stored in every assigned task. The hash covers the entire role,
// permission, route, credential-reference and budget definition.
type Binding struct {
	JobID        string     `json:"job_id"`
	ConfigSHA256 string     `json:"config_sha256"`
	Definition   Definition `json:"definition"`
}

type Lease struct {
	Token       string    `json:"token"`
	ExecutionID string    `json:"execution_id"`
	Worker      string    `json:"worker"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// RunUsage holds only accounting and lifecycle summaries. Full results and
// operation receipts stay in the independently rotated coordinator journal.
type RunUsage struct {
	RunID              string    `json:"run_id"`
	SourceRevision     uint64    `json:"source_revision"`
	Status             string    `json:"status"`
	InputTokens        int64     `json:"input_tokens"`
	OutputTokens       int64     `json:"output_tokens"`
	CacheReadTokens    int64     `json:"cache_read_tokens"`
	CacheWriteTokens   int64     `json:"cache_write_tokens"`
	SpentUSD           float64   `json:"spent_usd"`
	UnresolvedRequests int       `json:"unresolved_requests,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// RunHistory is a content-addressed, immutable index of older settled runs.
// Its totals are cumulative, and remain part of all lifetime budget checks.
type RunHistory struct {
	SHA256       string  `json:"sha256,omitempty"`
	Count        uint64  `json:"count,omitempty"`
	SpentUSD     float64 `json:"spent_usd,omitempty"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
}

type Job struct {
	Version            int        `json:"version"`
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Status             Status     `json:"status"`
	ConfigSHA256       string     `json:"config_sha256"`
	Definition         Definition `json:"definition"`
	SessionID          string     `json:"session_id"`
	SessionDir         string     `json:"session_dir"`
	MemoryDir          string     `json:"memory_dir"`
	Lease              *Lease     `json:"lease,omitempty"`
	Runs               []RunUsage `json:"runs,omitempty"`
	RunHistory         RunHistory `json:"run_history,omitempty"`
	SpentUSD           float64    `json:"spent_usd"`
	InputTokens        int64      `json:"input_tokens"`
	OutputTokens       int64      `json:"output_tokens"`
	UnresolvedRequests int        `json:"unresolved_requests,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// RunCount includes the archived history as well as the hot manifest.
func (j Job) RunCount() uint64 { return j.RunHistory.Count + uint64(len(j.Runs)) }

func (j Job) Binding() Binding {
	b := Binding{JobID: j.ID, ConfigSHA256: j.ConfigSHA256, Definition: j.Definition}
	data, _ := json.Marshal(b)
	var independent Binding
	_ = json.Unmarshal(data, &independent)
	return independent
}

func HashDefinition(d Definition) (string, error) {
	if err := ValidateDefinition(d); err != nil {
		return "", err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateBinding(b *Binding, workspace string) error {
	if b == nil {
		return nil
	}
	if !validID(b.JobID) {
		return errors.New("invalid persistent job identity")
	}
	digest, err := HashDefinition(b.Definition)
	if err != nil {
		return err
	}
	if digest != b.ConfigSHA256 {
		return errors.New("persistent job configuration snapshot hash mismatch")
	}
	if workspace != "" && b.Definition.Workspace != workspace {
		return errors.New("persistent job workspace does not match task workspace")
	}
	return nil
}

func ValidateDefinition(d Definition) error {
	if !filepath.IsAbs(d.Workspace) || filepath.Clean(d.Workspace) != d.Workspace {
		return errors.New("persistent job workspace must be canonical and absolute")
	}
	if strings.TrimSpace(d.Provider) == "" || len(d.Provider) > 128 {
		return errors.New("persistent job provider is required")
	}
	if len(d.ProviderConfigSHA256) != 64 {
		return errors.New("persistent job provider configuration digest is required")
	}
	if _, err := hex.DecodeString(d.ProviderConfigSHA256); err != nil {
		return errors.New("invalid provider configuration digest")
	}
	if strings.TrimSpace(d.Profile.Name) == "" || strings.TrimSpace(d.Profile.Model) == "" {
		return errors.New("persistent job profile name and model must be frozen")
	}
	switch d.Profile.PermissionMode {
	case "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions", "fullAccess":
	default:
		return errors.New("persistent job permission mode must be explicit")
	}
	switch d.Profile.Source {
	case "project", "user", "builtin":
	default:
		return errors.New("invalid profile trust provenance")
	}
	if d.Profile.MaxTurns < 0 || d.Profile.MaxTurns > 100000 {
		return errors.New("invalid persistent job turn limit")
	}
	if !validUSD(d.Budget.MaxUSD) || d.Budget.MaxInputTokens < 0 || d.Budget.MaxOutputTokens < 0 {
		return errors.New("invalid persistent job lifetime budget")
	}
	for _, ref := range d.Credentials {
		if ref.Kind != "env" && ref.Kind != "provider_account" {
			return errors.New("credential reference must name an env or provider account")
		}
		if ref.Kind == "env" && !validEnv(ref.Name) {
			return errors.New("credential environment reference must be a variable name, never a secret value")
		}
		if ref.Kind == "provider_account" && ref.Name != d.Provider {
			return errors.New("credential account reference must match the bound provider")
		}
	}
	if data, err := json.Marshal(d); err != nil || len(data) > 512<<10 {
		return errors.New("persistent job definition exceeds 512 KiB")
	}
	return nil
}

func validEnv(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func validUSD(x float64) bool { return x >= 0 && !math.IsNaN(x) && !math.IsInf(x, 0) }
func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("persistent job random identity: %v", err))
	}
	return prefix + hex.EncodeToString(b[:])
}

func (j Job) exhausted() bool {
	b := j.Definition.Budget
	return b.MaxUSD > 0 && j.SpentUSD >= b.MaxUSD || b.MaxInputTokens > 0 && j.InputTokens >= b.MaxInputTokens || b.MaxOutputTokens > 0 && j.OutputTokens >= b.MaxOutputTokens
}

func (j Job) RemainingUSD() float64 {
	if j.Definition.Budget.MaxUSD == 0 {
		return 0
	}
	return math.Max(0, j.Definition.Budget.MaxUSD-j.SpentUSD)
}
