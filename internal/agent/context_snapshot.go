package agent

import (
	"reflect"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// activeContextSnapshot is the provider-authoritative size of the active
// context window after one completed response. It is deliberately separate
// from EventTokens/Budget accounting: those are cumulative spend counters,
// while this value replaces the previous response snapshot on every round
// trip.
//
// messageCount anchors the snapshot immediately after the assistant response
// was appended. Messages added after that point (normally tool results and the
// next user prompt) are estimated exactly once until the next response
// supplies a fresh provider snapshot.
type activeContextSnapshot struct {
	valid           bool
	tokens          int
	messageCount    int
	historyRevision uint64
	routingRevision uint64
	requestOverhead int
	provider        contextProviderIdentity
	requestModel    string
}

// contextProviderIdentity distinguishes both a logical provider/model and a
// rebuilt provider instance. The instance component matters when two clients
// share the same public name/model but point at different transports or
// endpoints.
type contextProviderIdentity struct {
	name         string
	model        string
	concreteType reflect.Type
	instance     uintptr
}

type contextRequestAnchor struct {
	valid           bool
	provider        llm.Provider
	providerID      contextProviderIdentity
	requestModel    string
	messageCount    int
	historyRevision uint64
	routingRevision uint64
	requestOverhead int
	maxContext      int
}

func identifyContextProvider(provider llm.Provider) contextProviderIdentity {
	if provider == nil {
		return contextProviderIdentity{}
	}
	value := reflect.ValueOf(provider)
	identity := contextProviderIdentity{
		name:         provider.Name(),
		model:        provider.ModelID(),
		concreteType: value.Type(),
	}
	// Providers are conventionally pointers. Keep the helper safe for external
	// plugins implemented as value types as well; their logical identity still
	// includes concrete type, name and wire model.
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer,
		reflect.Slice, reflect.UnsafePointer:
		if !value.IsNil() {
			identity.instance = value.Pointer()
		}
	}
	return identity
}

func (l *Loop) contextRequestAnchorLocked(provider llm.Provider, req llm.Request) contextRequestAnchor {
	if l == nil || provider == nil {
		return contextRequestAnchor{}
	}
	overhead := estimateRequestOverhead(req)
	// Keep the render/preflight cache synchronized with the request that was
	// actually assembled. Dynamic runtime state, plan text, memory and lazy
	// tool schemas can all change between two turns; inheriting the previous
	// request's value makes the provider usage anchor internally inconsistent.
	l.requestOverheadTokens.Store(int64(overhead))
	return contextRequestAnchor{
		valid:           true,
		provider:        provider,
		providerID:      identifyContextProvider(provider),
		requestModel:    req.Model,
		messageCount:    len(req.Messages),
		historyRevision: l.historyRevision,
		routingRevision: l.routingRevision,
		requestOverhead: overhead,
		maxContext:      provider.MaxContextTokens(),
	}
}

// estimateRequestOverhead returns the non-history portion of one concrete
// provider request. Typed system sections replace (rather than supplement)
// the flattened System string on providers that support them, matching the
// wire encoders and avoiding a second copy in the estimate.
func estimateRequestOverhead(req llm.Request) int {
	overhead := 0
	if len(req.SystemSections) > 0 {
		for _, section := range req.SystemSections {
			overhead += 4 + estimateStringTokens(section.Body)
		}
	} else {
		overhead += estimateStringTokens(req.System)
	}
	for _, spec := range req.Tools {
		overhead += estimateSpecTokens(spec)
	}
	return overhead
}

func (l *Loop) invalidateActiveContextLocked() {
	l.activeContext = activeContextSnapshot{}
}

func disjointPromptUsageTokens(usage *usageTotals) (int, bool) {
	if usage == nil {
		return 0, false
	}
	input := max(usage.in, 0)
	cacheCreate := max(usage.cacheCreate, 0)
	cacheRead := max(usage.cacheRead, 0)
	prompt := input + cacheCreate + cacheRead
	// Output-only reports are not a usable context snapshot: several
	// compatibility gateways omit prompt usage. Preserve the full-history
	// estimator for that case instead of making the UI collapse toward zero.
	if prompt == 0 {
		return 0, false
	}
	return prompt, true
}

func disjointUsageTokens(usage *usageTotals) (int, bool) {
	prompt, ok := disjointPromptUsageTokens(usage)
	if !ok {
		return 0, false
	}
	return prompt + max(usage.out, 0), true
}

func usageClearlyUnderreports(reported, estimated int) bool {
	// Provider tokenizers and the local content-aware heuristic legitimately
	// differ. Preserve authoritative raw usage across ordinary drift; reject
	// only a material omission (more than 256 tokens and over 20% below the
	// matching full request), which is the compatibility-gateway failure this
	// guard targets.
	return estimated-reported > 256 && reported*5 < estimated*4
}

// estimateUsageFloor intentionally ignores inline image payload bytes. Native
// vision APIs bill images by tiles/provider-specific rules, not as the base64
// string that Metis stores locally. Counting that string here made a correct
// 1K-token provider report look smaller than a 500K-token "minimum", causing
// the active snapshot to be discarded and the UI to jump above 100% again.
// Text, tool arguments/results and message envelopes remain a useful lower
// bound for detecting compatibility gateways that omit whole prompt fields.
func estimateUsageFloor(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += 4
		for _, block := range message.Content {
			total += estimateContentBlockUsageFloor(block)
		}
	}
	return total
}

