package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "verification_fixture_token_1234567890"
const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNewClientNarrowEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:1", "http://127.0.0.1:65535", "http://[::1]:8030"} {
		if _, err := NewClient(endpoint, testToken); err != nil {
			t.Errorf("valid endpoint rejected: %v", err)
		}
	}
	for _, endpoint := range []string{
		"https://127.0.0.1:80", "http://localhost:80", "http://127.0.0.2:80", "http://127.1:80",
		"http://example.com:80", "http://[::ffff:127.0.0.1]:80", "http://127.0.0.1", "http://127.0.0.1:0",
		"http://127.0.0.1:65536", "http://127.0.0.1:+80", "http://127.0.0.1:abc", "http://user:secret@127.0.0.1:80",
		"http://127.0.0.1:80/", "http://127.0.0.1:80/v1", "http://127.0.0.1:80?", "http://127.0.0.1:80?token=x",
		"http://127.0.0.1:80#x", "http://127.0.0.1:80#", " http://127.0.0.1:80", "http://127.0.0.1:80\\evil",
	} {
		if _, err := NewClient(endpoint, testToken); err == nil {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	for _, token := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 257), testToken + "\n", testToken + "+", testToken + "="} {
		if _, err := NewClient("http://127.0.0.1:80", token); err == nil {
			t.Errorf("unsafe token accepted")
		} else if token != "" && strings.Contains(err.Error(), token) {
			t.Fatal("token leaked in error")
		}
	}
}

func newFixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.pollInterval = time.Millisecond
	return client
}

func writeJob(w http.ResponseWriter, status string) {
	fmt.Fprintf(w, `{"job_id":"job-001","suite":"smoke","status":%q,"source_sha256":%q,"source_changed":false,"result":{"checks":[]},"error":null}`, status, testHash)
}

func TestClientAuthSourceAndAcceptance(t *testing.T) {
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Method != http.MethodGet {
			t.Errorf("incorrect request authentication/method")
		}
		switch r.URL.Path {
		case "/v1/source":
			fmt.Fprintf(w, `{"source_sha256":%q}`, testHash)
		case "/v1/acceptance":
			fmt.Fprintf(w, `{"source_sha256":%q,"checks":[{"id":"smoke","status":"passed","exit_code":0,"duration_ms":12.25,"artifact_sha256":null,"details":{"note":"safe"}}]}`, testHash)
		default:
			t.Error("unexpected route")
		}
	})
	if got, err := client.SourceHash(context.Background()); err != nil || got != testHash {
		t.Fatalf("source = %q, %v", got, err)
	}
	got, err := client.Acceptance(context.Background())
	if err != nil || len(got.Checks) != 1 || got.Checks[0].DurationMS == nil || *got.Checks[0].DurationMS != 12.25 {
		t.Fatalf("acceptance = %+v, %v", got, err)
	}
}

func TestRunPollsTerminalWithoutRepeatingPost(t *testing.T) {
	var posts, gets atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing job request authentication")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			posts.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"suite":"smoke"}` {
				t.Errorf("unexpected start body %s", body)
			}
			w.WriteHeader(http.StatusAccepted)
			writeJob(w, "pending")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job-001":
			if gets.Add(1) == 1 {
				writeJob(w, "running")
			} else {
				writeJob(w, "completed")
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	job, err := client.Run(context.Background(), "smoke")
	if err != nil || job.Status != "completed" || posts.Load() != 1 || gets.Load() != 2 {
		t.Fatalf("run = %+v, %v; posts/gets=%d/%d", job, err, posts.Load(), gets.Load())
	}
}

func TestRunInvalidSuiteDoesNotRequest(t *testing.T) {
	var calls atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, suite := range []string{"", "build", "../endurance", "SMOKE", "custom"} {
		if _, err := client.Run(context.Background(), suite); err == nil {
			t.Errorf("invalid suite accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid suite caused network request")
	}
}

func TestRedirectIsNotFollowedAndBodyNotLeaked(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer other.Close()
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/?"+testToken, http.StatusTemporaryRedirect)
	})
	if _, err := client.SourceHash(context.Background()); err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), other.URL) {
		t.Fatalf("redirect error unsafe or absent: %v", err)
	}
	if reached.Load() != 0 {
		t.Fatal("followed redirect")
	}
}

func TestStrictJSONAndSize(t *testing.T) {
	for name, body := range map[string]string{
		"unknown_field": `{"source_sha256":"` + testHash + `","token":"` + testToken + `"}`,
		"trailing":      `{"source_sha256":"` + testHash + `"} {}`,
		"duplicate":     `{"source_sha256":"` + testHash + `","source_sha256":"` + testHash + `"}`,
		"null":          `null`, "bad_hash": `{"source_sha256":"bad"}`,
		"too_large": `{"source_sha256":"` + strings.Repeat("a", 1024*1024) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if _, err := client.SourceHash(context.Background()); err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatalf("unsafe or missing parse error: %v", err)
			}
		})
	}
}

func TestCancellationCancelsKnownJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancellations atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/jobs":
			w.WriteHeader(http.StatusAccepted)
			writeJob(w, "running")
		case r.URL.Path == "/v1/jobs/job-001/cancel" && r.Method == http.MethodPost:
			cancellations.Add(1)
			writeJob(w, "cancelled")
		default:
			cancel()
			<-r.Context().Done()
		}
	})
	_, err := client.Run(ctx, "smoke")
	if !errors.Is(err, context.Canceled) || cancellations.Load() != 1 {
		t.Fatalf("cancellation = %v, calls=%d", err, cancellations.Load())
	}
}

func TestAmbiguousStartIsNotRetried(t *testing.T) {
	var posts atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	if _, err := client.Run(context.Background(), "smoke"); err == nil {
		t.Fatal("ambiguous start accepted")
	}
	if posts.Load() != 1 {
		t.Fatalf("ambiguous POST retried %d times", posts.Load())
	}
}

func TestJobTextNeverContainsToken(t *testing.T) {
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"job_id":"job-001","suite":"smoke","status":"failed","source_sha256":%q,"source_changed":false,"result":{"checks":[{"id":"smoke","status":"error","exit_code":null,"duration_ms":null,"artifact_sha256":null,"details":{"nested":[%q,{%q:%q}]}}]},"error":{"code":"FAILED","message":%q}}`, testHash, testToken, testToken, "Bearer "+testToken, testToken)
	})
	job, err := client.Run(context.Background(), "smoke")
	if err != nil || job.Status != "failed" {
		t.Fatalf("failed job not preserved: %+v %v", job, err)
	}
	encoded, _ := json.Marshal(job)
	if strings.Contains(string(encoded), testToken) {
		t.Fatal("token leaked in returned DTO")
	}
}

func TestUnknownJobStatesAndUnsafeIDsFailClosed(t *testing.T) {
	for _, state := range []string{"done", "passed", "environment_blocked", "complete", "", "mystery"} {
		t.Run("state_"+state, func(t *testing.T) {
			var starts, cancellations atomic.Int32
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					cancellations.Add(1)
					writeJob(w, "cancelled")
					return
				}
				starts.Add(1)
				writeJob(w, state)
			})
			if _, err := client.Run(context.Background(), "smoke"); err == nil {
				t.Fatal("unknown state accepted")
			}
			if starts.Load() != 1 || cancellations.Load() != 1 {
				t.Fatalf("unknown-state lifecycle requests: %d/%d", starts.Load(), cancellations.Load())
			}
		})
	}
	for _, id := range []string{"", "../other", "x/y", "x?token=secret", strings.Repeat("a", 129)} {
		t.Run("id_"+id, func(t *testing.T) {
			var calls atomic.Int32
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"job_id":%q,"suite":"smoke","status":"running","source_sha256":null,"source_changed":false,"result":{"checks":[]},"error":null}`, id)
			})
			if _, err := client.Run(context.Background(), "smoke"); err == nil || calls.Load() != 1 {
				t.Fatalf("unsafe ID used: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestPollingCannotSwitchJobOrSuite(t *testing.T) {
	for _, mutate := range []string{"id", "suite"} {
		t.Run(mutate, func(t *testing.T) {
			var cancelled atomic.Int32
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/jobs":
					writeJob(w, "pending")
				case r.URL.Path == "/v1/jobs/job-001/cancel":
					cancelled.Add(1)
					writeJob(w, "cancelled")
				default:
					id, suite := "job-001", "smoke"
					if mutate == "id" {
						id = "other-job"
					} else {
						suite = "endurance"
					}
					fmt.Fprintf(w, `{"job_id":%q,"suite":%q,"status":"completed","source_sha256":%q,"source_changed":false,"result":{"checks":[]},"error":null}`, id, suite, testHash)
				}
			})
			if _, err := client.Run(context.Background(), "smoke"); err == nil || cancelled.Load() != 1 {
				t.Fatalf("mismatched job not rejected/cancelled: %v", err)
			}
		})
	}
}

func TestClientHasNoProxyReplayOrCredentialFormatting(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1234", testToken)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || !transport.DisableKeepAlives || client.http.Timeout != 10*time.Second {
		t.Fatal("proxy, replay or HTTP time limit configuration is unsafe")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v %+v %#v", client, client, client, *client, *client), testToken) {
		t.Fatal("Client formatting exposes token")
	}
}

