package tui

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func (m *Model) openConfigScreen() {
	s := screen.NewConfigScreen(m.configSettingsSnapshot())
	s.Resize(m.width, m.height)
	m.activeScreen = s
}

func (m *Model) configSettingsSnapshot() []screen.ConfigSetting {
	running := m.cfg
	if running == nil {
		running = &config.Config{}
	}
	configured := running
	// Restart-bound settings should show the value already persisted for the
	// next process, while EffectiveValue remains the current runtime snapshot.
	// A failed load is handled conservatively below by locking affected rows;
	// never replace the known-good running snapshot with a partial config.
	if loaded, _, err := config.Load(); err == nil && loaded != nil {
		configured = loaded
	}

	permissionValue := running.Permission.Mode
	if m.gate != nil {
		permissionValue = string(m.gate.Mode())
	}
	if _, ok := permission.ParseMode(permissionValue); !ok {
		permissionValue = string(permission.ModeDefault)
	}

	settings := []screen.ConfigSetting{
		{Key: "permission.mode", Label: "Permission mode", Description: "Default tool approval policy", Value: permissionValue, EffectiveValue: permissionValue, Options: []string{"default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"}},
		{Key: "ui.thinking_display", Label: "Thinking display", Description: "Show, summarize, or hide provider reasoning", Value: configuredThinkingDisplay(running), EffectiveValue: m.thinkingDisplay, Options: []string{"show", "auto", "hide"}},
		{Key: "ui.permission_timeout_seconds", Label: "Permission timeout", Description: "Seconds before an unanswered prompt denies (next launch)", Value: strconv.Itoa(configured.UI.PermissionTimeoutSeconds), EffectiveValue: strconv.Itoa(running.UI.PermissionTimeoutSeconds), RestartRequired: true},
		{Key: "session.auto_compact_threshold", Label: "Auto-compact threshold", Description: "Context fraction that triggers compaction on next launch", Value: strconv.FormatFloat(configured.Session.AutoCompactThreshold, 'g', -1, 64), EffectiveValue: strconv.FormatFloat(running.Session.AutoCompactThreshold, 'g', -1, 64), RestartRequired: true},
		{Key: "session.auto_compact_minimum_tokens", Label: "Compact minimum tokens", Description: "Absolute auto-compaction floor on next launch", Value: strconv.Itoa(configured.Session.AutoCompactMinimumTokens), EffectiveValue: strconv.Itoa(running.Session.AutoCompactMinimumTokens), RestartRequired: true},
		{Key: "session.max_iterations", Label: "Max iterations", Description: "Maximum tool-loop iterations per turn on next launch", Value: strconv.Itoa(configured.Session.MaxIterations), EffectiveValue: strconv.Itoa(running.Session.MaxIterations), RestartRequired: true},
		{Key: "loop_detection.disabled", Label: "Loop detection disabled", Description: "Repeated-tool loop detection on next launch", Value: strconv.FormatBool(configured.LoopDetection.Disabled), EffectiveValue: strconv.FormatBool(running.LoopDetection.Disabled), Options: []string{"false", "true"}, RestartRequired: true},
		{Key: "ui.performance.reduced_motion", Label: "Reduced motion", Description: "Reduce animation and redraw frequency", Value: strconv.FormatBool(running.UI.Performance.ReducedMotion), EffectiveValue: strconv.FormatBool(running.UI.Performance.ReducedMotion), Options: []string{"false", "true"}},
		{Key: "ui.performance.mouse_wheel_lines", Label: "Mouse wheel lines", Description: "Transcript lines per wheel event", Value: strconv.Itoa(running.UI.Performance.MouseWheelLines), EffectiveValue: strconv.Itoa(running.UI.Performance.MouseWheelLines)},
		{Key: "ui.performance.max_mounted_items", Label: "Mounted item limit", Description: "Bound rendered transcript items (0 unlimited)", Value: strconv.Itoa(running.UI.Performance.MaxMountedItems), EffectiveValue: strconv.Itoa(running.UI.Performance.MaxMountedItems)},
	}
	for i := range settings {
		source, err := config.UserSettingOverrideSource(settings[i].Key)
		switch {
		case err != nil:
			settings[i].LockedReason = "project config is unreadable; open the editor"
		case source != "":
			settings[i].LockedReason = "controlled by " + source
		}
		if settings[i].LockedReason == "" {
			switch settings[i].Key {
			case "ui.performance.reduced_motion":
				if reducedMotionEnabled {
					settings[i].LockedReason = "controlled by METIS_REDUCED_MOTION or NO_MOTION"
				}
			case "ui.performance.mouse_wheel_lines":
				if raw := os.Getenv("METIS_MOUSE_WHEEL_LINES"); raw != "" {
					if n, parseErr := strconv.Atoi(raw); parseErr == nil && n >= 1 && n <= 50 {
						settings[i].LockedReason = "controlled by METIS_MOUSE_WHEEL_LINES"
					}
				}
			}
		}
	}
	return settings
}