func estimateContentBlockUsageFloor(block llm.ContentBlock) int {
	total := 8
	total += estimateStringTokens(block.Text)
	total += estimateStringTokens(block.ToolResult)
	total += len(block.ToolName) / 4
	total += len(block.ToolUseID) / 4
	for key, value := range block.ToolInput {
		total += len(key) / 4
		total += approxValueLen(value) / 4
	}
	for _, nested := range block.ToolResultBlocks {
		total += estimateContentBlockUsageFloor(nested)
	}
	return total
}

const estimatedNativeVisionTokensPerImage = 1_600

// estimateActiveHistoryTokens is the display/active-window fallback. Image
// bytes are sent as native image parts and billed by pixels/tiles, not by
// tokenizing their base64 transport encoding. Use a bounded native vision
// allowance for both the visible meter and request-pressure decisions so a
// screenshot cannot manufacture a 300% context window before the provider's
// authoritative usage arrives. HTTP request-size limits guard transport bytes.
func estimateActiveHistoryTokens(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += 4
		for _, block := range message.Content {
			total += estimateActiveContentBlockTokens(block)
		}
	}
	return total
}

func estimateActiveContentBlockTokens(block llm.ContentBlock) int {
	nested := block.ToolResultBlocks
	block.ToolResultBlocks = nil
	hasImage := block.Type == "image" && block.Data != ""
	if hasImage {
		block.Data = ""
	}
	total := estimateContentBlockTokens(block)
	if hasImage {
		total += estimatedNativeVisionTokensPerImage
	}
	for _, child := range nested {
		total += estimateActiveContentBlockTokens(child)
	}
	return total
}

// assistantContextTokens converts response usage into what the next request
// will really replay. Billing output_tokens can include hidden/unsigned
// reasoning that a provider adapter deliberately omits from history. Providers
// exposing ContextHistoryPolicy get a content-based estimate of only replayed
// blocks; legacy providers retain the authoritative output-token fast path.
func assistantContextTokens(provider llm.Provider, assistant llm.Message, reportedOutput int) int {
	policy, hasPolicy := provider.(llm.ContextHistoryPolicy)
	if !hasPolicy {
		if reportedOutput > 0 {
			return reportedOutput
		}
		if len(assistant.Content) == 0 {
			return 0
		}
		return estimateTokens([]llm.Message{assistant})
	}

	filtered := make([]llm.ContentBlock, 0, len(assistant.Content))
	for _, block := range assistant.Content {
		if policy.ContextIncludesAssistantBlock(block) {
			filtered = append(filtered, block)
		}
	}
	if len(filtered) == 0 {
		return 0
	}
	assistant.Content = filtered
	return estimateTokens([]llm.Message{assistant})
}

// storeActiveContextSnapshotLocked installs a response snapshot only when the
// live history and provider/header identity still describe the exact request
// that produced it. Callers hold l.mu and have already appended the assistant
// response.
func (l *Loop) storeActiveContextSnapshotLocked(usage *usageTotals, anchor contextRequestAnchor) {
	if l == nil || !anchor.valid {
		return
	}
	promptTokens, ok := disjointPromptUsageTokens(usage)
	tokens := promptTokens
	var assistant llm.Message
	if len(l.Messages) == anchor.messageCount+1 {
		assistant = l.Messages[anchor.messageCount]
		if usage != nil {
			tokens += assistantContextTokens(anchor.provider, assistant, usage.out)
		}
	}
	estimatedAnchorTokens := estimateUsageFloor(l.Messages[:min(anchor.messageCount, len(l.Messages))]) + anchor.requestOverhead
	if len(assistant.Content) > 0 {
		estimatedAnchorTokens += assistantContextTokens(anchor.provider, assistant, 0)
	}
	currentProvider := identifyContextProvider(l.Provider)
	if !ok ||
		// Compatibility gateways occasionally omit cached/system/tool usage.
		// Reject only a clear omission; normal tokenizer drift must keep the
		// provider's authoritative raw count for the UI tooltip.
		usageClearlyUnderreports(tokens, estimatedAnchorTokens) ||
		(anchor.maxContext > 0 && tokens > anchor.maxContext) ||
		l.historyRevision != anchor.historyRevision ||
		l.routingRevision != anchor.routingRevision ||
		len(l.Messages) != anchor.messageCount+1 ||
		l.Model != anchor.requestModel ||
		currentProvider != anchor.providerID {
		l.invalidateActiveContextLocked()
		return
	}
	l.activeContext = activeContextSnapshot{
		valid:           true,
		tokens:          tokens,
		messageCount:    len(l.Messages),
		historyRevision: l.historyRevision,
		routingRevision: l.routingRevision,
		requestOverhead: anchor.requestOverhead,
		provider:        anchor.providerID,
		requestModel:    anchor.requestModel,
	}
	l.estTokens.Store(int64(tokens))
}

// activeContextBaseLocked returns the active snapshot adjusted for messages
// appended after its anchor, but without the current request overhead. The
// caller adds the freshly estimated/cached overhead, which makes overhead
// changes a delta instead of double-counting the original request prefix.
func (l *Loop) activeContextBaseLocked() (int, bool) {
	if l == nil {
		return 0, false
	}
	snapshot := l.activeContext
	if !snapshot.valid ||
		snapshot.historyRevision != l.historyRevision ||
		snapshot.routingRevision != l.routingRevision ||
		snapshot.messageCount < 0 || snapshot.messageCount > len(l.Messages) ||
		snapshot.requestModel != l.Model ||
		snapshot.provider != identifyContextProvider(l.Provider) {
		return 0, false
	}
	return snapshot.tokens + estimateActiveHistoryTokens(l.Messages[snapshot.messageCount:]) - snapshot.requestOverhead, true
}
