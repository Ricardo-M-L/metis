package llm

// Hand-rolled GCP service-account → OAuth2 access token flow. Mirrors
// what google.golang.org/x/oauth2/google.JWTConfigFromJSON does, but
// without pulling that dependency tree in. Used by the Vertex AI
// provider; future GCS / GenAI integrations can reuse the same client.
//
// Service-account JSON shape (subset metis needs):
//
//	{
//	  "type": "service_account",
//	  "client_email": "metis-sa@my-project.iam.gserviceaccount.com",
//	  "private_key_id": "abc123...",
//	  "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
//	  "token_uri": "https://oauth2.googleapis.com/token"
//	}
//
// Standard scope for Vertex AI is
// `https://www.googleapis.com/auth/cloud-platform`.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ServiceAccountKey is the parsed service-account JSON. Only the
// fields metis uses are kept.
type ServiceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`    // PEM-encoded RSA
	PrivateKeyID string `json:"private_key_id"` // optional, surfaced for debug
	TokenURI     string `json:"token_uri"`      // usually https://oauth2.googleapis.com/token
}

// LoadServiceAccount parses a service-account JSON blob (file
// contents). Validates the type field — pasting a different shape of
// credential file would otherwise fail mysteriously inside the JWT
// signer.
func LoadServiceAccount(data []byte) (*ServiceAccountKey, error) {
	var k ServiceAccountKey
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("service-account: %w", err)
	}
	if k.Type != "service_account" {
		return nil, fmt.Errorf("service-account: type=%q, want service_account (this looks like a different credential format)", k.Type)
	}
	if k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("service-account: missing client_email or private_key")
	}
	if k.TokenURI == "" {
		k.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &k, nil
}

// rsaPrivateKey decodes the PEM-encoded RSA private key from a
// service-account JSON. Returns a typed *rsa.PrivateKey ready for
// jwt.Sign.
func (k *ServiceAccountKey) rsaPrivateKey() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, errors.New("service-account: private_key has no PEM block")
	}
	// GCP issues PKCS#8-encoded keys.
	if priv, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("service-account: PKCS8 key is not RSA")
		}
		return rsaKey, nil
	}
	// Fallback for legacy PKCS#1 keys (rare from GCP but valid PEM).
	if priv, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return priv, nil
	}
	return nil, errors.New("service-account: private_key is neither PKCS8 nor PKCS1 RSA")
}

// jwtClaims is the minimum claim set GCP's token endpoint requires for
// the urn:ietf:params:oauth:grant-type:jwt-bearer flow.
type jwtClaims struct {
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
	Aud   string `json:"aud"`
	Exp   int64  `json:"exp"`
	Iat   int64  `json:"iat"`
}

// signJWT builds a self-signed assertion JWT (RS256). GCP exchanges it
// for a short-lived access token at the token endpoint.
func (k *ServiceAccountKey) signJWT(scope string, now time.Time) (string, error) {
	priv, err := k.rsaPrivateKey()
	if err != nil {
		return "", err
	}

	header := struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid,omitempty"`
	}{Alg: "RS256", Typ: "JWT", Kid: k.PrivateKeyID}

	claims := jwtClaims{
		Iss:   k.ClientEmail,
		Scope: scope,
		Aud:   k.TokenURI,
		Iat:   now.Unix(),
		Exp:   now.Add(1 * time.Hour).Unix(),
	}

	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) +
		"." + base64.RawURLEncoding.EncodeToString(claimsJSON)

	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("service-account: sign JWT: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// GCPTokenSource issues + caches OAuth2 access tokens for a single
// service account + scope. Tokens are refreshed when they have less
// than 60 seconds remaining — leaves headroom for slow networks
// without paying the round-trip on every call.
type GCPTokenSource struct {
	Key   *ServiceAccountKey
	Scope string
	HTTP  *http.Client
	Now   func() time.Time

	mu        sync.Mutex
	cached    string
	expiresAt time.Time
}

// NewGCPTokenSource returns a token source bound to one service
// account + scope. The default scope works for Vertex AI; pass a
// different one for other GCP APIs.
func NewGCPTokenSource(key *ServiceAccountKey, scope string) *GCPTokenSource {
	if scope == "" {
		scope = "https://www.googleapis.com/auth/cloud-platform"
	}
	return &GCPTokenSource{
		Key:   key,
		Scope: scope,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		Now:   time.Now,
	}
}

// Token returns a current OAuth2 access token. Cached when within the
// 1-minute refresh window; re-fetched otherwise.
func (s *GCPTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != "" && s.Now().Add(60*time.Second).Before(s.expiresAt) {
		return s.cached, nil
	}

	now := s.Now()
	jwt, err := s.Key.signJWT(s.Scope, now)
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)

	req, err := http.NewRequestWithContext(ctx, "POST", s.Key.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gcp token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gcp token endpoint %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("gcp token: decode response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("gcp token: empty access_token in response: %s", truncate(string(body), 200))
	}

	s.cached = tr.AccessToken
	s.expiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	return s.cached, nil
}