func (m *Model) applyConfigScreen(w *screen.ConfigScreen) tea.Cmd {
	if w.OpenEditorRequested() {
		return m.openConfigEditor()
	}
	if !w.Applied() {
		m.messages = append(m.messages, Message{Role: "info", Content: "(config dialog dismissed — no changes)", Timestamp: time.Now()})
		return nil
	}
	changes := w.Changes()
	if len(changes) == 0 {
		m.messages = append(m.messages, Message{Role: "info", Content: "(config unchanged)", Timestamp: time.Now()})
		return nil
	}
	settings := make([]config.UserSetting, len(changes))
	for i, change := range changes {
		settings[i] = config.UserSetting{Key: change.Key, Value: change.Value}
	}
	var previousPermissionState rtpkg.PermissionModeState
	permissionApplied := false
	for _, change := range changes {
		if change.Key != "permission.mode" || m.gate == nil {
			continue
		}
		var err error
		previousPermissionState, err = rtpkg.CapturePermissionModeState(m.gate, m.loop)
		if err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: "permission mode unchanged: " + err.Error(), Timestamp: time.Now()})
			return nil
		}
		if err := applyModelPermissionMode(m, permission.CanonicalMode(change.Value)); err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: "permission mode unchanged: " + err.Error(), Timestamp: time.Now()})
			return nil
		}
		permissionApplied = true
		break
	}
	loaded, err := config.SaveUserSettingsAndLoad(settings)
	if err != nil {
		var rollbackErr error
		if permissionApplied {
			rollbackErr = rtpkg.RestorePermissionModeState(previousPermissionState, func(mode permission.Mode) error {
				return applyModelPermissionMode(m, mode)
			})
		}
		message := "config save failed: " + err.Error()
		if rollbackErr != nil {
			message += "; permission rollback failed: " + rollbackErr.Error()
		}
		m.messages = append(m.messages, Message{Role: "error", Content: message, Timestamp: time.Now()})
		return nil
	}
	live := m.applyLiveConfigChanges(loaded, changes)
	restart := len(changes) - live
	message := ""
	switch {
	case live > 0 && restart > 0:
		message = fmt.Sprintf("config saved · %d applied now · %d will apply after restart", live, restart)
	case live > 0:
		message = fmt.Sprintf("config saved · %d setting(s) applied now", live)
	default:
		message = fmt.Sprintf("config saved · %d setting(s) will apply after restart", restart)
	}
	m.messages = append(m.messages, Message{Role: "success", Content: message, Timestamp: time.Now()})
	return nil
}

func (m *Model) applyLiveConfigChanges(candidate *config.Config, changes []screen.ConfigChange) int {
	if candidate == nil {
		return 0
	}
	if m.cfg == nil {
		m.cfg = &config.Config{}
	}
	live := 0
	perfChanged := false
	for _, change := range changes {
		switch change.Key {
		case "permission.mode":
			mode := permission.CanonicalMode(candidate.Permission.Mode)
			if m.gate != nil && m.gate.Mode() != mode {
				if err := applyModelPermissionMode(m, mode); err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "permission mode unchanged: " + err.Error(), Timestamp: time.Now()})
					continue
				}
			}
			m.cfg.Permission.Mode = candidate.Permission.Mode
			live++
		case "ui.thinking_display":
			m.cfg.UI.ThinkingDisplay = candidate.UI.ThinkingDisplay
			m.thinkingDisplay = configuredThinkingDisplay(candidate)
			live++
		case "ui.performance.reduced_motion":
			m.cfg.UI.Performance.ReducedMotion = candidate.UI.Performance.ReducedMotion
			perfChanged = true
			live++
		case "ui.performance.mouse_wheel_lines":
			m.cfg.UI.Performance.MouseWheelLines = candidate.UI.Performance.MouseWheelLines
			perfChanged = true
			live++
		case "ui.performance.max_mounted_items":
			m.cfg.UI.Performance.MaxMountedItems = candidate.UI.Performance.MaxMountedItems
			perfChanged = true
			live++
		}
	}
	if perfChanged {
		SetPerfConfig(PerfConfig{
			TickMs:          m.cfg.UI.Performance.TickMs,
			EventBufferSize: m.cfg.UI.Performance.EventBufferSize,
			MouseWheelLines: m.cfg.UI.Performance.MouseWheelLines,
			ReducedMotion:   m.cfg.UI.Performance.ReducedMotion,
			SlowRenderMs:    m.cfg.UI.Performance.SlowRenderMs,
			StatsLogEvery:   m.cfg.UI.Performance.StatsLogEvery,
			MaxMountedItems: m.cfg.UI.Performance.MaxMountedItems,
			ScrollQuantum:   m.cfg.UI.Performance.ScrollQuantum,
		})
		if m.chatList != nil {
			m.chatList.SetMouseWheelDelta(mouseWheelLines())
			m.chatList.SetMaxMounted(m.cfg.UI.Performance.MaxMountedItems)
		}
	}
	return live
}
