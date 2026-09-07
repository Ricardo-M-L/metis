package runtime

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

type providerWithoutRecoveryMarker struct{ llm.Provider }

func TestVisionOverrideForwardsRecoveryOwnership(t *testing.T) {
	responses := openai.NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
	for _, vision := range []bool{false, true} {
		wrapped := withVisionOverride(responses, vision)
		managed, ok := wrapped.(interface{ ManagesRecoverySession() bool })
		if !ok || !managed.ManagesRecoverySession() {
			t.Fatalf("Responses recovery marker lost through %T", wrapped)
		}
		plain := withVisionOverride(providerWithoutRecoveryMarker{}, vision)
		if managed, ok := plain.(interface{ ManagesRecoverySession() bool }); ok && managed.ManagesRecoverySession() {
			t.Fatal("plain provider incorrectly claims HTTP recovery")
		}
	}
}
