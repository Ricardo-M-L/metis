package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestViewMouseModeRespectsDisableEnv(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want tea.MouseMode
	}{
		{name: "unset equivalent", env: "", want: tea.MouseModeCellMotion},
		{name: "one", env: "1", want: tea.MouseModeNone},
		{name: "true", env: "true", want: tea.MouseModeNone},
		{name: "yes", env: "yes", want: tea.MouseModeNone},
		{name: "on with whitespace and case", env: " ON ", want: tea.MouseModeNone},
		{name: "other non-empty value", env: "enabled", want: tea.MouseModeNone},
		{name: "zero", env: "0", want: tea.MouseModeCellMotion},
		{name: "false", env: "false", want: tea.MouseModeCellMotion},
		{name: "no", env: "no", want: tea.MouseModeCellMotion},
		{name: "off", env: "off", want: tea.MouseModeCellMotion},
		{name: "n", env: "n", want: tea.MouseModeCellMotion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("METIS_DISABLE_MOUSE", tt.env)
			m := newE2EModel(t, 120, 30, 0)
			if got := m.View().MouseMode; got != tt.want {
				t.Fatalf("MouseMode with METIS_DISABLE_MOUSE=%q = %v; want %v", tt.env, got, tt.want)
			}
		})
	}
}
