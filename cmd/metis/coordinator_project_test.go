package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/exitcode"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestCoordinatorProviderStreamFailureRequiresCompletionBoundaryEvidence(t *testing.T) {
	message := "stream error: stream disconnected before completion: stream closed before response.completed"
	envelope := `{"type":"error","error":{"type":"invalid_request_error","message":"` + message + `"}}`
	stopped, stop := context.WithCancel(context.Background())
	stop()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"observed_sse_error", context.Background(), errors.New(envelope), "provider_stream_interrupted"},
		{"wrapped_sse_error", context.Background(), fmt.Errorf("provider stream reader: %w", errors.New(envelope)), "provider_stream_interrupted"},
		{"completion_marker_missing", context.Background(), errors.New("stream closed before response.completed"), "provider_stream_interrupted"},
		{"plain_completed_boundary", context.Background(), errors.New(message), "provider_stream_interrupted"},
		{"normal_http_400", context.Background(), errors.New("anthropic 400: invalid request"), projectcoord.FailureWorkerExecution},
		{"http_400_body", context.Background(), errors.New("anthropic 400: " + envelope), projectcoord.FailureWorkerExecution},
		{"typed_http_400", context.Background(), &transport.HTTPStatusError{StatusCode: 400, Err: errors.New(envelope)}, projectcoord.FailureWorkerExecution},
		{"typed_auth", context.Background(), &transport.HTTPStatusError{StatusCode: 401, Err: errors.New(message)}, projectcoord.FailureWorkerExecution},
		{"auth_envelope", context.Background(), errors.New(strings.Replace(envelope, "invalid_request_error", "authentication_error", 1)), projectcoord.FailureWorkerExecution},
		{"context_code", context.Background(), errors.New(strings.TrimSuffix(envelope, "}}") + `,"code":"context_length_exceeded"}}`), projectcoord.FailureWorkerExecution},
		{"context_limit", context.Background(), errors.New("context_length_exceeded: " + message), projectcoord.FailureWorkerExecution},
		{"configuration", context.Background(), errors.New("unknown model; stream disconnected"), projectcoord.FailureWorkerExecution},
		{"generic_stream", context.Background(), errors.New("stream error: connection disconnected"), projectcoord.FailureWorkerExecution},
		{"eof_text", context.Background(), errors.New("unexpected EOF"), projectcoord.FailureWorkerExecution},
		{"host_acceptance", context.Background(), errors.New("acceptance check failed: " + envelope), projectcoord.FailureWorkerExecution},
		{"task_deadline", expired, errors.New(envelope), projectcoord.FailureWorkerExecution},
		{"wrapped_deadline", context.Background(), fmt.Errorf("%s: %w", message, context.DeadlineExceeded), projectcoord.FailureWorkerExecution},
		{"service_stop", stopped, errors.New(envelope), projectcoord.FailureWorkerInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordinatorWorkerFailure(tc.ctx, tc.err); got.Code != tc.want {
				t.Fatalf("failure code=%s want=%s", got.Code, tc.want)
			}
		})
	}
}

type coordinatorCompletionMarkerWrapper struct{ cause error }

func (e coordinatorCompletionMarkerWrapper) Error() string {
	return "stream closed before response.completed"
}
func (e coordinatorCompletionMarkerWrapper) Unwrap() error { return e.cause }

func TestCoordinatorProviderStreamInterruptionRejectsWrappedPermanentReasons(t *testing.T) {
	marker := errors.New("stream closed before response.completed")
	for _, reason := range []string{"authentication_error", "permission_error", "invalid_api_key", "context_length_exceeded"} {
		t.Run(reason, func(t *testing.T) {
			err := fmt.Errorf("%s: %w", reason, marker)
			if coordinatorProviderStreamInterrupted(err) {
				t.Fatal("permanent outer reason was bypassed by an inner completion marker")
			}
			if coordinatorProviderStreamInterrupted(fmt.Errorf("provider stream reader: %w", err)) {
				t.Fatal("nested permanent reason was bypassed by an inner completion marker")
			}
			if coordinatorProviderStreamInterrupted(fmt.Errorf("%w (%s)", marker, reason)) {
				t.Fatal("terminal provider error type was bypassed by an inner completion marker")
			}
			if coordinatorProviderStreamInterrupted(coordinatorCompletionMarkerWrapper{cause: errors.New(reason)}) {
				t.Fatal("outer completion marker was accepted before inspecting the permanent cause")
			}
		})
	}
	// These words can occur in unrelated diagnostics; only explicit reason codes
	// at an error boundary, not arbitrary auth/context text, reject the retry.
	for _, context := range []string{"stream reader with authentication configured", "context annotations"} {
		if !coordinatorProviderStreamInterrupted(fmt.Errorf("%s: %w", context, marker)) {
			t.Fatal("unrelated wrapper text masked an explicit completion interruption")
		}
	}
}

func TestCoordinatorSummaryDoesNotExposeGraphOrWorkerOutput(t *testing.T) {
	run := projectcoord.Run{ID: "run", Status: projectcoord.RunBlocked, Goal: "private goal"}
	summary := coordinatorSummary(run, []coordinatorRunOutcome{{Output: "private output", Error: "private details"}})
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"outcomes_count":1,"run_id":"run","status":"blocked"}` {
		t.Fatalf("unexpected service summary: %s", data)
	}
}

func TestCoordinatorServiceCancellationCanRetryWithoutRetryingExecutionErrors(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"service_stop", stopped, context.Canceled, "worker_interrupted"},
		{"wrapped_service_stop", stopped, errors.New("stream interrupted"), "worker_interrupted"},
		{"task_deadline", context.Background(), context.DeadlineExceeded, projectcoord.FailureWorkerExecution},
		{"lost_lease", context.Background(), context.Canceled, projectcoord.FailureWorkerExecution},
		{"provider_failure", context.Background(), errors.New("HTTP 401"), projectcoord.FailureWorkerExecution},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordinatorWorkerFailure(tc.ctx, tc.err).Code; got != tc.want {
				t.Fatalf("failure code=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestWorkerReportedBlockedRequiresStandaloneStatusLine(t *testing.T) {
	if workerReportedBlocked("The instructions say to finish with `METIS_PROJECT_STATUS: blocked` only when blocked.") {
		t.Fatal("quoted prompt marker must not block completed work")
	}
	if !workerReportedBlocked("Could not reach the service.\nMETIS_PROJECT_STATUS: blocked\n") {
		t.Fatal("standalone status marker must block the work item")
	}
	if !workerReportedBlocked("details\n  `METIS_PROJECT_STATUS: blocked` because credentials are unavailable") {
		t.Fatal("indented standalone status marker must block the work item")
	}
}

func TestCoordinatorRunStatusPreservesIncompleteExit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         projectcoord.RunStatus
		outcomes       []coordinatorRunOutcome
		wantIncomplete bool
	}{
		{"completed", projectcoord.RunCompleted, nil, false},
		{"partial_progress", projectcoord.RunActive, []coordinatorRunOutcome{{Status: string(projectcoord.ItemCompleted)}}, false},
		{"blocked", projectcoord.RunBlocked, nil, true},
		{"cancelled", projectcoord.RunCancelled, nil, true},
		{"failed_worker_with_other_ready_work", projectcoord.RunActive, []coordinatorRunOutcome{{Status: string(projectcoord.ItemFailed)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := coordinatorRunStatusError(projectcoord.Run{ID: "run", Status: tc.status}, tc.outcomes)
			if tc.wantIncomplete {
				if code := exitcode.Classify(err); code != exitcode.Incomplete {
					t.Fatalf("exit=%d error=%v", code, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
