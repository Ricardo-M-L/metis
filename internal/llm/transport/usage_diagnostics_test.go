package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSafeUsageObserverDiagnosticKeepsOnlyWhitelistedStageAndCode(t *testing.T) {
	var empty *UsageObserverError
	if SafeUsageObserverClassification(empty) != nil {
		t.Fatal("typed nil observer error must have no safe classification")
	}
	original := &UsageObserverError{Stage: "begin", Code: "provider_usage_unresolved", Err: errors.Join(errors.New("private-auth-and-provider-body"), io.EOF)}
	safe := SafeUsageObserverClassification(original)
	var copied *UsageObserverError
	if !errors.As(safe, &copied) || copied == original || copied.Err != nil || copied.Stage != "begin" || copied.Code != "provider_usage_unresolved" {
		t.Fatalf("unsafe observer classification copy: %#v", safe)
	}
	if errors.Is(safe, io.EOF) || shouldRetry(safe) || strings.Contains(safe.Error(), "private-") {
		t.Fatal("safe observer diagnostics exposed the cause or became a retry signal")
	}
	original.Stage = "private-stage"
	original.Code = "private-code"
	safe = SafeUsageObserverClassification(original)
	if !errors.As(safe, &copied) || copied.Stage != "unknown" || copied.Code != "" || copied.Err != nil || strings.Contains(safe.Error(), "private-") {
		t.Fatal("untrusted diagnostic fields escaped the whitelist")
	}
}

type httpStatusTestRequest struct {
	*usageTestRequest
	codes []int
}

func (r *httpStatusTestRequest) HTTPStatus(code int) error {
	r.codes = append(r.codes, code)
	return nil
}

func TestHTTPStatusObservationIsOptionalAndDoesNotInventUsage(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusForbidden, http.StatusServiceUnavailable} {
		owner := &usageTestObserver{}
		legacy := &usageTestRequest{owner: owner}
		status := &httpStatusTestRequest{usageTestRequest: legacy}
		b := &observedUsageBody{observer: status, protocol: "openai"}
		resp := &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"private-body"}`))}
		if err := b.attach(resp); err != nil {
			t.Fatal(err)
		}
		if len(status.codes) != 1 || status.codes[0] != code || len(legacy.snapshots) != 0 {
			t.Fatalf("status observation invented token evidence: %#v %#v", status.codes, legacy.snapshots)
		}
		if code >= 400 && (len(legacy.statuses) != 1 || legacy.statuses[0] != UsageRejected) {
			t.Fatal("HTTP rejection lifecycle changed")
		}
		_ = resp.Body.Close()
	}
	// Old observers keep the exact original callback set.
	owner := &usageTestObserver{}
	legacy := &usageTestRequest{owner: owner}
	b := &observedUsageBody{observer: legacy}
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private-body"))}
	if err := b.attach(resp); err != nil || len(legacy.statuses) != 1 || len(legacy.snapshots) != 0 {
		t.Fatalf("legacy observer compatibility changed: %v", err)
	}
	_ = resp.Body.Close()
}

func TestHTTPStatusDiagnosticRejectsInvalidStatusWithoutCallingObserver(t *testing.T) {
	status := &httpStatusTestRequest{usageTestRequest: &usageTestRequest{owner: &usageTestObserver{}}}
	b := &observedUsageBody{observer: status}
	err := b.attach(&http.Response{StatusCode: 777, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private-body"))})
	var boundary *UsageObserverError
	if !errors.As(err, &boundary) || boundary.Stage != "response_status" || len(status.codes) != 0 || strings.Contains(err.Error(), "private-") {
		t.Fatalf("invalid HTTP status was persisted or leaked: %v", err)
	}
}
