package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const (
	openAICodexProviderID = "openai-codex"
	openAIAuthClaim       = "https://api.openai.com/auth"
)

// AcquireOAuthCredential runs a provider's login flow and returns the complete
// refreshable credential without persisting it. Command layers can then switch
// authentication methods with one ActivateOAuth transaction.
func AcquireOAuthCredential(ctx context.Context, provider string, opts OAuthOptions) (*OAuthCredential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return nil, err
	}
	if _, ok := KnownProviders[provider]; !ok {
		return nil, errors.New("llm oauth: unsupported provider")
	}
	opts.SkipPersist = true
	token, err := OAuthLoginOptsContext(ctx, provider, opts)
	if err != nil {
		return nil, err
	}
	credential, err := credentialFromToken(provider, token)
	if err != nil {
		return nil, err
	}
	return cloneOAuthCredential(credential), nil
}

// LoginOAuthCredential preserves the historical library contract: run the
// flow and persist the rich credential. New interactive login commands should
// prefer AcquireOAuthCredential followed by ActivateOAuth.
func LoginOAuthCredential(ctx context.Context, provider string, opts OAuthOptions) (*OAuthCredential, error) {
	credential, err := AcquireOAuthCredential(ctx, provider, opts)
	if err != nil {
		return nil, err
	}
	if err := PutOAuth(provider, *credential); err != nil {
		return nil, err
	}
	return cloneOAuthCredential(credential), nil
}

func credentialFromToken(provider string, token *Token) (*OAuthCredential, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("llm oauth: provider returned no access token")
	}
	credential := &OAuthCredential{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    token.ExpiresAt,
		TokenType:    token.TokenType,
		Scope:        token.Scope,
	}
	if credential.TokenType == "" {
		credential.TokenType = "Bearer"
	}
	if provider == "anthropic" || provider == openAICodexProviderID {
		if strings.TrimSpace(credential.RefreshToken) == "" || credential.ExpiresAt.IsZero() {
			return nil, errors.New("llm oauth: provider returned an incomplete refreshable credential")
		}
	}
	if provider == openAICodexProviderID {
		credential.AccountID = openAICodexAccountID(token.AccessToken)
		if credential.AccountID == "" {
			return nil, errors.New("llm oauth: OpenAI credential did not contain an account id")
		}
	}
	if err := validateOAuthCredential(*credential); err != nil {
		return nil, err
	}
	return credential, nil
}

// openAICodexAccountID decodes only the JWT payload to obtain routing
// metadata. It does not authenticate the JWT; the bearer token remains opaque
// and is validated by OpenAI. No token or payload text is returned in errors.
func openAICodexAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 || len(parts[1]) == 0 || len(parts[1]) > 1<<20 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 1<<20 {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	// Current tokens use an object under https://api.openai.com/auth.
	if authClaims, ok := claims[openAIAuthClaim].(map[string]any); ok {
		if accountID := boundedAccountID(authClaims["chatgpt_account_id"]); accountID != "" {
			return accountID
		}
	}
	// Accept the flattened form used by a few JWT libraries and fixtures.
	if accountID := boundedAccountID(claims[openAIAuthClaim+".chatgpt_account_id"]); accountID != "" {
		return accountID
	}
	if accountID := boundedAccountID(claims[openAIAuthClaim+"/chatgpt_account_id"]); accountID != "" {
		return accountID
	}
	return boundedAccountID(claims["chatgpt_account_id"])
}

func boundedAccountID(value any) string {
	accountID, ok := value.(string)
	if !ok {
		return ""
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(accountID) > 512 || strings.ContainsAny(accountID, "\x00\r\n") {
		return ""
	}
	return accountID
}
