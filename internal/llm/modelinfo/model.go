// Package modelinfo discovers model capabilities from the configured endpoint.
// Transport names and public model catalog entries are not evidence that a
// custom endpoint exposes the same limits.
package modelinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path"
	"strings"
	"unicode"
)

const (
	AuthBearer                  = "bearer"
	AuthAPIKey                  = "x-api-key"
	AuthAPIKeyBearer            = "x-api-key+bearer"
	SourceEndpointPI            = "endpoint_pi"
	SourceEndpointModels        = "endpoint_models"
	SourceEndpointMerged        = "endpoint_pi+endpoint_models"
	WindowKindCapacity          = "capacity"
	WindowKindClientProfile     = "client_profile"
	LimitEvidenceServedCapacity = "served_capacity"
	AssocEvidenceCapacityWindow = "capacity_window"
	AssocEvidenceDeclaredServed = "declared_served"
)

// AssociatedLimits retain constraints declared by an ordinary listing for the
// same selection or one uniquely associated wire alias. Client profile windows
// remain separate. Evidence applies to individual fields: a capacity window
// proves only total limits; explicit served_capacity metadata proves the group.
type AssociatedLimits struct {
	ContextWindow    int    `json:"context_window,omitempty"`
	MaxContextWindow int    `json:"max_context_window,omitempty"`
	MaxInputTokens   int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	Source           string `json:"source"`
	ModelID          string `json:"model_id"`
	Evidence         string `json:"evidence"`
}

// Model keeps the endpoint's default window separate from its optional maximum
// window and independent input/output limits. Zero means the endpoint did not
// report that value.
type Model struct {
	ID               string           `json:"id"`
	Name             string           `json:"name,omitempty"`
	DisplayName      string           `json:"display_name,omitempty"`
	OwnedBy          string           `json:"owned_by,omitempty"`
	OwnerSource      string           `json:"owner_source,omitempty"`
	OwnerModelID     string           `json:"owner_model_id,omitempty"`
	LimitEvidence    string           `json:"limit_evidence,omitempty"`
	AssocLimits      AssociatedLimits `json:"assoc_limits,omitzero"`
	ContextWindow    int              `json:"context_window,omitempty"`
	MaxContextWindow int              `json:"max_context_window,omitempty"`
	MaxInputTokens   int              `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int              `json:"max_output_tokens,omitempty"`
	Source           string           `json:"source"`
	// Client profiles are window policies reported by a client catalog, rather
	// than a claim about the model's complete capacity. Empty means capacity.
	WindowKind string `json:"window_kind,omitempty"`
	// Duplicate raw records cannot establish a unique cross-ID association.
	// This is used only while merging freshly discovered endpoint responses.
	aliasAmbiguous bool
}

// Binding is the model's route identity. Secrets belong only to Discover's
// per-call argument, never to this structure or its cache key.
type Binding struct {
	ProviderID string `json:"provider_id"`
	Transport  string `json:"transport,omitempty"`
	BaseURL    string `json:"base_url"`
	AuthKind   string `json:"-"`
}

var (
	ErrInvalidBinding   = errors.New("model discovery: invalid endpoint binding")
	ErrInvalidResponse  = errors.New("model discovery: invalid capability response")
	ErrResponseTooLarge = errors.New("model discovery: capability response exceeds limit")
	ErrTooManyModels    = errors.New("model discovery: model count exceeds limit")
)

func validLabel(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

func normalizeBinding(b Binding) (Binding, string, error) {
	if !validLabel(b.ProviderID, 256) || (b.Transport != "" && !validLabel(b.Transport, 256)) {
		return Binding{}, "", ErrInvalidBinding
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Binding{}, "", ErrInvalidBinding
	}
	if !validLabel(u.Host, 512) || strings.ContainsAny(u.Path, "\\\r\n") || strings.ContainsFunc(u.Path, unicode.IsControl) {
		return Binding{}, "", ErrInvalidBinding
	}
	// Escaped path separators and traversal are not equivalent endpoint paths.
	// Reject them instead of accidentally normalizing onto another route.
	if u.RawPath != "" || strings.Contains(u.Path, "//") {
		return Binding{}, "", ErrInvalidBinding
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return Binding{}, "", ErrInvalidBinding
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = strings.TrimRight(path.Clean("/"+strings.TrimPrefix(u.Path, "/")), "/")
	u.RawPath = ""
	b.BaseURL = u.String()
	// AuthKind only selects request headers, and does not determine capability.
	if b.AuthKind != "" && b.AuthKind != AuthBearer && b.AuthKind != AuthAPIKey && b.AuthKind != AuthAPIKeyBearer {
		return Binding{}, "", ErrInvalidBinding
	}
	digest := sha256.Sum256([]byte(b.ProviderID + "\x00" + b.Transport + "\x00" + b.BaseURL))
	return b, hex.EncodeToString(digest[:]), nil
}

// NormalizeBinding exposes the same identity rules used by Get and Refresh.
func NormalizeBinding(b Binding) (Binding, error) {
	n, _, err := normalizeBinding(b)
	return n, err
}

// CanonicalBinding is the route identity shared by UI discovery and runtime.
func CanonicalBinding(b Binding) (Binding, error) { return NormalizeBinding(b) }

func modelsURL(b Binding, rich bool) string {
	u, _ := url.Parse(b.BaseURL)
	if u.Path == "" || b.Transport == "anthropic_messages" || b.Transport == "anthropic" {
		u.Path += "/v1/models"
	} else {
		u.Path += "/models"
	}
	if rich {
		u.RawQuery = "client_version=pi"
	}
	return u.String()
}
