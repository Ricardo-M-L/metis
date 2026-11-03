package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type chatTokenBudgetField string

const (
	chatMaxTokens           chatTokenBudgetField = "max_tokens"
	chatMaxCompletionTokens chatTokenBudgetField = "max_completion_tokens"
)

type chatTokenBudgetRoute struct {
	baseURL string
	model   string
}

// Keep protocol selection on the concrete Chat provider. ToRequest is also
// used by Azure deployment routes, where a deployment name is not necessarily
// an OpenAI model id; changing that shared converter would guess its protocol.
func defaultChatTokenBudgetField(model string) chatTokenBudgetField {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, family := range []string{"o1", "o3", "o4", "gpt-5", "gpt-6"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return chatMaxCompletionTokens
		}
		// GPT version suffixes are numeric, e.g. gpt-6.1-sol. Do not
		// accidentally classify gpt-60-local or gpt-6.preview aliases.
		if strings.HasPrefix(family, "gpt-") && strings.HasPrefix(model, family+".") {
			rest := strings.TrimPrefix(model, family+".")
			if len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
				return chatMaxCompletionTokens
			}
		}
	}
	return chatMaxTokens
}

// chatTokenBudget is private to one logical Complete/Stream call. In
// particular, compatibilityRetried must survive the separate overflow retry,
// so an inconsistent gateway cannot make the two budget fields ping-pong.
type chatTokenBudget struct {
	provider             *OpenAI
	route                chatTokenBudgetRoute
	field                chatTokenBudgetField
	amount               int
	compatibilityRetried bool
}

func (o *OpenAI) newChatTokenBudget(req Request) *chatTokenBudget {
	route := chatTokenBudgetRoute{baseURL: o.BaseURL, model: o.Model}
	field := defaultChatTokenBudgetField(route.model)
	o.tokenBudgetMu.Lock()
	if learned, ok := o.tokenBudgetFields[route]; ok {
		field = learned
	}
	o.tokenBudgetMu.Unlock()
	amount := o.MaxTokens
	if req.MaxTokens > 0 {
		amount = req.MaxTokens
	}
	return &chatTokenBudget{provider: o, route: route, field: field, amount: amount}
}

func (b *chatTokenBudget) apply(body *oaiReq, amount int) {
	body.MaxTokens, body.MaxCompletionTokens = 0, 0
	if b.field == chatMaxCompletionTokens {
		body.MaxCompletionTokens = amount
	} else {
		body.MaxTokens = amount
	}
}

func (b *chatTokenBudget) remember() {
	b.provider.tokenBudgetMu.Lock()
	defer b.provider.tokenBudgetMu.Unlock()
	if b.provider.tokenBudgetFields == nil {
		b.provider.tokenBudgetFields = make(map[chatTokenBudgetRoute]chatTokenBudgetField)
	}
	b.provider.tokenBudgetFields[b.route] = b.field
}

// Only a structured 400 that identifies the rejected budget field and
// explicitly tells us to use the other field is safe to negotiate. Generic
// validation errors, auth/rate failures, transport errors and accepted streams
// are never replayed by this compatibility path.
func alternateChatTokenBudgetField(status int, raw string, current chatTokenBudgetField) (chatTokenBudgetField, bool) {
	if status != http.StatusBadRequest {
		return "", false
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil || payload.Error.Code != "unsupported_parameter" || payload.Error.Param != string(current) {
		return "", false
	}
	alternate := chatMaxCompletionTokens
	if current == chatMaxCompletionTokens {
		alternate = chatMaxTokens
	}
	message := strings.ToLower(payload.Error.Message)
	message = strings.NewReplacer("'", "", "\"", "", "`", "").Replace(message)
	message = strings.Join(strings.Fields(message), " ")
	if !strings.Contains(message, "use "+string(alternate)+" instead") {
		return "", false
	}
	return alternate, true
}

func (b *chatTokenBudget) run(ctx context.Context, attempt func() (status int, raw string, err error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, raw, err := attempt()
		if err == nil {
			b.remember()
			return nil
		}
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		alternate, ok := alternateChatTokenBudgetField(status, raw, b.field)
		if b.compatibilityRetried || !ok {
			return err
		}
		b.compatibilityRetried = true
		b.field = alternate
	}
}
