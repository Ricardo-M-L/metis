package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func TestFormatJobNotificationsRedactsCommandCredentials(t *testing.T) {
	got := formatJobNotifications([]jobs.Notification{{
		JobID:  "bg_secret",
		Status: jobs.StatusCompleted,
		Command: `curl -H 'Authorization: Bearer background-super-secret' ` +
			`https://example.test?api_key=second-super-secret`,
	}})
	for _, secret := range []string{"background-super-secret", "second-super-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("formatted notification leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("formatted notification did not retain a redaction marker: %s", got)
	}
}
