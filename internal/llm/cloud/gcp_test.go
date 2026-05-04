package cloud

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// genTestKey makes a small (1024-bit, fine for tests) RSA key paired
// with a service-account JSON blob.
func genTestKey(t *testing.T) (*rsa.PrivateKey, *ServiceAccountKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return priv, &ServiceAccountKey{
		Type:        "service_account",
		ClientEmail: "test-sa@example.iam.gserviceaccount.com",
		PrivateKey:  string(pemBytes),
		TokenURI:    "https://oauth2.googleapis.com/token",
	}
}

func TestLoadServiceAccount_HappyPath(t *testing.T) {
	_, key := genTestKey(t)
	data, _ := json.Marshal(key)

	got, err := LoadServiceAccount(data)
	if err != nil {
		t.Fatalf("LoadServiceAccount: %v", err)
	}
	if got.ClientEmail != key.ClientEmail {
		t.Errorf("ClientEmail: got %q, want %q", got.ClientEmail, key.ClientEmail)
	}
}

func TestLoadServiceAccount_RejectsWrongType(t *testing.T) {
	wrong := []byte(`{"type":"authorized_user","client_email":"x"}`)
	_, err := LoadServiceAccount(wrong)
	if err == nil || !strings.Contains(err.Error(), "service_account") {
		t.Errorf("expected type-mismatch error; got %v", err)
	}
}

func TestLoadServiceAccount_DefaultsTokenURI(t *testing.T) {
	_, key := genTestKey(t)
	key.TokenURI = "" // simulate older JSON files
	data, _ := json.Marshal(key)

	got, err := LoadServiceAccount(data)
	if err != nil {
		t.Fatalf("LoadServiceAccount: %v", err)
	}
	if got.TokenURI != "https://oauth2.googleapis.com/token" {
		t.Errorf("TokenURI: got %q, want default oauth2.googleapis.com/token", got.TokenURI)
	}
}

func TestSignJWT_ProducesValidStructure(t *testing.T) {
	_, key := genTestKey(t)
	tok, err := key.signJWT("https://www.googleapis.com/auth/cloud-platform", time.Now())
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 dot-separated segments; got %d", len(parts))
	}
	// Decode header
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatalf("header json: %v", err)
	}
	if hdr["alg"] != "RS256" {
		t.Errorf("alg: got %q, want RS256", hdr["alg"])
	}
	// Decode claims
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims jwtClaims
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatalf("claims json: %v", err)
	}
	if claims.Iss != key.ClientEmail {
		t.Errorf("iss: got %q, want %q", claims.Iss, key.ClientEmail)
	}
	if claims.Aud != key.TokenURI {
		t.Errorf("aud: got %q, want %q", claims.Aud, key.TokenURI)
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("exp(%d) must be > iat(%d)", claims.Exp, claims.Iat)
	}
}

// TestTokenSource_FetchAndCache: first Token() POSTs to token URI;
// second Token() within TTL uses cache (no extra POST).
func TestTokenSource_FetchAndCache(t *testing.T) {
	_, key := genTestKey(t)

	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != "POST" {
			t.Errorf("token endpoint must POST; got %s", r.Method)
		}
		// Verify required form fields are present.
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type: got %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("assertion") == "" {
			t.Error("assertion (JWT) missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	key.TokenURI = srv.URL
	ts := NewGCPTokenSource(key, "")
	now := time.Now()
	ts.Now = func() time.Time { return now }

	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	if got != "ya29.test" {
		t.Errorf("token: got %q", got)
	}
	if posts != 1 {
		t.Errorf("first Token: posts=%d, want 1", posts)
	}

	// Second call within cache window — should NOT hit network.
	got2, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if got2 != got {
		t.Errorf("second token differs: %q vs %q", got, got2)
	}
	if posts != 1 {
		t.Errorf("cache miss: posts=%d, want still 1", posts)
	}
}

// TestTokenSource_RefreshNearExpiry: fast-forward to within 60s of
// expiry → next Token() refreshes from server.
func TestTokenSource_RefreshNearExpiry(t *testing.T) {
	_, key := genTestKey(t)

	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":300,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	key.TokenURI = srv.URL

	ts := NewGCPTokenSource(key, "")
	now := time.Now()
	ts.Now = func() time.Time { return now }

	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	// Move clock to 250s in (token expires at +300, refresh window
	// kicks in at +240).
	ts.Now = func() time.Time { return now.Add(250 * time.Second) }
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if posts != 2 {
		t.Errorf("expected refresh near expiry: posts=%d, want 2", posts)
	}
}

// TestTokenSource_TokenEndpointError surfaces server-side rejection.
func TestTokenSource_TokenEndpointError(t *testing.T) {
	_, key := genTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad sig"}`))
	}))
	defer srv.Close()
	key.TokenURI = srv.URL

	ts := NewGCPTokenSource(key, "")
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error from 401 token endpoint")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should include status code: %v", err)
	}
}

// TestTokenSource_DefaultScope: empty scope arg falls back to
// cloud-platform.
func TestTokenSource_DefaultScope(t *testing.T) {
	_, key := genTestKey(t)
	ts := NewGCPTokenSource(key, "")
	if ts.Scope != "https://www.googleapis.com/auth/cloud-platform" {
		t.Errorf("default scope: got %q", ts.Scope)
	}
}

// silence unused — url is referenced in the form-parsing assertion via
// r.ParseForm() (which uses net/url internally).
var _ = url.PathEscape
