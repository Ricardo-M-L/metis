package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDumpRequestDigestMatchesOriginalWireBodyBeforeRedaction(t *testing.T) {
	path := setupDumpTest(t)
	raw := []byte(`{"model":"audit-model","private_token":"opaque-audit-secret","messages":[{"role":"user","content":"audit marker"}]}`)
	receivedBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		receivedBody <- received
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = WrapRoundTripper(client.Transport, "anthropic", nil)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "opaque-audit-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	FlushDumps()
	entries := drainDumpFile(t, path, 2)
	received := <-receivedBody
	if !bytes.Equal(received, raw) {
		t.Fatal("diagnostic capture changed the dispatched body")
	}
	expected := sha256.Sum256(received)
	found := false
	for _, entry := range entries {
		if entry.Type != "request" {
			continue
		}
		encoded, _ := json.Marshal(entry)
		var fields map[string]any
		json.Unmarshal(encoded, &fields)
		if fields["request_sha256"] != hex.EncodeToString(expected[:]) || fields["request_bytes"] != float64(len(raw)) {
			t.Fatalf("original request digest/size missing or changed: digest=%v size=%v", fields["request_sha256"], fields["request_bytes"])
		}
		redactedHash := sha256.Sum256(entry.Body)
		if hex.EncodeToString(redactedHash[:]) == fields["request_sha256"] {
			t.Fatal("redacted JSON hash was substituted for the original wire digest")
		}
		found = true
	}
	if !found {
		t.Fatal("request evidence missing")
	}
	logged, _ := os.ReadFile(path)
	if strings.Contains(string(logged), "opaque-audit-secret") {
		t.Fatal("raw credential escaped diagnostic redaction")
	}
}

func TestDumpContentDigestAndProbesUseOriginalText(t *testing.T) {
	path := setupDumpTest(t)
	marker := "METIS_DIGEST_PROBE_fedcba9876543210"
	text := marker + " 0123456789 " + marker
	system := "system 0123456789"
	t.Setenv("METIS_DUMP_TEXT_PROBES", `["`+marker+`","missing-probe"]`)
	raw, err := json.Marshal(map[string]any{
		"model": "audit-model", "system": []map[string]string{{"type": "text", "text": system}},
		"messages": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	receivedBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody <- body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = WrapRoundTripper(client.Transport, "anthropic", nil)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", bytes.NewReader(raw))
	req.Header.Set("X-Api-Key", "123456")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	FlushDumps()
	if !bytes.Equal(<-receivedBody, raw) {
		t.Fatal("diagnostic content digest changed the physical request")
	}
	for _, entry := range drainDumpFile(t, path, 2) {
		if entry.Type != "request" {
			continue
		}
		encoded, _ := json.Marshal(entry)
		var fields map[string]json.RawMessage
		json.Unmarshal(encoded, &fields)
		var digest struct {
			Schema       int `json:"schema"`
			SystemBlocks []struct {
				TextSHA256 string `json:"text_sha256"`
				TextBytes  int    `json:"text_bytes"`
			} `json:"system_blocks"`
			Messages []struct {
				Index  int    `json:"index"`
				Role   string `json:"role"`
				Blocks []struct {
					TextSHA256 string `json:"text_sha256"`
					TextBytes  int    `json:"text_bytes"`
				} `json:"blocks"`
			} `json:"messages"`
			Probes []struct {
				SHA256      string `json:"sha256"`
				Occurrences int    `json:"occurrences"`
			} `json:"probes"`
		}
		json.Unmarshal(fields["request_content_digest"], &digest)
		if digest.Schema != 1 || len(digest.SystemBlocks) != 1 || len(digest.Messages) != 1 || len(digest.Messages[0].Blocks) != 1 || len(digest.Probes) != 2 {
			t.Fatalf("pre-redaction content evidence missing: %+v", digest)
		}
		sha := func(text string) string { sum := sha256.Sum256([]byte(text)); return hex.EncodeToString(sum[:]) }
		block := digest.Messages[0].Blocks[0]
		if block.TextSHA256 != sha(text) || block.TextBytes != len(text) || digest.Messages[0].Role != "user" || digest.Messages[0].Index != 0 {
			t.Fatalf("original message digest/order lost: %+v", digest.Messages[0])
		}
		if digest.SystemBlocks[0].TextSHA256 != sha(system) || digest.SystemBlocks[0].TextBytes != len(system) {
			t.Fatal("original system digest lost")
		}
		if digest.Probes[0].SHA256 != sha(marker) || digest.Probes[0].Occurrences != 2 || digest.Probes[1].Occurrences != 0 {
			t.Fatalf("probe counts not based on original string values: %+v", digest.Probes)
		}
		if strings.Contains(string(fields["request_content_digest"]), marker) || strings.Contains(string(fields["request_content_digest"]), "0123456789") {
			t.Fatal("content evidence retained raw text instead of hashes")
		}
		if !strings.Contains(string(entry.Body), "[REDACTED]") || strings.Contains(string(entry.Body), "123456") {
			t.Fatal("content evidence bypassed existing diagnostic body redaction")
		}
		return
	}
	t.Fatal("no request evidence")
}
