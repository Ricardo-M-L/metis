package openai

import (
	"context"
	"fmt"
	"github.com/Ricardo-M-L/metis/pkg/provider"
	"net/http"
	"strings"
	"testing"
)

func TestToolCountAdmissionBoundariesAndNoHTTPOnOverflow(t *testing.T) {
	for _, n := range []int{127, 128, 129} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			specs := make([]provider.ToolSpec, n)
			for i := range specs {
				specs[i] = provider.ToolSpec{Name: fmt.Sprintf("t%d", i), InputSchema: map[string]any{"type": "object"}}
			}
			if err := provider.ValidateToolDefinitions(specs, 128); (err != nil) != (n > 128) {
				t.Fatalf("wrong admission: %v", err)
			}
			r := &Responses{}
			body, err := r.buildResponsesRequest(provider.Request{Tools: specs})
			if (err != nil) != (n > 128) {
				t.Fatalf("Responses admission: %v", err)
			}
			if err == nil && len(body.Tools) != n {
				t.Fatal("tools silently truncated")
			}
			if n <= 128 {
				return
			}
			calls := 0
			o := &OpenAI{APIKey: "test", httpClient: &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("must not send") })}}
			_, completeErr := o.Complete(context.Background(), provider.Request{Tools: specs})
			_, streamErr := o.Stream(context.Background(), provider.Request{Tools: specs})
			for _, err := range []error{completeErr, streamErr} {
				if err == nil || !strings.Contains(err.Error(), "tool definition limit exceeded") {
					t.Fatalf("missing local admission: %v", err)
				}
			}
			if calls != 0 {
				t.Fatal("overflow reached HTTP")
			}
		})
	}
}

func TestResponsesCountIncludesHostedTools(t *testing.T) {
	r := &Responses{HostedTools: []string{"web_search"}}
	r.Capabilities.HostedTools = true
	for _, n := range []int{127, 128} {
		body, err := r.buildResponsesRequest(provider.Request{Tools: make([]provider.ToolSpec, n)})
		if n == 127 && (err != nil || len(body.Tools) != 128) {
			t.Fatalf("valid total 128 rejected: %v", err)
		}
		if n == 128 && err == nil {
			t.Fatal("hosted tool bypassed cap")
		}
	}
}

func TestResponsesFinalWireCountCannotExceedLimit(t *testing.T) {
	r := &Responses{HostedTools: make([]string, 128)}
	r.Capabilities.HostedTools = true
	for i := range r.HostedTools {
		r.HostedTools[i] = "web_search"
	}
	_, err := r.buildResponsesRequest(provider.Request{Tools: []provider.ToolSpec{{Name: "Read"}}})
	if err == nil {
		t.Fatal("adapter-owned entries bypassed final wire count")
	}
}