func TestCancellationCleanupHasIndependentThreeSecondBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cleanup atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cleanup.Add(1)
			_, _ = io.Copy(io.Discard, r.Body) // Allow the HTTP server to observe client disconnect.
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodGet {
			cancel()
			<-r.Context().Done()
			return
		}
		writeJob(w, "running")
	})
	start := time.Now()
	_, err := client.Run(ctx, "smoke")
	if !errors.Is(err, context.Canceled) || cleanup.Load() != 1 || time.Since(start) < 2800*time.Millisecond || time.Since(start) > 5*time.Second {
		t.Fatalf("cleanup not independently bounded: duration=%s calls=%d err=%v", time.Since(start), cleanup.Load(), err)
	}
}

func TestUnknownFieldsAndMissingRequiredJobShape(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":        `{"job_id":"job-001","suite":"smoke","status":"completed","source_sha256":null,"source_changed":false,"result":{"checks":[]},"error":null,"extra":true}`,
		"null_result":    `{"job_id":"job-001","suite":"smoke","status":"completed","source_sha256":null,"source_changed":false,"result":null,"error":null}`,
		"missing_result": `{"job_id":"job-001","suite":"smoke","status":"completed","source_sha256":null,"source_changed":false,"error":null}`,
		"null_checks":    `{"job_id":"job-001","suite":"smoke","status":"completed","source_sha256":null,"source_changed":false,"result":{"checks":null},"error":null}`,
		"null_changed":   `{"job_id":"job-001","suite":"smoke","status":"completed","source_sha256":null,"source_changed":null,"result":{"checks":[]},"error":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					writeJob(w, "cancelled")
					return
				}
				io.WriteString(w, body)
			})
			if _, err := client.Run(context.Background(), "smoke"); err == nil {
				t.Fatal("invalid job shape accepted")
			}
		})
	}
}

func TestHTTPFailureNeverEchoesServerBody(t *testing.T) {
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "private-server-body "+testToken)
	})
	if _, err := client.Acceptance(context.Background()); err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "private-server-body") {
		t.Fatalf("unsafe or missing HTTP error: %v", err)
	}
}

func TestHTTPDeadlineAndAlreadyCancelledContext(t *testing.T) {
	var calls atomic.Int32
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.SourceHash(ctx); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("cancelled request dispatched: calls=%d err=%v", calls.Load(), err)
	}
	// Production timeout is asserted as ten seconds in the configuration test.
	client.http.Timeout = 20 * time.Millisecond
	start := time.Now()
	if _, err := client.SourceHash(context.Background()); err == nil || time.Since(start) > time.Second {
		t.Fatalf("HTTP request was not bounded: %v", err)
	}
}

func TestEvidenceRejectsUnknownStatusesAndMalformedChecks(t *testing.T) {
	for name, check := range map[string]string{
		"unknown_status":    `{"id":"smoke","status":"pass","exit_code":0,"duration_ms":1,"artifact_sha256":null,"details":null}`,
		"negative_duration": `{"id":"smoke","status":"passed","exit_code":0,"duration_ms":-1,"artifact_sha256":null,"details":null}`,
		"missing_duration":  `{"id":"smoke","status":"passed","exit_code":0,"artifact_sha256":null,"details":null}`,
		"bad_artifact":      `{"id":"smoke","status":"passed","exit_code":0,"duration_ms":1,"artifact_sha256":"bad","details":null}`,
		"unknown_field":     `{"id":"smoke","status":"passed","exit_code":0,"duration_ms":1,"artifact_sha256":null,"details":null,"extra":true}`,
		"null_check":        `null`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"source_sha256":%q,"checks":[%s]}`, testHash, check)
			})
			if _, err := client.Acceptance(context.Background()); err == nil {
				t.Fatal("malformed check accepted")
			}
		})
	}
}

func TestConcurrentClientReadOperations(t *testing.T) {
	client := newFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"source_sha256":%q}`, testHash)
	})
	var tasks sync.WaitGroup
	for range 12 {
		tasks.Go(func() {
			if hash, err := client.SourceHash(context.Background()); err != nil || hash != testHash {
				t.Errorf("concurrent read failed: %v", err)
			}
		})
	}
	tasks.Wait()
}

func TestNumericDetailsCannotEchoNumericToken(t *testing.T) {
	const numericToken = "1234567890123456789012345678901234"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"source_sha256":%q,"checks":[{"id":"smoke","status":"error","exit_code":null,"duration_ms":null,"artifact_sha256":null,"details":%s}]}`, testHash, numericToken)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, numericToken)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Acceptance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), numericToken) {
		t.Fatal("numeric token leaked through JSON number")
	}
}
