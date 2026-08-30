package sandbox

import (
	"strings"
	"testing"
)

func TestManagerFilterEnv(t *testing.T) {
	m, err := NewManagerWithOptions(Options{TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	got := m.FilterEnv([]string{
		"PATH=/bin",
		"OPENAI_API_KEY=secret",
		"AWS_ACCESS_KEY_ID=secret",
		"CUSTOM_TOKEN=secret",
		"TMPDIR=/tmp",
		"TMP=/tmp",
		"TEMP=/tmp",
	}, false)
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"OPENAI_API_KEY=", "AWS_ACCESS_KEY_ID=", "CUSTOM_TOKEN="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("filtered env retained %s:\n%s", forbidden, joined)
		}
	}
	for _, want := range []string{
		"PATH=/bin", "AGENT=metis", "AI_AGENT=metis", "METIS=1",
		"TMPDIR=" + m.TempDir(), "TMP=" + m.TempDir(), "TEMP=" + m.TempDir(),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("filtered env missing %s:\n%s", want, joined)
		}
	}
}

func TestManagerFilterEnvExplicitSecretInheritance(t *testing.T) {
	m, err := NewManagerWithOptions(Options{TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	got := strings.Join(m.FilterEnv([]string{"OPENAI_API_KEY=kept"}, true), "\n")
	if !strings.Contains(got, "OPENAI_API_KEY=kept") {
		t.Fatalf("explicit inheritance dropped credential: %s", got)
	}
}
