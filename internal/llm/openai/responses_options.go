package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// ResponsesStateMode controls who owns multi-turn state. Local is the safe
// cross-provider default; provider uses store + previous_response_id; auto
// enables provider state only for a capability profile that declares it.
type ResponsesStateMode string

const (
	ResponsesStateLocal    ResponsesStateMode = "local"
	ResponsesStateProvider ResponsesStateMode = "provider"
	ResponsesStateAuto     ResponsesStateMode = "auto"
)

// ConfigureStateMode validates user configuration instead of silently
// interpreting a typo as local state.
func (r *Responses) ConfigureStateMode(mode string) error {
	switch normalized := ResponsesStateMode(strings.ToLower(strings.TrimSpace(mode))); normalized {
	case "", ResponsesStateLocal:
		r.StateMode = ResponsesStateLocal
	case ResponsesStateProvider, ResponsesStateAuto:
		r.StateMode = normalized
	default:
		return fmt.Errorf("unknown Responses state mode %q (want local, provider, or auto)", mode)
	}
	return nil
}

const (
	responsesHintResponseID = "openai.responses.response_id"
	responsesHintStateKey   = "openai.responses.state_key"
	responsesHintItemID     = "openai.responses.item_id"
)

// ResponsesCapabilities describes optional parts of the Responses protocol.
// Compatibility endpoints vary substantially; the conservative profile only
// enables the common text/function/SSE subset.
type ResponsesCapabilities struct {
	StatefulResponses  bool
	EncryptedReasoning bool
	PromptCaching      bool
	Images             bool
	StructuredOutputs  bool
	HostedTools        bool
}

// ConfigureCapabilityProfile replaces URL inference with an explicit profile.
// "compatible" is intentionally conservative for gateways that implement
// only the common text/function/SSE subset.
func (r *Responses) ConfigureCapabilityProfile(profile string) error {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "auto":
		r.Capabilities = detectResponsesCapabilities(r.BaseURL)
	case "openai":
		r.Capabilities = ResponsesCapabilities{
			StatefulResponses:  true,
			EncryptedReasoning: true,
			PromptCaching:      true,
			Images:             true,
			StructuredOutputs:  true,
			HostedTools:        true,
		}
	case "compatible":
		r.Capabilities = ResponsesCapabilities{}
	default:
		return fmt.Errorf("unknown Responses capability profile %q (want auto, openai, or compatible)", profile)
	}
	return nil
}

func detectResponsesCapabilities(baseURL string) ResponsesCapabilities {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ResponsesCapabilities{}
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimRight(strings.ToLower(u.Path), "/")
	switch {
	case host == "api.openai.com":
		return ResponsesCapabilities{
			StatefulResponses:  true,
			EncryptedReasoning: true,
			PromptCaching:      true,
			Images:             true,
			StructuredOutputs:  true,
			HostedTools:        true,
		}
	case host == "api.x.ai", host == "mtls.api.x.ai":
		return ResponsesCapabilities{StatefulResponses: true, Images: true, StructuredOutputs: true}
	case (host == "open.bigmodel.cn" || host == "api.z.ai") && strings.HasSuffix(path, "/api/v1"):
		// GLM Coding Plan's /api/v1 surface is Responses-only. Live probes
		// confirm stored continuation, prompt_cache_key/cached_tokens and the
		// provider-hosted web_search tool. Its text.format JSON Schema field is
		// currently accepted but not enforced, so StructuredOutputs stays false.
		return ResponsesCapabilities{StatefulResponses: true, PromptCaching: true, HostedTools: true}
	case strings.Contains(host, "openrouter.ai"):
		// OpenRouter exposes the Responses request/response shape, but its
		// public create schema currently accepts only store=false. In
		// particular, previous_response_id is not a promise that OpenRouter
		// retained the response for tail-only continuation. Keep state local so
		// auto mode never drops history or sends the rejected store=true value.
		return ResponsesCapabilities{PromptCaching: true, Images: true, StructuredOutputs: true, HostedTools: true}
	case strings.Contains(host, "maas.aliyuncs.com"), strings.Contains(host, "dashscope.aliyuncs.com"):
		// Alibaba's session-cache switch is a vendor header rather than the
		// OpenAI prompt_cache_key field, so do not claim generic prompt caching.
		return ResponsesCapabilities{StatefulResponses: true, Images: true, HostedTools: true}
	case strings.Contains(host, "volces.com"):
		return ResponsesCapabilities{StatefulResponses: true, Images: true, HostedTools: true}
	case strings.Contains(host, "fireworks.ai"):
		// Fireworks documents state, function tools and MCP/SSE server tools,
		// but not the parameter-free web_search tool Metis currently exposes.
		return ResponsesCapabilities{StatefulResponses: true}
	case strings.Contains(host, "amazonaws.com"), strings.HasSuffix(host, ".api.aws"):
		// Bedrock Runtime explicitly excludes server-side tools such as web
		// search. Keep the shared Responses fields while avoiding a false claim.
		return ResponsesCapabilities{StatefulResponses: true, Images: true, StructuredOutputs: true}
	default:
		return ResponsesCapabilities{}
	}
}

