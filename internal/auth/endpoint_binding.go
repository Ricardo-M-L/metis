package auth

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var (
	// ErrEndpointBindingRequired means a legacy managed key has no endpoint
	// metadata and therefore cannot safely be used with a custom endpoint.
	ErrEndpointBindingRequired = errors.New("stored API key requires endpoint binding")
	// ErrEndpointBindingMismatch means the managed key was authorized for a
	// different provider, transport, host, or path.
	ErrEndpointBindingMismatch = errors.New("stored API key endpoint binding does not match current provider configuration")
)

// EndpointBinding records the endpoint identity a managed LLM API key may be
// sent to. Model is intentionally excluded: rotating models behind the same
// authenticated API endpoint must not require another login.
type EndpointBinding struct {
	Provider  string `json:"provider"`
	Transport string `json:"transport"`
	BaseURL   string `json:"base_url"`
}

// NormalizeEndpointBinding returns a stable endpoint identity. It treats the
// default port and a trailing slash as spelling differences while preserving
// transport, host, and non-trailing path changes as distinct trust boundaries.
func NormalizeEndpointBinding(provider, transport, baseURL string) (EndpointBinding, error) {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return EndpointBinding{}, fmt.Errorf("auth endpoint binding: %w", err)
	}
	transport, err = normalizeEndpointTransport(transport)
	if err != nil {
		return EndpointBinding{}, err
	}
	baseURL, err = normalizeEndpointBaseURL(baseURL)
	if err != nil {
		return EndpointBinding{}, err
	}
	return EndpointBinding{Provider: provider, Transport: transport, BaseURL: baseURL}, nil
}

func normalizeEndpointTransport(transport string) (string, error) {
	transport = strings.ToLower(strings.TrimSpace(transport))
	switch transport {
	case "anthropic", "anthropic_messages":
		return "anthropic_messages", nil
	case "openai", "chat", "openai_chat":
		return "openai_chat", nil
	case "responses", "openai_responses":
		return "openai_responses", nil
	case "gemini", "gemini_native":
		return "gemini_native", nil
	case "azure", "azure_openai":
		return "azure_openai", nil
	case "vertex_anthropic", "bedrock_anthropic":
		return transport, nil
	case "":
		// Historical custom profiles omitted transport and have always been
		// interpreted by the runtime as Anthropic-compatible Messages. Preserve
		// that identity in the credential binding so a freshly stored key can be
		// resolved by the same legacy profile.
		return "anthropic_messages", nil
	default:
		return "", fmt.Errorf("auth endpoint binding: unsupported transport %q", transport)
	}
}

func normalizeEndpointBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.IsAbs() == false || u.Opaque != "" || u.Hostname() == "" {
		return "", errors.New("auth endpoint binding: base URL must be an absolute HTTP(S) URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("auth endpoint binding: base URL must use HTTP(S)")
	}
	if u.Scheme == "http" && !isLoopbackEndpointHost(u.Hostname()) {
		return "", errors.New("auth endpoint binding: plain HTTP is allowed only for localhost or loopback IP addresses")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("auth endpoint binding: base URL must not contain credentials, query, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		if port != "" {
			host = net.JoinHostPort(host, port)
		} else {
			host = "[" + host + "]"
		}
	} else if port != "" {
		host = net.JoinHostPort(host, port)
	}
	path := u.EscapedPath()
	if !strings.HasSuffix(path, "//") {
		// Normalize a conventional single trailing separator only. Repeated
		// slashes can affect routing, so preserve them on every normalization.
		path = strings.TrimSuffix(path, "/")
	}
	return u.Scheme + "://" + host + path, nil
}

func isLoopbackEndpointHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
