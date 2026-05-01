// Package runtime — host-level schedule installer (HIDDEN).
//
// This is the metis equivalent of claude-code's `/schedule` (which uses
// Anthropic's CCR cloud). Since metis has no cloud, the same "runs even
// when machine is off" outcome is approximated by writing host
// scheduler config:
//
//   - macOS: ~/Library/LaunchAgents/com.metis.cron.<id>.plist (LaunchAgent —
//     fires at user login + on schedule; if the Mac sleeps, fires on next wake)
//   - Linux: ~/.config/systemd/user/metis-cron-<id>.{service,timer}
//     (systemd user timer — Persistent=true catches missed runs)
//
// IMPORTANT: This file is intentionally NOT wired into cmd dispatch /
// printUsage. It's a complete subsystem we want to ship the code for so
// future expose is one line, but we don't want users discovering it
// today. To expose: add `case "schedule": return cmdSchedule(...)` to
// dispatch and document it.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// HostScheduleSpec is the input to ScheduleInstall.
//
// ID — short opaque identifier; used to construct file names and
// uninstall lookups. Caller's job to make it unique within metis.
//
// MetisBinary — absolute path to the metis executable to invoke. Usually
// os.Executable() resolved by the caller.
//
// JobID — the metis cron job id (i.e. CronJob.ID). The host schedule
// fires `metis cron run <JobID>` so the actual prompt + schedule live
// in metis's own state, not in the host config.
//
// Cron — UTC 5-field cron expression (macOS LaunchAgent translates the
// fields one-by-one; Linux systemd uses the OnCalendar form derived
// from this).
//
// Description — human-readable tag for the LaunchAgent / Service Unit.
type HostScheduleSpec struct {
	ID          string
	MetisBinary string
	JobID       string
	Cron        string
	Description string
}

