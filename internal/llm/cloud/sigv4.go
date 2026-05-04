// Package cloud holds cross-provider auth + signing helpers — AWS
// SigV4 (used by Bedrock) and GCP service-account → OAuth2 (used by
// Vertex AI). Lives in its own package so future cloud-auth providers
// (Cloudflare AI Gateway, Azure AAD device flow, …) can reuse the
// signer without pulling in any provider-specific code.
package cloud

// Hand-rolled AWS Signature Version 4 signer. Just enough for Bedrock
// Runtime's POST /model/{id}/invoke[-with-response-stream] — no
// chunked uploads, no presigned URLs, no STS-session-token wrap (yet).
// Spec: https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
//
// Why hand-roll: pulling aws-sdk-go-v2 brings ~10 transitive deps for
// what is fundamentally a single-request signer with one hash + one
// HMAC chain. metis's other providers are stdlib-only HTTP; staying
// consistent keeps the LLM transport layer reviewable.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWSCreds is the minimum set of credentials a SigV4 signer needs.
// SessionToken is optional and only present when using STS-issued
// short-lived creds (e.g. assumed-role / IAM Identity Center / web
// identity). Bedrock IAM users typically just have the first two.
type AWSCreds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional: STS / assumed-role / IRSA
}

// SignV4 mutates r in place: adds Host, X-Amz-Date, Authorization
// (and X-Amz-Security-Token when SessionToken is set). Body is
// hashed via the included payload bytes — caller must pass the same
// bytes that will end up on the wire (the http.Request.Body is NOT
// re-read, so this signs an unconditional snapshot).
//
// Order of operations matters: every header that participates in
// Authorization's signed-headers list must be added BEFORE signing,
// or the server rejects with `SignatureDoesNotMatch`.
func SignV4(r *http.Request, payload []byte, creds AWSCreds, region, service string, now time.Time) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return fmt.Errorf("sigv4: missing AWS credentials")
	}

	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	r.Header.Set("Host", r.URL.Host)
	r.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	hashedPayload := sha256Hex(payload)
	r.Header.Set("X-Amz-Content-Sha256", hashedPayload)

	// Canonical request — strict order: method, URI, query, headers
	// (sorted lowercase), signed-headers list, payload hash.
	canonURI := r.URL.EscapedPath()
	if canonURI == "" {
		canonURI = "/"
	}
	canonQuery := canonicalQuery(r.URL.RawQuery)
	signedHeaders, canonHeaders := canonicalHeaders(r.Header)

	canonReq := strings.Join([]string{
		r.Method,
		canonURI,
		canonQuery,
		canonHeaders, // ends with newline
		signedHeaders,
		hashedPayload,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonReq)),
	}, "\n")

	// Derive the signing key by chaining HMACs through date / region /
	// service / "aws4_request".
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, credentialScope, signedHeaders, signature,
	))
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// canonicalHeaders builds the lowercase-name + trim-value header list
// AWS expects in the canonical request. Returns (signed-headers list,
// canonical-headers block — already terminated with a newline so the
// caller can drop it directly into the canonical request string).
func canonicalHeaders(h http.Header) (string, string) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		// Use the canonical (lowercase) name, but pull the value via
		// any case http.Header indexes under.
		vals := h.Values(k)
		if len(vals) == 0 {
			vals = h.Values(http.CanonicalHeaderKey(k))
		}
		// Collapse multi-line values per AWS rules: trim each, join
		// with comma. http.Header allows duplicates which represent the
		// list shape clients care about.
		joined := strings.Join(trimAll(vals), ",")
		fmt.Fprintf(&sb, "%s:%s\n", k, joined)
	}
	return strings.Join(keys, ";"), sb.String()
}

func trimAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// canonicalQuery sorts query params lexicographically by key (then
// value). Empty query → "" (not "/"), which is what the spec
// expects.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}
