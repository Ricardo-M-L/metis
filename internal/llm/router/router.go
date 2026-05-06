// Package router picks the right LLM provider+model for a given
// request based on capability hints. Inspired by opencode's
// provider/models.ts: instead of hard-coding "if attachment use
// claude-haiku-vision", encode the requirements as a Hint and let the
// catalog (models.dev) tell us which model satisfies them at the
// cheapest input price.
//
// Common use case: the user pastes an image. The agent loop sets
// Hint{NeedAttachment: true, MinContext: 32_000} and switches to
// whichever attachment-capable model is cheapest among the user's
// configured providers.
package router

import (
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
)

// Hint describes what a particular request needs from the model.
// All fields are optional — zero values mean "any." Combine fields
// freely; unmatched constraints exclude the model entirely.
type Hint struct {
	// NeedAttachment requires Model.Attachment == true (vision / file
	// upload supported). Set when the user includes an image or PDF.
	NeedAttachment bool

	// NeedReasoning requires Model.Reasoning == true (extended thinking
	// / chain-of-thought). Set for hard problems or when the user
	// explicitly opts into "think harder."
	NeedReasoning bool

	// NeedToolCall requires Model.ToolCall == true. Almost always
	// true for the agent loop — only completion-only models would
	// fail this. Default false here so unit tests don't have to set
	// it on every Hint they construct.
	NeedToolCall bool

	// MinContext requires Limit.Context >= this many tokens.
	// Use for "I'm pasting a big diff, need a long-context model."
	MinContext int

	// MaxInputCost rejects models charging more than this USD per
	// million input tokens. Use for cost-sensitive batch jobs.
	// 0 means "no cost ceiling."
	MaxInputCost float64

	// PreferProvider gives a small ranking nudge (10% effective price
	// cut) toward this provider when otherwise tied. Use to bias
	// toward the user's authenticated provider.
	PreferProvider string

	// AllowDeprecated lets models with status="deprecated" enter the
	// pool. Default false because picking a deprecated model behind
	// the user's back is a bad surprise.
	AllowDeprecated bool
}

// Match reports whether m satisfies h. Zero-valued hint fields are
// always satisfied. Cost / context comparisons skip when the catalog
// entry has zero values (incomplete metadata) so a provider missing
// pricing data isn't auto-disqualified.
func (h Hint) Match(m catalog.Model) bool {
	if h.NeedAttachment && !m.Attachment {
		return false
	}
	if h.NeedReasoning && !m.Reasoning {
		return false
	}
	if h.NeedToolCall && !m.ToolCall {
		return false
	}
	if h.MinContext > 0 && m.Limit.Context > 0 && m.Limit.Context < h.MinContext {
		return false
	}
	if h.MaxInputCost > 0 && m.Cost.Input > 0 && m.Cost.Input > h.MaxInputCost {
		return false
	}
	if !h.AllowDeprecated && strings.EqualFold(m.Status, "deprecated") {
		return false
	}
	return true
}

// Choice is one router result. Reasons documents which Hint fields
// were satisfied — useful for "/route why" introspection so the user
// understands why a given model was picked.
type Choice struct {
	Provider string
	Model    string
	Reasons  []string
}

// Pick scans cat for the cheapest model satisfying hint. Tie-break
// order: lower cost.input → larger context → alphabetical (stable).
//
// Returns the empty Choice when no model in cat matches. Callers
// should treat (Choice{}.Provider == "") as "no route — fall back
// to the user's configured default."
func Pick(cat catalog.Catalog, h Hint) Choice {
	type cand struct {
		provider, model string
		cost            float64
		context         int
	}
	var cands []cand
	for pid, p := range cat {
		for mid, m := range p.Models {
			if !h.Match(m) {
				continue
			}
			cost := m.Cost.Input
			if h.PreferProvider != "" && strings.EqualFold(pid, h.PreferProvider) {
				cost *= 0.9
			}
			cands = append(cands, cand{
				provider: pid,
				model:    mid,
				cost:     cost,
				context:  m.Limit.Context,
			})
		}
	}
	if len(cands) == 0 {
		return Choice{}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].cost != cands[j].cost {
			return cands[i].cost < cands[j].cost
		}
		if cands[i].context != cands[j].context {
			return cands[i].context > cands[j].context
		}
		return cands[i].provider+"/"+cands[i].model <
			cands[j].provider+"/"+cands[j].model
	})
	return Choice{
		Provider: cands[0].provider,
		Model:    cands[0].model,
		Reasons:  reasonsFor(h),
	}
}

// PickAll returns every catalog entry matching hint, ordered by
// price ascending. Useful for /route list and "show me the runners-
// up" debug surfaces. The cost-tie-break is stable so two runs in a
// row return the same ordering.
func PickAll(cat catalog.Catalog, h Hint) []Choice {
	type cand struct {
		choice  Choice
		cost    float64
		context int
	}
	var cands []cand
	reasons := reasonsFor(h)
	for pid, p := range cat {
		for mid, m := range p.Models {
			if !h.Match(m) {
				continue
			}
			cost := m.Cost.Input
			if h.PreferProvider != "" && strings.EqualFold(pid, h.PreferProvider) {
				cost *= 0.9
			}
			cands = append(cands, cand{
				choice:  Choice{Provider: pid, Model: mid, Reasons: reasons},
				cost:    cost,
				context: m.Limit.Context,
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].cost != cands[j].cost {
			return cands[i].cost < cands[j].cost
		}
		if cands[i].context != cands[j].context {
			return cands[i].context > cands[j].context
		}
		return cands[i].choice.Provider+"/"+cands[i].choice.Model <
			cands[j].choice.Provider+"/"+cands[j].choice.Model
	})
	out := make([]Choice, len(cands))
	for i, c := range cands {
		out[i] = c.choice
	}
	return out
}

// reasonsFor renders the active hint constraints as human-readable
// strings. Returned slice is freshly allocated on each call; safe
// for the caller to extend.
func reasonsFor(h Hint) []string {
	var r []string
	if h.NeedAttachment {
		r = append(r, "needs attachment")
	}
	if h.NeedReasoning {
		r = append(r, "needs reasoning")
	}
	if h.NeedToolCall {
		r = append(r, "needs tool_call")
	}
	if h.MinContext > 0 {
		r = append(r, "min context")
	}
	if h.MaxInputCost > 0 {
		r = append(r, "cost ceiling")
	}
	if h.PreferProvider != "" {
		r = append(r, "prefers "+h.PreferProvider)
	}
	if h.AllowDeprecated {
		r = append(r, "deprecated allowed")
	}
	return r
}