// ScheduleInstall writes the platform-appropriate scheduler config and
// loads it. Returns the path of the installed file so callers can echo
// it (and so the test harness can introspect).
func ScheduleInstall(spec HostScheduleSpec) (string, error) {
	if err := validateScheduleSpec(spec); err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(spec)
	case "linux":
		return installSystemdUserTimer(spec)
	default:
		return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// ScheduleUninstall removes a previously-installed schedule by ID.
func ScheduleUninstall(id string) error {
	if !validScheduleID(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchAgent(id)
	case "linux":
		return uninstallSystemdUserTimer(id)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// ScheduleList returns every metis-managed schedule on the host.
// Each element is the *file path* of the unit / plist; callers can stat
// or read to render whatever detail they want.
func ScheduleList() ([]string, error) {
	switch runtime.GOOS {
	case "darwin":
		return listLaunchAgents()
	case "linux":
		return listSystemdUnits()
	default:
		return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func validateScheduleSpec(s HostScheduleSpec) error {
	if !validScheduleID(s.ID) {
		return fmt.Errorf("invalid id %q (use [a-zA-Z0-9._-])", s.ID)
	}
	if s.MetisBinary == "" {
		return fmt.Errorf("MetisBinary is required")
	}
	if s.JobID == "" {
		return fmt.Errorf("JobID is required")
	}
	if s.Cron == "" {
		return fmt.Errorf("Cron is required")
	}
	if !filepath.IsAbs(s.MetisBinary) {
		return fmt.Errorf("MetisBinary must be absolute, got %q", s.MetisBinary)
	}
	return nil
}

func validScheduleID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// ---- macOS LaunchAgent ----

const launchAgentTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.metis.cron.{{.ID}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.MetisBinary}}</string>
    <string>cron</string>
    <string>run</string>
    <string>{{.JobID}}</string>
  </array>
  <key>StartCalendarInterval</key>
  {{.CalendarInterval}}
  <key>RunAtLoad</key>
  <false/>
  <key>StandardOutPath</key>
  <string>{{.LogPath}}</string>
  <key>StandardErrorPath</key>
  <string>{{.LogPath}}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
  </dict>
</dict>
</plist>
`

func launchAgentDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func launchAgentPath(id string) string {
	return filepath.Join(launchAgentDir(), "com.metis.cron."+id+".plist")
}

func installLaunchAgent(spec HostScheduleSpec) (string, error) {
	if err := os.MkdirAll(launchAgentDir(), 0o755); err != nil {
		return "", err
	}
	calInt, err := cronToCalendarInterval(spec.Cron)
	if err != nil {
		return "", err
	}
	tmpl, _ := template.New("plist").Parse(launchAgentTemplate)
	logPath := filepath.Join(os.TempDir(), "metis-schedule-"+spec.ID+".log")
	var sb strings.Builder
	if err := tmpl.Execute(&sb, struct {
		ID, MetisBinary, JobID, CalendarInterval, LogPath string
	}{spec.ID, spec.MetisBinary, spec.JobID, calInt, logPath}); err != nil {
		return "", err
	}
	out := launchAgentPath(spec.ID)
	if err := os.WriteFile(out, []byte(sb.String()), 0o644); err != nil {
		return "", err
	}
	// load on darwin (best-effort — file alone schedules at next login).
	_ = exec.Command("launchctl", "unload", out).Run() // tolerate "not loaded"
	if err := exec.Command("launchctl", "load", out).Run(); err != nil {
		return out, fmt.Errorf("launchctl load: %w (file written to %s)", err, out)
	}
	return out, nil
}

func uninstallLaunchAgent(id string) error {
	p := launchAgentPath(id)
	_ = exec.Command("launchctl", "unload", p).Run()
	return os.Remove(p)
}

func listLaunchAgents() ([]string, error) {
	dir := launchAgentDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "com.metis.cron.") && strings.HasSuffix(name, ".plist") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// cronToCalendarInterval translates a 5-field UTC cron expression to the
// LaunchAgent <dict>...<dict> body. Only handles the simple forms we use
// in /loop / /schedule today (`0 9 * * *`, `*/5 * * * *`, etc). Falls
// back to "every minute" if the expression is too complex.
func cronToCalendarInterval(cron string) (string, error) {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return "", fmt.Errorf("cron must be 5 fields, got %d (%q)", len(parts), cron)
	}
	min, hr, dom, mon, dow := parts[0], parts[1], parts[2], parts[3], parts[4]
	keys := []struct {
		Key   string
		Value string
	}{
		{"Minute", cronFieldToValue(min)},
		{"Hour", cronFieldToValue(hr)},
		{"Day", cronFieldToValue(dom)},
		{"Month", cronFieldToValue(mon)},
		{"Weekday", cronFieldToValue(dow)},
	}
	var sb strings.Builder
	sb.WriteString("<dict>\n")
	for _, kv := range keys {
		if kv.Value == "" {
			continue
		}
		fmt.Fprintf(&sb, "    <key>%s</key>\n    <integer>%s</integer>\n", kv.Key, kv.Value)
	}
	sb.WriteString("  </dict>")
	return sb.String(), nil
}

// cronFieldToValue picks an integer for a cron field, or returns "" to
// drop the field (which makes LaunchAgent treat it as "any").
func cronFieldToValue(f string) string {
	if f == "*" {
		return ""
	}
	// "*/N" → first sample N (LaunchAgent has no native step support;
	// approximating by picking the first value loses fidelity but keeps
	// the schedule firing at least once per period).
	if strings.HasPrefix(f, "*/") {
		return strings.TrimPrefix(f, "*/")
	}
	// Single integer — keep it.
	for _, r := range f {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return f
}

// ---- Linux systemd user timer ----

const systemdServiceTemplate = `[Unit]
Description=metis cron job {{.JobID}} ({{.Description}})

[Service]
Type=oneshot
ExecStart={{.MetisBinary}} cron run {{.JobID}}
`

const systemdTimerTemplate = `[Unit]
Description=metis cron schedule for {{.JobID}} ({{.Description}})

[Timer]
OnCalendar={{.OnCalendar}}
Persistent=true
Unit=metis-cron-{{.ID}}.service

[Install]
WantedBy=timers.target
`

func systemdUserDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

func systemdServicePath(id string) string {
	return filepath.Join(systemdUserDir(), "metis-cron-"+id+".service")
}

func systemdTimerPath(id string) string {
	return filepath.Join(systemdUserDir(), "metis-cron-"+id+".timer")
}

func installSystemdUserTimer(spec HostScheduleSpec) (string, error) {
	if err := os.MkdirAll(systemdUserDir(), 0o755); err != nil {
		return "", err
	}
	cal, err := cronToOnCalendar(spec.Cron)
	if err != nil {
		return "", err
	}
	stmpl, _ := template.New("svc").Parse(systemdServiceTemplate)
	ttmpl, _ := template.New("tmr").Parse(systemdTimerTemplate)
	var s, t strings.Builder
	if err := stmpl.Execute(&s, spec); err != nil {
		return "", err
	}
	if err := ttmpl.Execute(&t, struct {
		HostScheduleSpec
		OnCalendar string
	}{spec, cal}); err != nil {
		return "", err
	}
	if err := os.WriteFile(systemdServicePath(spec.ID), []byte(s.String()), 0o644); err != nil {
		return "", err
	}
	tp := systemdTimerPath(spec.ID)
	if err := os.WriteFile(tp, []byte(t.String()), 0o644); err != nil {
		return "", err
	}
	// Best-effort enable.
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("systemctl", "--user", "enable", "--now", "metis-cron-"+spec.ID+".timer").Run()
	return tp, nil
}

func uninstallSystemdUserTimer(id string) error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", "metis-cron-"+id+".timer").Run()
	if err := os.Remove(systemdTimerPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(systemdServicePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func listSystemdUnits() ([]string, error) {
	dir := systemdUserDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "metis-cron-") && strings.HasSuffix(name, ".timer") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// cronToOnCalendar converts a 5-field cron expression to systemd's
// OnCalendar format. Same simplified handling as the LaunchAgent path.
func cronToOnCalendar(cron string) (string, error) {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return "", fmt.Errorf("cron must be 5 fields, got %d (%q)", len(parts), cron)
	}
	// systemd OnCalendar: DayOfWeek YYYY-MM-DD HH:MM:SS
	min, hr := parts[0], parts[1]
	if min == "*" {
		min = "*"
	}
	if hr == "*" {
		hr = "*"
	}
	// Drop dom/mon/dow specificity for now — supporting */N requires
	// transforming to systemd "0/N" syntax which differs by field.
	return fmt.Sprintf("*-*-* %s:%s:00", hr, min), nil
}
