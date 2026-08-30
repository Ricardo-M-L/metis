package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientRejectsCrossOriginRedirectsBeforeSendingCredentials(t *testing.T) {
	for _, status := range []int{
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			const redirectSecret = "mcp-location-must-not-leak"
			var reached atomic.Int32
			fake := &fakeMCPServer{}
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				fake.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(target.Close)

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/?secret="+redirectSecret, status)
			}))
			t.Cleanup(source.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, err := NewHTTPClient(ctx, source.URL, map[string]string{
				"Authorization": "Bearer private-bearer",
				"X-API-Key":     "private-api-key",
			})
			if client != nil {
				_ = client.Close()
			}
			if err == nil {
				t.Fatal("cross-origin MCP redirect was followed")
			}
			if reached.Load() != 0 {
				t.Fatalf("cross-origin destination received %d request(s)", reached.Load())
			}
			if strings.Contains(err.Error(), redirectSecret) {
				t.Fatalf("redirect destination leaked through error: %v", err)
			}
		})
	}
}

func TestMCPHTTPClientPreservesSameOriginRedirectBehavior(t *testing.T) {
	for _, status := range []int{
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			mux := http.NewServeMux()
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/final", status)
			})
			mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-API-Key"); got != "same-origin-api-key" {
					t.Errorf("same-origin header = %q", got)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read redirected body: %v", err)
				}
				if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
					if string(body) != "private-json-rpc-body" {
						t.Errorf("redirected body = %q", body)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})

			req, err := http.NewRequest(http.MethodPost, server.URL+"/start", strings.NewReader("private-json-rpc-body"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-API-Key", "same-origin-api-key")
			resp, err := newMCPHTTPClient().Do(req)
			if err != nil {
				t.Fatalf("same-origin redirect failed: %v", err)
			}
			_ = resp.Body.Close()
		})
	}
}
