package mcpoauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDynamicRegistrationRejectsCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			const redirectSecret = "registration-location-must-not-leak"
			var reached atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached.Add(1)
				_, _ = fmt.Fprint(w, `{"client_id":"redirected-client"}`)
			}))
			t.Cleanup(target.Close)

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/?secret="+redirectSecret, status)
			}))
			t.Cleanup(source.Close)

			_, err := registerClient(context.Background(), source.URL, []string{"http://127.0.0.1:7700/callback"})
			if err == nil {
				t.Fatal("cross-origin registration redirect was followed")
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

func TestDynamicRegistrationPreservesSameOriginRedirectBehavior(t *testing.T) {
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
			mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, `{"client_id":"same-origin-client"}`)
			})

			clientID, err := registerClient(
				context.Background(), server.URL+"/start",
				[]string{"http://127.0.0.1:7700/callback"},
			)
			if err != nil {
				t.Fatalf("same-origin registration redirect failed: %v", err)
			}
			if clientID != "same-origin-client" {
				t.Fatalf("client id = %q", clientID)
			}
		})
	}
}
