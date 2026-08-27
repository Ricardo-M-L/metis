package main

import "testing"

func TestShouldEnableAutoMemoryByStartupPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags *cliFlags
		want  bool
	}{
		{
			name:  "desktop defaults on",
			flags: &cliFlags{autoMemoryStartup: autoMemoryStartupDesktop},
			want:  true,
		},
		{
			name:  "interactive TUI defaults on",
			flags: &cliFlags{autoMemoryStartup: autoMemoryStartupInteractive, useTUI: true},
			want:  true,
		},
		{
			name:  "interactive plain REPL defaults on",
			flags: &cliFlags{autoMemoryStartup: autoMemoryStartupInteractive},
			want:  true,
		},
		{
			name:  "headless defaults off",
			flags: &cliFlags{autoMemoryStartup: autoMemoryStartupHeadless},
			want:  false,
		},
		{
			name:  "zero value stays headless",
			flags: &cliFlags{},
			want:  false,
		},
		{
			name:  "legacy flag enables headless",
			flags: &cliFlags{autoMemory: true},
			want:  true,
		},
		{
			name:  "nil flags are disabled",
			flags: nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldEnableAutoMemory(tt.flags, nil); got != tt.want {
				t.Fatalf("shouldEnableAutoMemory() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldEnableAutoMemoryEnvironmentOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flags   *cliFlags
		value   string
		present bool
		want    bool
	}{
		{name: "zero disables desktop", flags: &cliFlags{autoMemoryStartup: autoMemoryStartupDesktop}, value: "0", present: true, want: false},
		{name: "false disables TUI", flags: &cliFlags{autoMemoryStartup: autoMemoryStartupInteractive}, value: " false ", present: true, want: false},
		{name: "off disables legacy flag", flags: &cliFlags{autoMemory: true}, value: "OFF", present: true, want: false},
		{name: "one enables headless", flags: &cliFlags{}, value: "1", present: true, want: true},
		{name: "true enables headless", flags: &cliFlags{}, value: " TRUE ", present: true, want: true},
		{name: "on enables headless", flags: &cliFlags{}, value: "on", present: true, want: true},
		{name: "empty falls back to desktop default", flags: &cliFlags{autoMemoryStartup: autoMemoryStartupDesktop}, value: "", present: true, want: true},
		{name: "unknown falls back to headless default", flags: &cliFlags{}, value: "sometimes", present: true, want: false},
		{name: "unset falls back to interactive default", flags: &cliFlags{autoMemoryStartup: autoMemoryStartupInteractive}, present: false, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lookup := func(key string) (string, bool) {
				if key != "METIS_AUTO_MEMORY" {
					t.Fatalf("unexpected environment lookup %q", key)
				}
				return tt.value, tt.present
			}
			if got := shouldEnableAutoMemory(tt.flags, lookup); got != tt.want {
				t.Fatalf("shouldEnableAutoMemory(METIS_AUTO_MEMORY=%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseFlagsKeepsAutoMemoryCompatibility(t *testing.T) {
	t.Parallel()

	flags, rest, err := parseFlags([]string{"--auto-memory", "prompt"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flags.autoMemory {
		t.Fatal("--auto-memory did not set the compatibility opt-in")
	}
	if flags.autoMemoryStartup != autoMemoryStartupHeadless {
		t.Fatalf("parsed startup path = %d, want conservative headless zero value", flags.autoMemoryStartup)
	}
	if len(rest) != 1 || rest[0] != "prompt" {
		t.Fatalf("remaining args = %q, want [prompt]", rest)
	}
}
