package main

import (
	"errors"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

const desktopResumeTurnReminder = "继续被用户停止的同一轮任务。历史中已完成的工具调用及结果是已执行事实，不要重放。未确认完成的工具可能已产生外部副作用；先检查当前结果，再决定是否重试。"

// Managed prompts use typed sections as their provider input; custom/legacy
// prompts use System. Update both representations together through the loop's
// synchronized runtime binding, since the Desktop status sampler is active.
func applyDesktopResumeTurnReminder(loop *agent.Loop) {
	snapshot := loop.ProviderRuntimeState()
	system := snapshot.System + "\n\n" + desktopResumeTurnReminder
	sections := snapshot.SystemSections
	if len(sections) > 0 {
		sections = append(sections, llm.SystemSection{Name: "desktop_resume_turn", Body: desktopResumeTurnReminder, Volatile: true})
	}
	loop.RebindProviderRuntime(snapshot.Provider, snapshot.Model, snapshot.MaxOutputTokens, system, sections)
}

// This flag is private to the Desktop worker. Ordinary headless resume still
// requires a prompt, and no startup option may turn this into a fresh session
// or silently supply a new input.
func validateDesktopResumeFlags(flags *cliFlags, rest []string) error {
	if flags == nil || !flags.desktopResumeTurn {
		return nil
	}
	if !flags.desktopWorker || strings.TrimSpace(flags.resumeID) == "" || flags.pickResume {
		return errors.New("--desktop-resume-turn requires --desktop-worker and an explicit nonempty --resume session id")
	}
	if len(rest) != 0 {
		return errors.New("--desktop-resume-turn does not accept a prompt or input")
	}
	if flags.cont || flags.newSessionID != "" || flags.worktree != "" || flags.worktreeOn || flags.agentProfile != "" ||
		flags.useTUI || flags.preflightOnly || flags.dumpPrompt || flags.outputSchema != "" ||
		flags.inputFormat != "" || flags.outputFormat != "" {
		return errors.New("--desktop-resume-turn is incompatible with continue, fresh-session, interactive, or input/output startup options")
	}
	return nil
}