func (r *Responses) effectiveStateMode() ResponsesStateMode {
	switch r.StateMode {
	case ResponsesStateProvider:
		return ResponsesStateProvider
	case ResponsesStateAuto:
		if r.Capabilities.StatefulResponses {
			return ResponsesStateProvider
		}
	}
	return ResponsesStateLocal
}

func (r *Responses) stateKey() string {
	sum := sha256.Sum256([]byte(strings.TrimRight(r.BaseURL, "/") + "\x00" + r.Model))
	return hex.EncodeToString(sum[:16])
}

func (r *Responses) previousResponse(messages []provider.Message) (string, []provider.Message) {
	if r.effectiveStateMode() != ResponsesStateProvider {
		return "", messages
	}
	key := r.stateKey()
	for i := len(messages) - 1; i >= 0; i-- {
		for _, block := range messages[i].Content {
			if block.Type != "provider_state" || block.ProviderHint[responsesHintStateKey] != key {
				continue
			}
			if id := block.ProviderHint[responsesHintResponseID]; id != "" {
				return id, messages[i+1:]
			}
		}
	}
	return "", messages
}

// buildStateRecoveryRequest falls back to a full local replay when a provider
// has evicted a previous_response_id. Store remains enabled so the successful
// recovery establishes a fresh continuation checkpoint for later turns.
func (r *Responses) buildStateRecoveryRequest(req provider.Request) (*responsesRequest, error) {
	fallback := *r
	fallback.StateMode = ResponsesStateLocal
	fallback.Capabilities.EncryptedReasoning = false
	// The fallback replays local history, but its successful response becomes
	// the next provider-managed checkpoint. Keep volatile state out of that
	// newly stored input chain just like a normal provider continuation.
	body, err := fallback.buildResponsesRequestWithVolatilePlacement(req, true)
	if err != nil {
		return nil, err
	}
	body.PreviousResponseID = ""
	body.Store = true
	return body, nil
}

func isMissingPreviousResponse(raw string) bool {
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(envelope.Error.Code))
	message := strings.ToLower(strings.TrimSpace(envelope.Error.Message))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(envelope.Code))
		message = strings.ToLower(strings.TrimSpace(envelope.Message))
	}
	if code == "previous_response_not_found" {
		return true
	}
	return code == "not_found" &&
		(strings.Contains(message, "previous_response_id") ||
			strings.Contains(message, "previous response"))
}

func stablePromptCacheKey(model, instructions string, tools []provider.ToolSpec) string {
	payload, _ := json.Marshal(struct {
		Model        string
		Instructions string
		Tools        []provider.ToolSpec
	}{model, instructions, tools})
	sum := sha256.Sum256(payload)
	return "metis-" + hex.EncodeToString(sum[:16])
}
