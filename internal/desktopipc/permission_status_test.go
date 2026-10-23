package desktopipc

import (
	"strings"
	"testing"
)

func TestPermissionStatusRequiresCompleteExplicitState(t *testing.T) {
	for _, tc := range []struct {
		state string
		valid bool
	}{
		{``, true},
		{`,"permission":{"sessionId":"s","mode":"plan","prePlanMode":"fullAccess"}`, true},
		{`,"permission":{"sessionId":"s","mode":"acceptEdits","prePlanMode":""}`, true},
		{`,"permission":{"sessionId":"s","mode":"plan"}`, false},
		{`,"permission":{"sessionId":"s","mode":""}`, false},
		{`,"permission":{"sessionId":"other/path","mode":"default"}`, false},
		{`,"permission":{"sessionId":"s","mode":"fullAccess","prePlanMode":"default"}`, false},
	} {
		message, err := NewDecoder(strings.NewReader(`{"version":1,"type":"status","status":{"subAgents":0` + tc.state + `}}`)).Decode()
		if (err == nil) != tc.valid {
			t.Fatalf("state %s validity=%v, err=%v message=%+v", tc.state, tc.valid, err, message)
		}
	}
}
