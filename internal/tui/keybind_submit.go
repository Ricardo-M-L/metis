package tui

// keybind_submit.go — handleSubmit + dispatch for slash commands and
// user prompts. Slash commands route through the REPL command registry
// or the slash-signal table; plain user text starts a new turn when idle
// and steers the current turn while one is running.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/desktop"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/themes"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

// splitOffImageBlocks returns (count of image blocks dropped, blocks
// without them). Used by the vision-capability gate when the active
// provider can't accept image content. The text-only remainder still
// goes to the model so the question/context survives.
func splitOffImageBlocks(blocks []llm.ContentBlock) (int, []llm.ContentBlock) {
	out := make([]llm.ContentBlock, 0, len(blocks))
	stripped := 0
	for _, b := range blocks {
		if b.Type == "image" {
			stripped++
			continue
		}
		out = append(out, b)
	}
	return stripped, out
}

// appendImageWarningOnce prevents key-repeat / repeated Enter from filling the
// transcript with the same preflight message while the attachment remains in
// the editor. Refreshing the timestamp is unnecessary: the warning describes
// unchanged state, and retaining its original position keeps the chat stable.
func (m *Model) appendImageWarningOnce(content string) {
	if n := len(m.messages); n > 0 {
		last := m.messages[n-1]
		if last.Role == "warning" && last.Content == content {
			return
		}
	}
	m.messages = append(m.messages, Message{Role: "warning", Content: content, Timestamp: time.Now()})
}

// openBodyScreen wraps screen.NewBodyScreen with a Resize so tests
// (which never receive a real tea.WindowSizeMsg) and the cold-open
// path both render with the right dimensions on the first frame. Real
// runs get a fresh Resize on the next WindowSizeMsg anyway, so this
// is purely a "don't render at width=0,height=0 the first frame" guard.
func (m *Model) openBodyScreen(command, body string) {
	s := screen.NewBodyScreen(command, body)
	s.Resize(m.width, m.height)
	m.activeScreen = s
}

// modalCommands is the set of REPLCommand names whose output should
// open as a full-window modal overlay (BodyScreen) rather than appending
// inline into the chat scroll. Information-dense commands benefit from
// the dedicated screen (scroll, Esc to dismiss); short-status commands
// like /title or /save still append inline. Mirrors claude-code's
// pattern of "modal for browseable content, inline for confirmations".
var modalCommands = map[string]bool{
	"help":   true,
	"doctor": true,
	// "context" / "cost" / "tokens" intentionally NOT modal — claude-code
	// parity (2026-05-11 image #1 for /context, 2026-06-24 image #4 for
	// /cost): short status/info commands render inline as a chat-style info
	// message so the conversation stays visible and the user doesn't need to
	// press Esc to return to typing. Only browsable lists (tools, skills,
	// stats…) and interactive pickers stay modal. The renderers
	// (render_info.go::renderCost etc.) are box-shaped but fit inline fine.
	"stats":       true,
	"keybindings": true,
	"permissions": true,
	"hooks":       true,
	"tools":       true,
	"skills":      true,
	"version":     true,
	"env":         true,
	"agents":      true,
	"files":       true,
	"memory":      true,
	"mcp":         true,
	"sessions":    true,
	"statusline":  true,
	"resume":      true,
	"diff-view":   true,
	"desktop":     true,
}

// promotePaletteSelection applies Enter's palette-selection semantics to the
// submitted text. It deliberately runs before turn-state routing so commands
// selected while an agent is busy behave exactly like commands selected while
// idle. Exact command names are never replaced, and an incomplete prefix is
// promoted only when it identifies one command. With several matches, Tab or
// arrow navigation first writes the chosen canonical name into the input; a
// bare ambiguous prefix such as /s must not silently become /share.
func (m *Model) promotePaletteSelection(text string) string {
	if !m.showPalette || len(m.palMatched) == 0 || !strings.HasPrefix(text, "/") {
		return text
	}

	typedName, restArgs, hasArgs := cut(text[1:], " ")
	exactREPL := m.cmds != nil && m.cmds.Get(typedName) != nil
	exactSlash := false
	if m.slash != nil {
		_, exactSlash = m.slash.Get(typedName)
	}
	if exactREPL || exactSlash {
		return text
	}
	if len(m.palMatched) != 1 {
		return text
	}

	cursor := m.palCursor
	if cursor < 0 || cursor >= len(m.palMatched) {
		cursor = 0
	}
	promoted := "/" + m.palMatched[cursor].Name
	if hasArgs {
		promoted += " " + restArgs
	}
	return promoted
}

// isExportCommand resolves aliases through the REPL registry rather than
// comparing raw text. Export is intentionally the only command dispatched by
// handleSubmit before the active-turn steering branch: it is read-only with
// respect to the live loop and its history snapshot is concurrency-safe.
func (m *Model) isExportCommand(text string) bool {
	if m.cmds == nil || !strings.HasPrefix(text, "/") {
		return false
	}
	name, _, _ := cut(text[1:], " ")
	cmd := m.cmds.Get(name)
	return cmd != nil && cmd.Name == "export"
}

func isThinkingDisplayCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, _, _ := cut(text[1:], " ")
	return strings.EqualFold(name, "thinking")
}

func (m *Model) applyThinkingDisplay(mode string) {
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode != "show" && mode != "hide" && mode != "auto" {
		return
	}
	m.thinkingDisplay = mode
	if m.renderCache != nil {
		m.renderCache.InvalidateAll()
	}
	m.messages = append(m.messages, Message{
		Role:      "info",
		Content:   "thinking display: " + mode,
		Timestamp: time.Now(),
	})
}

// /thinking changes only local presentation state, so it must work while a
// turn is streaming. Routing it through SteerInject made the model receive the
// literal command while the TUI stayed unchanged.
func (m *Model) handleLocalThinkingDisplay(text string) bool {
	if !isThinkingDisplayCommand(text) || m.slash == nil {
		return false
	}
	handled, display, sig, args := m.slash.Parse(text)
	if !handled {
		return false
	}
	m.input.Reset()
	m.showPalette = false
	m.palFilter = ""
	if display != "" {
		m.messages = append(m.messages, Message{Role: "info", Content: display, Timestamp: time.Now()})
	}
	if sig == slash.SignalThinkingDisplay {
		m.applyThinkingDisplay(args)
	}
	return true
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	// Resolve the highlighted slash-palette entry before any mid-turn
	// routing. Previously this happened only after the turnActive branch,
	// so `/expo` + Enter while a turn was closing was treated as literal
	// model input and queued instead of invoking `/export`.
	text = m.promotePaletteSelection(text)
	// Most prompts have identical provider-facing and transcript-facing text.
	// /review is the exception: the model needs a long internal review frame,
	// while the user should see only the command they submitted.
	visibleUserText := ""
	// Custom-command frontmatter may attach request-local permission rules.
	// Keep the derived context local to this submit; ordinary prompts continue
	// using m.ctx and cannot inherit a prior command's approvals.
	turnRunCtx := m.ctx

	// /export is a local snapshot operation. Loop.History returns a locked
	// copy, so it is safe to run while the provider/tool loop is active (or
	// in the one-frame window where the loop has closed steering but the TUI
	// has not received doneCh yet). Handle it before generic steering: sending
	// `/export` to the model made a closing/error turn retain it forever in the
	// ordinary prompt queue. Resetting the editor first also makes key-repeat
	// Enter events idempotent: subsequent empty submits are no-ops.
	if m.isExportCommand(text) {
		m.input.Reset()
		m.showPalette = false
		m.palFilter = ""
		m.stickyBottom = true
		m.messages = append(m.messages, Message{Role: "user", Content: text, Timestamp: time.Now()})

		name, args, _ := cut(text[1:], " ")
		cmd := m.cmds.Get(name)
		if output := cmd.Handler(m.asREPL(), args); output != "" {
			m.messages = append(m.messages, Message{
				Role:      classifyREPLOutput(output),
				Content:   output,
				Timestamp: time.Now(),
			})
		}
		return m, nil
	}
	if m.handleLocalThinkingDisplay(text) {
		return m, nil
	}
	// /agents is a local, read-only view and is safe to open while the
	// agent loop is running. Handle it before the generic mid-turn queue/
	// steering branch; otherwise the literal command is queued as a prompt and
	// the only useful moment for inspecting running sub-agents is lost.
	if isAgentsViewCommand(text) {
		m.input.Reset()
		m.showPalette = false
		m.palFilter = ""
		m.openAgentsView()
		return m, nil
	}
	// Reasoning effort is local runtime state, not model input. Handle it
	// before turnActive routing so `/effort` never becomes a literal steer.
	if m.handleLocalEffortCommand(text) {
		return m, nil
	}
	// `[Image #N]` placeholders STAY in the displayed text — the user
	// pasted those and expects to see them in the transcript. The
	// real image bytes are split out into ContentBlocks at the
	// AppendUserBlocks call site below; here we leave the text alone.
	// Never start a second turn while one is in flight. Mid-turn text is
	// injected into the running loop at its next iteration boundary;
	// commands that explicitly request queueing still use queuedPrompts.
	// This avoids racing a second runTurnAsync goroutine on doneCh while
	// letting follow-up instructions affect work that is already running.
	if m.turnActive && !m.dispatchingMidTurnCommand {
		// A text steer cannot carry multimodal ContentBlocks. The previous
		// path injected the literal "[Image #N]" into the running loop,
		// reset the editor, and left the model to invent a filesystem path.
		// Preserve both editor text and imagePaste until the active turn ends;
		// the next Enter then follows the normal vision-aware submit path.
		if len(m.imagePaste) > 0 {
			model := m.model
			if m.loop != nil && m.loop.Model != "" {
				model = m.loop.Model
			}
			hint := "image kept — wait for the running turn to finish (or press Esc), then press Enter again"
			if m.loop != nil && m.loop.Provider != nil && !pubprov.ProviderSupportsVision(m.loop.Provider) {
				hint = fmt.Sprintf("image kept — current model (%s) is text-only. Wait for the running turn to finish (or press Esc), then press Enter again to choose a vision model; prompt and cached images stay attached", model)
			}
			m.appendImageWarningOnce(hint)
			return m, nil
		}
		// Steering (Task #78) + slash-during-steer (Task #87): mid-turn
		// input is injected into the agent loop and folded into the next
		// iteration's user message. Slash classification:
		//
		//   - Destructive (/clear /new /quit /compact /undo /retry …)
		//     refuse with hint — these would invalidate the running
		//     turn or expect to BE the start of a turn.
		//   - Custom prompt (~/.metis/commands/<name>.md):
		//     resolve the template, SteerInject the resolved TEXT
		//     (not the literal "/intro Chinese" string).
		//   - Anything else (plain text, /cost, /tools, unknown slash):
		//     SteerInject the literal text. Safe-info commands like
		//     /cost don't open their overlay mid-turn in this MVP —
		//     the model just sees them as text in the next iteration.
		//     Fixing that requires running the signal-overlay path
		//     without re-entering runTurnAsync, which is a separate
		//     refactor.
		raw := text
		if raw == "" {
			return m, nil
		}
		// /abort is the command spelling of the single Ctrl-C interrupt. It
		// cancels the live provider/tool context and clears queued follow-ups;
		// sending the literal command to the model cannot abort anything.
		if strings.EqualFold(strings.TrimSpace(raw), "/abort") {
			m.turnCancelledByUser = true
			if m.turnCancel != nil {
				m.turnCancel()
				m.turnCancel = nil
				m.spinnerActive = false
			}
			queueCleared := len(m.queuedPrompts)
			m.queuedPrompts = nil
			m.queuePending = false
			m.input.Reset()
			m.dismissPalette()
			msg := "interrupted"
			if queueCleared > 0 {
				msg = fmt.Sprintf("interrupted · queue cleared (%d dropped)", queueCleared)
			}
			m.messages = append(m.messages, Message{Role: "info", Content: msg, Timestamp: time.Now()})
			return m, nil
		}

		// /now <text> + /later <text> — explicit priority overrides
		// (2026-05-21). Intercepted BEFORE the slash registry so a
		// shadowed real command can't swallow them.
		//
		//   /now   → Priority=Now. Pops before any pending Next batch;
		//            users use this when a follow-up is more urgent
		//            than what they queued earlier ("wait — first try
		//            X instead").
		//   /later → Priority=Later. Drains AFTER every Next item is
		//            done. Lets a user say "also if you have time:"
		//            without bumping into the active follow-up batch.
		//
		// Both reuse the same enqueueQueuedItem path so the queue
		// preview's badge (`! ` for Now, `. ` for Later) and the
		// `(dequeued ×N merged · M remaining)` notice work out of the
		// box. Empty body → refuse with hint instead of queuing the
		// bare command.
		if prio, body, ok := parsePriorityCommand(raw); ok {
			if body == "" {
				m.messages = append(m.messages, Message{
					Role:      "info",
					Content:   "(empty " + prioCommandName(prio) + " — write the message after the command)",
					Timestamp: time.Now(),
				})
				m.input.Reset()
				return m, nil
			}
			m.enqueueQueuedItem(body, prio)
			m.input.Reset()
			return m, nil
		}

		if strings.HasPrefix(raw, "/") && m.slash != nil {
			handled, display, sig, _ := m.slash.Parse(raw)
			if handled {
				switch slash.ClassifyMidTurn(sig) {
				case slash.MidTurnDestructive:
					name, _, _ := strings.Cut(raw[1:], " ")
					m.messages = append(m.messages, Message{
						Role:      "info",
						Content:   "(can't /" + name + " mid-turn — press Esc to cancel the running turn first)",
						Timestamp: time.Now(),
					})
					m.input.Reset()
					return m, nil
				case slash.MidTurnCustom:
					// Custom command's handler already templated the
					// prompt into `display`. Steer THAT, not the
					// literal `/intro Chinese` text.
					customCmd := customCommandFromInput(m.slash, raw)
					if customCommandNeedsFreshTurn(customCmd, m.loop) {
						m.messages = append(m.messages, Message{
							Role: "warning",
							Content: fmt.Sprintf(
								"(can't /%s mid-turn — trusted allowed-tools require a new turn boundary; wait for this turn to finish, and use /model first if the command names another model)",
								customCmd.Name,
							),
							Timestamp: time.Now(),
						})
						// Keep the invocation in the editor so it can be submitted once
						// the active turn ends; silently clearing it loses user intent.
						return m, nil
					}
					if customCmd != nil && !customCmd.Trusted {
						_, warnings, _ := prepareCustomCommandTurn(m.ctx, customCmd, m.loop)
						for _, warning := range warnings {
							m.messages = append(m.messages, Message{Role: "warning", Content: warning, Timestamp: time.Now()})
						}
					}
					if display == "" {
						return m, nil
					}
					if !m.loop.SteerInject(display) {
						// LoopDone may be racing this Enter: the TUI still says
						// turnActive for one render tick after the loop atomically
						// closes steering. Preserve the input as a next-turn item.
						m.enqueueQueuedItem(display, QueuePriorityNext)
						m.messages = append(m.messages, Message{
							Role:      "info",
							Content:   "(current turn was already closing — queued for the next turn)",
							Timestamp: time.Now(),
						})
						m.input.Reset()
						return m, nil
					}
					// user-steer (not info) so the steered prompt is
					// visible in the same lane as a normal user message.
					m.messages = append(m.messages, Message{
						Role:      "user-steer",
						Content:   display,
						Timestamp: time.Now(),
					})
					m.input.Reset()
					return m, nil
				case slash.MidTurnSafe:
					// Execute the same local REPL/signal path used while idle. The
					// guard prevents recursion from re-entering this mid-turn branch,
					// while turnActive remains true for handlers such as /bg.
					m.dispatchingMidTurnCommand = true
					defer func() { m.dispatchingMidTurnCommand = false }()
					return m.handleSubmit()
				}
			}
		}

		// Plain text plus safe/unknown slash input steers the current
		// turn. Keep the submitted text visible in the transcript so
		// users can see that it landed; /later remains the explicit way
		// to defer a message to the next turn.
		if !m.loop.SteerInject(raw) {
			m.enqueueQueuedItem(raw, QueuePriorityNext)
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(current turn was already closing — queued for the next turn)",
				Timestamp: time.Now(),
			})
			m.input.Reset()
			return m, nil
		}
		m.messages = append(m.messages, Message{
			Role:      "user-steer",
			Content:   raw,
			Timestamp: time.Now(),
		})
		m.input.Reset()
		return m, nil
	}
	// /now /later out-of-turn: priority is meaningless when there's
	// nothing running to preempt, so strip the prefix and submit the
	// body as a normal user message. Without this stripping the slash
	// registry would 404 the unknown name and the user would see
	// "/now: unknown command — try /help" — confusing because the
	// command IS recognised when a turn is in flight. Empty body →
	// info hint, same as the mid-turn refusal path.
	if prio, body, ok := parsePriorityCommand(text); ok {
		if body == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(empty " + prioCommandName(prio) + " — write the message after the command)",
				Timestamp: time.Now(),
			})
			m.input.Reset()
			return m, nil
		}
		// Replace `text` with the body so the rest of submitInput
		// (palette logic, slash parse, llm submit) treats it as a
		// plain user prompt. Priority field is intentionally dropped
		// here — we already chose to ignore it out-of-turn.
		text = body
	}

	m.input.Reset()
	m.showPalette = false
	m.palFilter = ""

	// Submitting a new prompt re-anchors the viewport to the live
	// tail. claude-code parity: typing into the editor and hitting
	// Enter is the canonical "I'm caught up, follow the stream" gesture.
	m.stickyBottom = true

	// Bash mode: `!ls -la` runs the rest of the line as a shell command
	// without going through the LLM. Saves tokens for trivial things
	// (ls, pwd, cat, ...) and is also useful for shell debugging while
	// chatting (claude-code calls this "BashMode"; we just check the
	// first non-space character).
	if strings.HasPrefix(text, "!") {
		cmd := strings.TrimSpace(text[1:])
		if cmd == "" {
			m.messages = append(m.messages, Message{
				Role: "info", Content: "(bash mode: nothing after `!`)",
				Timestamp: time.Now(),
			})
			return m, nil
		}
		// Run ASYNC via a tea.Cmd: a synchronous CombinedOutput here would
		// block the bubbletea Update goroutine (and thus the whole UI, the
		// spinner, and event draining) for up to the bash timeout — `!sleep 60`
		// froze the TUI hard. The result lands as a bashLocalResultMsg.
		return m, m.bashLocalCmd(cmd)
	}

	// Built-in commands
	if strings.HasPrefix(text, "/") {
		cmdText := text[1:]
		name, args, _ := cut(cmdText, " ")

		// Defensive fallback for callers that reach this branch without the
		// early inline interception above. Explicit `/effort high` still falls
		// through to the REPL path so scripted usage stays unchanged.
		if name == "effort" && strings.TrimSpace(args) == "" {
			m.input.SetValue("/effort")
			m.openInlineEffortPicker()
			return m, nil
		}

		// Phase C2: `/help` opens the tabbed widget (general / commands
		// / custom-commands). Mirrors claude-code's tabbed help modal
		// (images #7-9 in the user TUI feedback). Pre-empt the REPL
		// path so cmdHelp's flat list never renders.
		if name == "help" || name == "h" || name == "?" {
			h := screen.NewHelpScreen(versionLabel(), m.buildHelpTabs())
			h.Resize(m.width, m.height)
			m.activeScreen = h
			return m, nil
		}

		// Phase C3: bare `/model` (alias `/m`) opens the picker widget.
		// Explicit `/model claude-opus-4-7` falls through to cmdModel
		// for scripted usage / palette autocomplete.
		if (name == "model" || name == "m") && strings.TrimSpace(args) == "" {
			m.openModelPicker(false, 0)
			return m, nil
		}

		// Phase C4: bare `/theme` opens the cycle widget with live
		// swatches. Explicit `/theme dark` falls through to cmdTheme
		// for direct selection.
		if name == "theme" && strings.TrimSpace(args) == "" {
			tp := screen.NewThemeScreen(themes.Current().Name, themes.BuildThemeChoices())
			tp.Resize(m.width, m.height)
			m.activeScreen = tp
			return m, nil
		}

		// Phase C5: `/permissions` (alias `/perms`) opens the
		// interactive editor — mode cycle + read-only rules list.
		if name == "permissions" || name == "perms" {
			ps := screen.NewPermissionsScreen(string(m.gate.Mode()), m.permRulesSnapshot())
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		}

		// Phase C+: list-style commands open the generic PickerScreen
		// (cursor + Enter to pick). Pre-empt the BodyScreen path so
		// the user can select + invoke instead of just reading a list.
		switch name {
		case "sessions", "ls":
			if strings.TrimSpace(args) != "" {
				break
			}
			items := m.sessionsPickerItems(20)
			ps := screen.NewPickerScreen("/sessions", pickerSubtitle("recent sessions", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		case "skills", "sk":
			if strings.TrimSpace(args) != "" {
				break
			}
			items := m.skillsPickerItems()
			ps := screen.NewPickerScreen("/skills", pickerSubtitle("skills loaded", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		case "tools", "t", "toolsets":
			if strings.TrimSpace(args) != "" {
				break
			}
			items := m.toolsPickerItems()
			ps := screen.NewPickerScreen("/tools", pickerSubtitle("tools registered", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		}

		// === Desktop Features (Codex parity) ===
		if name == "resume" || name == "rs" {
			sessions := m.buildResumeSessions()
			rs := screen.NewResumeScreen(sessions)
			rs.Resize(m.width, m.height)
			m.activeScreen = rs
			return m, nil
		}
		if name == "diff-view" || name == "dv" {
			files := m.buildDiffFiles()
			dv := screen.NewDiffViewerScreen(files)
			dv.Resize(m.width, m.height)
			m.activeScreen = dv
			return m, nil
		}
		if name == "agents" || name == "av" {
			m.openAgentsView()
			return m, nil
		}
		if name == "desktop" {
			cwd, _ := os.Getwd()
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "launching desktop app for: " + cwd,
				Timestamp: time.Now(),
			})
			// open(1) returns quickly. Keep the mutation on Bubble Tea's update
			// goroutine; writing m.messages from a detached goroutine races render.
			if err := desktop.LaunchApp(cwd); err != nil {
				m.messages = append(m.messages, Message{
					Role:      "error",
					Content:   "desktop launch failed: " + err.Error(),
					Timestamp: time.Now(),
				})
			}
			return m, nil
		}
		if name == "config" || name == "cfg" {
			return m, m.openConfigEditor()
		}

		if cmd := m.cmds.Get(name); cmd != nil && !preferSlashInTUI(cmd.Name) {
			if cmd.Name == "quit" || cmd.Name == "exit" {
				return m, tea.Quit
			}
			if cmd.Name == "clear" {
				// Consolidated path — see internal/tui/reload.go.
				if err := m.Reload(ReloadOpts{}); err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "clear: " + err.Error(), Timestamp: time.Now()})
				}
				return m, nil
			}
			repl := m.asREPL()
			output := cmd.Handler(repl, args)
			// asREPL is a value bridge. Model/provider switches mutate the
			// bridge and the shared Loop, so copy the successful labels back to
			// the TUI chrome as well. On rebuild failure the REPL helper is
			// atomic and these values remain unchanged.
			if cmd.Name == "model" {
				m.model = repl.model
				m.providerName = repl.providerName
			}
			// Sync m.sessionTitle from disk after REPL commands that
			// mutate the session header. cmdRename (registered in
			// commands.go as `rename` with alias `title`) calls
			// session.Store.SetTitle through the asREPL() proxy — that
			// path never touches the Model, so without this re-read the
			// terminal-tab WindowTitle stays at its launch-time value
			// even though the session JSONL header has the new title
			// (image #14 user feedback). Best-effort: header read errors
			// are silently ignored.
			if (cmd.Name == "rename" || cmd.Name == "title") &&
				m.session != nil && m.sessionID != "" {
				if hdr, _, err := m.session.LoadHeader(m.sessionID); err == nil && hdr != nil {
					m.sessionTitle = strings.TrimSpace(hdr.Title)
				}
			}
			if output != "" {
				if modalCommands[cmd.Name] {
					// Information-dense commands open as a full-window
					// modal overlay (claude-code parity) instead of
					// inlining into the chat scroll. Esc/q to dismiss.
					m.openBodyScreen("/"+cmd.Name, output)
				} else {
					m.messages = append(m.messages, Message{Role: classifyREPLOutput(output), Content: output, Timestamp: time.Now()})
				}
			}
			return m, nil
		}
	}

	// Slash registry
	if handled, display, sig, args := m.slash.Parse(text); handled {
		// Prompt-rewriting signals consume display as model input below. Echoing
		// it here first exposes internal /review instructions in the transcript
		// (and duplicates user-authored custom-command bodies).
		if display != "" && sig != slash.SignalCustomPrompt && sig != slash.SignalBatch {
			m.messages = append(m.messages, Message{Role: "info", Content: display, Timestamp: time.Now()})
		}
		_ = args // many signals don't need it; the ones that do read below
		switch sig {
		case slash.SignalQuit:
			return m, tea.Quit
		case slash.SignalClear:
			if err := m.Reload(ReloadOpts{}); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "clear: " + err.Error(), Timestamp: time.Now()})
			}
		case slash.SignalNew:
			if err := m.persistActiveSessionState(); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "new session: " + err.Error(), Timestamp: time.Now()})
				break
			}
			var noteErr error
			if m.loop != nil && m.loop.Memory != nil {
				noteErr = m.loop.Memory.SaveDailyNote(m.sessionID, "new", m.summarizeHistory())
			}
			newID, hdr, err := m.createFreshSession()
			if err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "new session: " + err.Error(), Timestamp: time.Now()})
				break
			}
			if err := m.activateSession(newID, hdr, nil, false); err != nil {
				m.messages = append(m.messages, Message{Role: "warning", Content: m.sessionActivationWarning("new session activation", err), Timestamp: time.Now()})
				if noteErr != nil {
					m.messages = append(m.messages, Message{Role: "warning", Content: "failed to save previous session note: " + noteErr.Error(), Timestamp: time.Now()})
				}
				break
			}
			m.firstRender = true
			m.showBanner = true
			m.messages = append(m.messages, Message{Role: "success", Content: "started new session: " + shortID(newID), Timestamp: time.Now()})
			if noteErr != nil {
				m.messages = append(m.messages, Message{Role: "warning", Content: "failed to save previous session note: " + noteErr.Error(), Timestamp: time.Now()})
			}
		case slash.SignalUndo:
			// Prefill behaviour: pop the last turn AND drop the user's
			// original text into the input box so they can edit-and-resend
			// instead of retyping. Mirrors kimi-cli's /undo UX. Empty
			// prefill (synthetic turn) preserves the input box untouched.
			if prefill, ok := m.loop.UndoLastTurnWithPrefill(); ok {
				persistErr := m.session.ReplaceHistoryAndMark(m.sessionID, m.loop.History(), &m.historyCursor)
				m.messages = trimVisibleMessagesToLastUser(m.messages)
				m.toolEvents = nil
				if prefill != "" {
					m.input.SetValue(prefill)
					if persistErr == nil {
						m.messages = append(m.messages, Message{Role: "success", Content: "(undid last turn — original text in input)", Timestamp: time.Now()})
					}
				} else {
					if persistErr == nil {
						m.messages = append(m.messages, Message{Role: "success", Content: "(undid last turn)", Timestamp: time.Now()})
					}
				}
				if persistErr != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "undo applied in memory but failed to persist: " + persistErr.Error(), Timestamp: time.Now()})
				}
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(nothing to undo)", Timestamp: time.Now()})
			}
		case slash.SignalRewind:
			// Unified rewind: restore files AND conversation to the
			// pre-edit snapshot of the last edit-turn.
			if res, ok := m.loop.Rewind(); ok {
				persistErr := m.session.ReplaceHistoryAndMark(m.sessionID, m.loop.History(), &m.historyCursor)
				// Trim exactly as many visible user blocks as turns were
				// undone — guards the TurnsUndone==0 case (e.g. /undo then
				// /rewind already rolled the conversation past the
				// snapshot) where an unconditional trim would desync the
				// on-screen chat from the loop's Messages.
				for i := 0; i < res.TurnsUndone; i++ {
					m.messages = trimVisibleMessagesToLastUser(m.messages)
				}
				m.toolEvents = nil
				if persistErr != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "rewind applied but failed to persist conversation: " + persistErr.Error(), Timestamp: time.Now()})
				} else {
					m.messages = append(m.messages, Message{
						Role:      "success",
						Content:   fmt.Sprintf("(rewound: restored files + undid %d turn(s) — %s)", res.TurnsUndone, res.Label),
						Timestamp: time.Now(),
					})
				}
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(nothing to rewind — no file snapshots yet, or checkpointing is off)", Timestamp: time.Now()})
			}
		case slash.SignalHistory:
			hs := screen.NewHistoryScreen(m.loop.History(), m.width, m.height)
			hs.Title = "session history (" + m.sessionID + ")"
			m.activeScreen = hs
		case slash.SignalThinkingDisplay:
			m.applyThinkingDisplay(args)
		case slash.SignalTitle:
			title := strings.TrimSpace(args)
			if title == "" {
				m.messages = append(m.messages, Message{Role: "info", Content: "(title: type `/title <text>` to set)", Timestamp: time.Now()})
			} else if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "warning", Content: "(title: no session store available)", Timestamp: time.Now()})
			} else if err := m.session.SetTitle(m.sessionID, title); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "title: " + err.Error(), Timestamp: time.Now()})
			} else {
				// Update Model cache so View() emits the new
				// tea.View.WindowTitle (bubbletea v2 diffs lastView and
				// auto-emits the OSC 0 escape when the value changes —
				// see cursed_renderer.go:372). Without this assignment
				// /rename only persists to disk; the terminal tab keeps
				// the old name until next launch.
				m.sessionTitle = title
				m.messages = append(m.messages, Message{Role: "success", Content: "(title set: " + title + ")", Timestamp: time.Now()})
			}
		case slash.SignalBranch:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "warning", Content: "(branch: no session store)", Timestamp: time.Now()})
			} else {
				if err := m.persistActiveSessionState(); err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "branch: " + err.Error(), Timestamp: time.Now()})
					break
				}
				newID, hdr, err := m.forkSession(m.sessionID, m.loop.History())
				if err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "branch: " + err.Error(), Timestamp: time.Now()})
					break
				}
				if err := m.activateSession(newID, hdr, m.loop.History(), false); err != nil {
					m.messages = append(m.messages, Message{Role: "warning", Content: m.sessionActivationWarning("branch activation", err), Timestamp: time.Now()})
					break
				}
				m.messages = append(m.messages, Message{Role: "success", Content: "(branched → " + newID + ")", Timestamp: time.Now()})
			}
		case slash.SignalSave:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "warning", Content: "(save: no session store)", Timestamp: time.Now()})
			} else if err := m.session.Sync(m.sessionID); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "save: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "success", Content: "(session synced to disk)", Timestamp: time.Now()})
			}
		case slash.SignalTools:
			items := m.toolsPickerItems()
			ps := screen.NewPickerScreen("/tools", pickerSubtitle("tools registered", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
		case slash.SignalSessions:
			items := m.sessionsPickerItems(20)
			ps := screen.NewPickerScreen("/sessions", pickerSubtitle("recent sessions", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
		case slash.SignalSession:
			m.messages = append(m.messages, Message{Role: "info", Content: renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())), Timestamp: time.Now()})
		case slash.SignalStatus:
			// Reuse renderCurrentSession — same data the user wants from
			// /status. Inline (not modal) so the transcript stays visible —
			// claude-code parity (image #4).
			m.messages = append(m.messages, Message{Role: "info", Content: renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())), Timestamp: time.Now()})
		case slash.SignalSkills:
			items := m.skillsPickerItems()
			ps := screen.NewPickerScreen("/skills", pickerSubtitle("skills loaded", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
		case slash.SignalVersion:
			m.openBodyScreen("/version", renderVersion())
		case slash.SignalAddDir:
			if m.ext.DirAdd == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "(/add-dir not wired in this build)", Timestamp: time.Now()})
			} else if err := m.ext.DirAdd(args, true); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "add-dir: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "success", Content: "(added: " + args + ")", Timestamp: time.Now()})
			}
		case slash.SignalRemoveDir:
			if m.ext.DirRemove == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "(/rm-dir not wired in this build)", Timestamp: time.Now()})
			} else if err := m.ext.DirRemove(args); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "rm-dir: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "success", Content: "(removed: " + args + ")", Timestamp: time.Now()})
			}
		case slash.SignalListDirs:
			if m.ext.DirList == nil {
				m.messages = append(m.messages, Message{Role: "info", Content: "(no additional dirs)", Timestamp: time.Now()})
			} else {
				dirs := m.ext.DirList()
				if len(dirs) == 0 {
					m.messages = append(m.messages, Message{Role: "info", Content: "(no additional dirs — `/add-dir <path>` to add one)", Timestamp: time.Now()})
				} else {
					var b strings.Builder
					b.WriteString("additional dirs:\n")
					for _, d := range dirs {
						b.WriteString("  ")
						b.WriteString(d)
						b.WriteString("\n")
					}
					m.messages = append(m.messages, Message{Role: "info", Content: strings.TrimRight(b.String(), "\n"), Timestamp: time.Now()})
				}
			}
		case slash.SignalCost:
			m.messages = append(m.messages, Message{Role: "info", Content: renderCost(m), Timestamp: time.Now()})
		case slash.SignalDiff:
			m.messages = append(m.messages, Message{Role: "info", Content: renderDiff(m), Timestamp: time.Now()})
		case slash.SignalDoctor:
			m.openBodyScreen("/doctor", renderDoctor(m))
		case slash.SignalStats:
			m.openBodyScreen("/stats", renderStats(m))
		case slash.SignalKeybindings:
			m.openBodyScreen("/keybindings", renderKeybindings())
		case slash.SignalPermissions:
			// Phase C5: interactive PermissionsScreen with mode cycle.
			ps := screen.NewPermissionsScreen(string(m.gate.Mode()), m.permRulesSnapshot())
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
		case slash.SignalHooks:
			m.openBodyScreen("/hooks", renderHooksList(m.cfg))
		case slash.SignalVim:
			toggleVimMode()
			m.messages = append(m.messages, Message{Role: "info", Content: vimModeStatus(), Timestamp: time.Now()})
		case slash.SignalExport:
			if m.loop == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: exportFailure(fmt.Errorf("no active conversation")), Timestamp: time.Now()})
			} else {
				p, err := exportConversationToFile(m.loop.History(), args, time.Now())
				if err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: exportFailure(err), Timestamp: time.Now()})
				} else {
					m.messages = append(m.messages, Message{Role: "command-result", Content: exportSuccess(p), Timestamp: time.Now()})
				}
			}
		case slash.SignalReleaseNotes:
			m.openBodyScreen("/release-notes", renderReleaseNotes())
		case slash.SignalTheme:
			// /theme stays inline for now — short toggle confirmation,
			// not browseable. Phase C4 will replace with a cycle widget.
			m.messages = append(m.messages, Message{Role: "info", Content: renderTheme(args), Timestamp: time.Now()})
		case slash.SignalEffort:
			// Bare `/effort` is rendered below the chat input; explicit form
			// `/effort high` remains a one-line command result.
			if strings.TrimSpace(args) == "" {
				m.input.SetValue("/effort")
				m.openInlineEffortPicker()
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: renderEffort(args), Timestamp: time.Now()})
			}
		case slash.SignalPRComments:
			m.openBodyScreen("/pr_comments", renderPRComments(args))
		case slash.SignalUpgrade:
			m.openBodyScreen("/upgrade", renderUpgrade())
		case slash.SignalContext:
			// Inline render (claude-code parity, 2026-05-11). Append
			// as an info-role message so it lives in the chat scroll
			// and the user doesn't need Esc to dismiss. Long output
			// flows like any other multi-line reply.
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   renderContext(m),
				Timestamp: time.Now(),
			})
		case slash.SignalResume:
			m.openBodyScreen("/resume", renderResumeHelp(m))
		case slash.SignalTag:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "error", Content: "(tag: no session store)", Timestamp: time.Now()})
			} else if err := tagCurrentSession(m.session, m.sessionID, args); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "tag: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "success", Content: "(tagged: " + args + ")", Timestamp: time.Now()})
			}
		case slash.SignalBtw:
			return m, m.startBtwQuery(args)
		case slash.SignalBatch:
			// `/batch <task>` rewrites the prompt to the embedded
			// research → plan → execute worker contract, then falls
			// through to the normal user-message path so the agent
			// loop runs on it. The flag below skips the early return.
			text = slash.BatchPrompt(args)
		case slash.SignalCustomPrompt:
			// User-authored slash command (~/.metis/commands/*.md):
			// the handler already substituted $ARGUMENTS / $1.. and
			// returned the resolved prompt body in `display`. Treat
			// it like /batch — re-enter the agent path with the
			// rewritten text, no separate echo of the template.
			if display != "" {
				customCmd := customCommandFromInput(m.slash, text)
				preparedCtx, warnings, err := prepareCustomCommandTurn(turnRunCtx, customCmd, m.loop)
				for _, warning := range warnings {
					m.messages = append(m.messages, Message{Role: "warning", Content: warning, Timestamp: time.Now()})
				}
				if err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: err.Error(), Timestamp: time.Now()})
					return m, nil
				}
				turnRunCtx = preparedCtx
				if strings.EqualFold(slashName(text), "/review") {
					visibleUserText = text
					text = wrapInternalReviewPrompt(display)
				} else {
					text = display
				}
				display = ""
			}
		case slash.SignalPlan:
			// /plan + /p — switch the gate to plan mode (read-only;
			// tool calls collected as proposal instead of executed).
			// Pre-2026-05-21 the signal was declared but had no
			// case, so typing /plan in the input box was silently
			// dead (compiled-bug-list audit).
			m.gate.SetMode(permission.ModePlan)
			m.messages = append(m.messages, Message{Role: "success", Content: "(mode: plan — tool calls will be surfaced for review)", Timestamp: time.Now()})
		case slash.SignalAcceptEdits:
			m.gate.SetMode(permission.ModeAcceptEdits)
			m.messages = append(m.messages, Message{Role: "success", Content: "(mode: acceptEdits — file edits are accepted; other state changes may still ask)", Timestamp: time.Now()})
		case slash.SignalBypassPermissions:
			m.gate.SetMode(permission.ModeBypassPermissions)
			m.messages = append(m.messages, Message{Role: "warning", Content: "(mode: bypassPermissions — tool calls auto-approved; use /default to restore prompts)", Timestamp: time.Now()})
		case slash.SignalDefault:
			m.gate.SetMode(permission.ModeDefault)
			m.messages = append(m.messages, Message{Role: "success", Content: "(mode: default — ask before state changes)", Timestamp: time.Now()})
		case slash.SignalDontAsk:
			m.gate.SetMode(permission.ModeDontAsk)
			m.messages = append(m.messages, Message{Role: "warning", Content: "(mode: dontAsk — actions requiring approval will be denied)", Timestamp: time.Now()})
		case slash.SignalRetry:
			// A retry replaces the prior user→assistant exchange, then submits
			// the same user prompt immediately. Merely prefilling the editor
			// created a duplicate turn and did not retry the response at all.
			lastUser, ok := m.loop.UndoLastTurnWithPrefill()
			if !ok || strings.TrimSpace(lastUser) == "" {
				m.messages = append(m.messages, Message{Role: "warning", Content: "(retry: no prior user prompt found in history)", Timestamp: time.Now()})
			} else {
				if m.session != nil && m.sessionID != "" {
					if err := m.session.ReplaceHistoryAndMark(m.sessionID, m.loop.History(), &m.historyCursor); err != nil {
						m.messages = append(m.messages, Message{Role: "error", Content: "retry: failed to persist rollback: " + err.Error(), Timestamp: time.Now()})
						break
					}
				}
				m.messages = trimVisibleMessagesToLastUser(m.messages)
				if m.turnToolEventStart >= 0 && m.turnToolEventStart <= len(m.toolEvents) {
					m.toolEvents = m.toolEvents[:m.turnToolEventStart]
				} else {
					m.toolEvents = nil
				}
				m.input.SetValue(lastUser)
				m.messages = append(m.messages, Message{Role: "info", Content: "(retrying last response)", Timestamp: time.Now()})
				return m.handleSubmit()
			}
		case slash.SignalLoop:
			// /loop — autopilot scheduling not yet implemented in the
			// chat surface (it works as a top-level skill in some
			// environments). Redirect to /cron which is the working
			// equivalent in metis core.
			m.messages = append(m.messages, Message{Role: "info", Content: "(/loop autopilot not wired in chat — use /cron for scheduled prompts instead)", Timestamp: time.Now()})
		case slash.SignalReload:
			if m.ext.ReloadCatalog == nil {
				m.messages = append(m.messages, Message{Role: "warning", Content: "reload: catalog hook unavailable in this build", Timestamp: time.Now()})
			} else if summary, err := m.ext.ReloadCatalog(); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "reload: " + err.Error(), Timestamp: time.Now()})
			} else {
				if strings.TrimSpace(summary) == "" {
					summary = "catalog refreshed"
				}
				m.messages = append(m.messages, Message{Role: "success", Content: "reload: " + summary, Timestamp: time.Now()})
			}
		}
		// Slash commands that produced a regular reply terminate here.
		// /batch and /custom-prompt rewrite `text` above and re-enter
		// the agent path below; everything else returns.
		if sig != slash.SignalBatch && sig != slash.SignalCustomPrompt {
			return m, nil
		}
	}

	// User message → run agent.
	//
	// Pasted images path: split text on `[Image #N]` placeholders and
	// build a multimodal content-block list (text + image_block …).
	// The agent sees real bytes; the displayed user message keeps the
	// placeholder text so the transcript looks clean.
	//
	// Plain-text path stays cheaper — single text block via AppendUser.
	//
	// Subdirectory hints: when the user @-mentions a path BELOW cwd,
	// collect per-directory CLAUDE.md / AGENTS.md / METIS.md along
	// the descent and prepend them to the LLM-facing text (not the
	// transcript). Pairs with loadProjectContext's UP-walk so a
	// monorepo's nested per-service conventions surface even when
	// cwd sits at the repo root. Mirrors claude-code's
	// getMemoryFilesForNestedDirectory attachment path — keeps the
	// system prompt cache warm by living on the user message side.
	llmText := text
	if cwd, err := os.Getwd(); err == nil {
		if hints := runtime.CollectSubdirHints(text, cwd, nil); hints != "" {
			llmText = hints + "\n\n" + text
		}
	}
	// P3 (2026-05-18) — `@/path/to/image.png` text-reference expansion.
	// Scans llmText for image-extension @-mentions, loads each via the
	// shared image preprocessor, and pulls them into the same blocks
	// stream the clipboard path uses. Runs BEFORE the paste branch so
	// a single message can mix pasted + referenced images.
	atCwd, _ := os.Getwd()
	llmTextRewritten, atFileBlocks, atFileErrs := expandAtFileImageBlocks(llmText, atCwd)
	for _, e := range atFileErrs {
		m.messages = append(m.messages, Message{
			Role: "warning", Content: "image: " + e, Timestamp: time.Now(),
		})
	}

	if len(m.imagePaste) > 0 || len(atFileBlocks) > 0 {
		var blocks []llm.ContentBlock
		var errs []string
		if len(m.imagePaste) > 0 {
			pasted, pErrs := expandPastedImagesToBlocks(llmTextRewritten, m.imagePaste)
			blocks = append(blocks, pasted...)
			errs = append(errs, pErrs...)
		} else {
			// Only @-file path active — text comes through as one block,
			// images appended after.
			if llmTextRewritten != "" {
				blocks = append(blocks, llm.ContentBlock{Type: "text", Text: llmTextRewritten})
			}
		}
		blocks = append(blocks, atFileBlocks...)
		// Pasted attachments are all-or-nothing. If a cached file vanished
		// or failed preprocessing, do not send the generated local-path
		// fallback as though it were the image. That reproduces the stale
		// Desktop-path failure from the user's session and then clears the
		// only attachment mapping that could recover it. Keep both editor
		// and side-table intact so a re-paste/retry is lossless.
		if len(errs) > 0 {
			m.input.SetValue(text)
			m.appendImageWarningOnce("image not sent — prompt and cached attachment(s) are kept: " + strings.Join(errs, "; "))
			return m, nil
		}

		// Vision capability gate. Never strip the image and submit the text
		// remainder: that leaves a visible [Image #N] with no corresponding
		// bytes and invites the model to guess a stale local path. Keep the
		// editor + cached image intact so /model followed by Enter sends the
		// original attachment without another paste.
		if m.loop != nil && m.loop.Provider != nil && !pubprov.ProviderSupportsVision(m.loop.Provider) {
			stripped, _ := splitOffImageBlocks(blocks)
			if stripped > 0 {
				m.input.SetValue(text)
				if m.openModelPicker(true, stripped) {
					m.appendImageWarningOnce(fmt.Sprintf(
						"image not sent — current model (%s) is text-only. Prompt and %d image(s) are kept; choose a vision model, then press Enter to send.",
						m.loop.Model, stripped,
					))
				} else {
					m.appendImageWarningOnce(fmt.Sprintf(
						"image not sent — current model (%s) is text-only. Prompt and %d image(s) are kept, but no configured vision-capable provider profile is available.",
						m.loop.Model, stripped,
					))
				}
				return m, nil
			}
		}
		m.loop.AppendUserBlocks(blocks)
		for _, e := range errs {
			m.messages = append(m.messages, Message{
				Role: "warning", Content: "image: " + e, Timestamp: time.Now(),
			})
		}
		m.imagePaste = nil
		m.imageCounter = 0
	} else if visibleUserText != "" {
		// Keep the tagged provider frame first and the concise invocation last.
		// transcript.UndoWithPrefill intentionally returns the last text block as
		// the user's editable input; reversing this order leaks the internal
		// review frame through /undo and /retry. Resume/export filter the tagged
		// block and show only the final invocation.
		m.loop.AppendUserBlocks([]llm.ContentBlock{
			{Type: "text", Text: llmTextRewritten},
			{Type: "text", Text: visibleUserText},
		})
	} else {
		m.loop.AppendUser(llmTextRewritten)
	}
	// Persist through a durable history cursor. This records the initial
	// prompt now, then persistTail at turn end records every assistant/tool
	// message plus any user steering injected while the run was active.
	m.persistTail()
	// Mirror to ~/.metis/history.jsonl for cross-session prompt search.
	// Fire-and-forget — disk hiccups must not block the chat.
	transcriptText := text
	if visibleUserText != "" {
		transcriptText = visibleUserText
	}
	_ = runtime.AppendHistory(runtime.HistoryEntry{
		SessionID: m.sessionID, Input: transcriptText, Source: "tui",
	})
	m.messages = append(m.messages, Message{Role: "user", Content: transcriptText, Timestamp: time.Now()})
	// 2026-05-25: do NOT wipe m.toolEvents on each new submit. Pre-fix
	// behaviour cleared every prior turn's tool calls the moment the
	// user typed a new prompt — the second turn collapsed to just the
	// model's narration + "thought-summary" + "recap" rows, hiding
	// the actual bash/edit/read tool history that produced the answer
	// (user image 70→71 feedback: same turn looked complete with all
	// the tool rows visible, then on next submit the rows disappeared
	// leaving only italic narration bullets). claude-code preserves
	// every tool call in the transcript for the lifetime of the
	// session; chatList virtualisation handles the row count fine
	// (setMaxMounted caps mounted item count for render perf, the
	// underlying slice can grow unbounded).
	//
	// streamingText cleared on each turn boundary so the new turn's
	// in-flight assistant text doesn't visually concatenate with the
	// previous turn's tail (that one is intentional and unchanged).
	m.streamingText = ""
	m.turnToolEventStart = len(m.toolEvents)
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerFrame = 0
	m.spinnerStartedAt = time.Now()
	// OSC 9;4 indeterminate progress — terminals that support it
	// (iTerm2 / Ghostty / WezTerm) light up the dock icon with a
	// barber-pole "working" indicator. Cleared on turn end.
	notify.SendProgress(notify.ProgressIndeterminate, 0)
	m.firstStreamAt = time.Time{}
	m.spinnerVerb = chooseSpinnerVerb(m.sessionID)
	m.spinnerSub = ""
	// Initial phase: prompt is being sent, no bytes received yet → ↑.
	// EventThinkingDelta / EventTextDelta / EventToolStart will flip
	// this once the first byte arrives.
	m.spinnerPhase = "requesting"
	m.showBanner = false // Hide banner after first message
	m.turnCancelledByUser = false

	// Snapshot what runTurnAsync needs BEFORE the `go` — see the
	// comment on runTurnAsync for why. m.turnCancel must be written
	// on this (main) thread, not from inside the goroutine; cleared
	// in finalizeTurn when doneCh fires.
	turnCtx, cancel := context.WithCancel(turnRunCtx)
	m.turnCancel = cancel
	go runTurnAsync(turnCtx, cancel, m.loop, m.eventCh, m.doneCh)
	// Critical: must return tickCmd here so spinnerTick events start flowing,
	// otherwise the "thinking" frame and elapsed timer freeze at 0s and the
	// UI looks dead until the LLM replies.
	return m, tickCmd
}

// slashName extracts the leading "/<name>" prefix from a raw input
// line for use in user-facing log messages. "/intro Chinese" → "/intro".
// Empty / non-slash input → empty string. Helper for Task #87
// mid-turn slash dispatch.
func slashName(raw string) string {
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	rest := raw[1:]
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		return "/" + rest[:i]
	}
	return "/" + rest
}

// =============================================================================
// Desktop Feature Helpers
// =============================================================================

// buildResumeSessions converts the session store entries to ResumeScreen items.
func (m *Model) buildResumeSessions() []screen.SessionEntry {
	if m.session == nil {
		return nil
	}
	cwd, _ := os.Getwd()
	entries, err := m.session.ListResumable(session.ResumeListOptions{Limit: 50, WorkDir: cwd})
	if err != nil {
		return nil
	}
	out := make([]screen.SessionEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, screen.SessionEntry{
			ID:           e.ID,
			Title:        e.Title,
			Model:        e.Model,
			CreatedAt:    sessionEntryTime(e),
			MessageCount: e.MessageCount,
		})
	}
	return out
}

// buildDiffFiles returns tracked staged/unstaged changes plus untracked files
// that are not ignored. `git diff HEAD` deliberately omits the latter, so the
// untracked list must be queried separately and rendered as additions.
func (m *Model) buildDiffFiles() []screen.DiffFile {
	// HEAD includes both staged and unstaged changes, matching what users
	// expect from a desktop "Changes" view.
	cmd := exec.Command("git", "diff", "HEAD", "--no-color", "--no-ext-diff")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}
	files := parseGitDiff(string(out))

	// -z keeps unusual but valid file names (spaces, tabs and newlines) intact;
	// --exclude-standard applies repository, info and global ignore rules.
	untracked, err := exec.Command("git", "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		// A failure to enumerate new files must not hide the tracked changes we
		// already collected.
		return files
	}
	for _, rawPath := range strings.Split(string(untracked), "\x00") {
		if rawPath == "" {
			continue
		}
		files = append(files, buildUntrackedDiffFile(rawPath))
	}
	return files
}

// buildUntrackedDiffFile asks git to produce the same unified patch format as
// tracked files. Exit status 1 is normal for --no-index when files differ, so
// the output is useful regardless of err. Empty and unreadable files still get
// an A entry even when no hunk can be produced.
func buildUntrackedDiffFile(path string) screen.DiffFile {
	out, _ := exec.Command(
		"git", "diff", "--no-index", "--no-color", "--no-ext-diff", "--", "/dev/null", path,
	).CombinedOutput()
	parsed := parseGitDiff(string(out))
	if len(parsed) == 0 {
		return screen.DiffFile{Path: path, Status: "A"}
	}
	file := parsed[0]
	// Git quotes unusual paths in patch headers. The NUL-delimited ls-files
	// result is authoritative and preserves the exact repository-relative name.
	file.Path = path
	file.Status = "A"
	return file
}

// parseGitDiff parses unified diff output into DiffFile structures.
func parseGitDiff(diff string) []screen.DiffFile {
	var files []screen.DiffFile
	var current *screen.DiffFile
	var currentHunk *screen.DiffHunk
	oldNum, newNum := 0, 0

	lines := strings.Split(diff, "\n")
	// A patch normally ends in a newline. strings.Split would turn that
	// terminator into a synthetic empty context line in the final hunk.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			// New file.
			parts := strings.Fields(line)
			path := ""
			if len(parts) >= 4 {
				path = strings.TrimPrefix(parts[3], "b/")
			}
			files = append(files, screen.DiffFile{Path: path, Status: "M"})
			current = &files[len(files)-1]
			currentHunk = nil
		case strings.HasPrefix(line, "--- a/"):
			if current != nil {
				current.Status = "M"
			}
		case strings.HasPrefix(line, "+++ /dev/null"):
			if current != nil {
				current.Status = "D"
			}
		case strings.HasPrefix(line, "--- /dev/null"):
			if current != nil {
				current.Status = "A"
			}
		case strings.HasPrefix(line, "new file mode "):
			if current != nil {
				current.Status = "A"
			}
		case strings.HasPrefix(line, "deleted file mode "):
			if current != nil {
				current.Status = "D"
			}
		case strings.HasPrefix(line, "rename to "):
			if current != nil {
				current.Status = "R"
				current.Path = strings.TrimPrefix(line, "rename to ")
			}
		case strings.HasPrefix(line, "@@"):
			if current == nil {
				break
			}
			hunk, ok := parseDiffHunkHeader(line)
			if !ok {
				currentHunk = nil
				break
			}
			current.Hunks = append(current.Hunks, hunk)
			currentHunk = &current.Hunks[len(current.Hunks)-1]
			oldNum, newNum = hunk.OldStart, hunk.NewStart
		case strings.HasPrefix(line, "+") && currentHunk != nil:
			currentHunk.Lines = append(currentHunk.Lines, screen.DiffLine{
				Type:    "+",
				Content: line[1:],
				NewNum:  newNum,
			})
			newNum++
		case strings.HasPrefix(line, "-") && currentHunk != nil:
			currentHunk.Lines = append(currentHunk.Lines, screen.DiffLine{
				Type:    "-",
				Content: line[1:],
				OldNum:  oldNum,
			})
			oldNum++
		case currentHunk != nil && !strings.HasPrefix(line, "\\ No newline at end of file"):
			content := strings.TrimPrefix(line, " ")
			currentHunk.Lines = append(currentHunk.Lines, screen.DiffLine{
				Type:    " ",
				Content: content,
				OldNum:  oldNum,
				NewNum:  newNum,
			})
			oldNum++
			newNum++
		}
	}
	return files
}

var diffHunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseDiffHunkHeader(line string) (screen.DiffHunk, bool) {
	m := diffHunkRE.FindStringSubmatch(line)
	if m == nil {
		return screen.DiffHunk{}, false
	}
	atoi := func(v string, fallback int) int {
		if v == "" {
			return fallback
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	return screen.DiffHunk{
		OldStart: atoi(m[1], 0), OldCount: atoi(m[2], 1),
		NewStart: atoi(m[3], 0), NewCount: atoi(m[4], 1),
	}, true
}

func isAgentsViewCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, _, _ := cut(strings.TrimPrefix(text, "/"), " ")
	return name == "agents" || name == "av"
}

func (m *Model) openAgentsView() {
	// A live source lets spinner ticks refresh the modal after new agent/tool
	// events are drained, without sharing mutable screen state with the worker
	// goroutine (all callbacks run on Bubble Tea's Update/View goroutine).
	av := screen.NewLiveMultiAgentScreen(m.buildAgentTasks)
	av.Resize(m.width, m.height)
	m.activeScreen = av
}

// buildAgentTasks converts the live status-bar roster and the retained tool
// event timeline to MultiAgentScreen items. subAgents is intentionally pruned
// shortly after completion; toolEvents survives for the session, so it serves
// as the durable fallback and keeps /agents useful after the 2s pill tail.
func (m *Model) buildAgentTasks() []screen.AgentTask {
	eventTasks := make(map[string]screen.AgentTask)
	eventOrder := make([]string, 0)
	for i, event := range m.toolEvents {
		if event.ToolName != "Agent" {
			continue
		}
		key := event.ID
		if key == "" {
			// Current producers always carry an ID. Keep legacy/resumed events
			// independently addressable rather than collapsing them together.
			key = fmt.Sprintf("legacy-agent-%d", i)
		}
		name := "agent"
		if label := agentDisplayLabel(event.Input); label != "" {
			name = truncate(label, 40)
		}
		status := "running"
		if event.Kind != "start" {
			status = "completed"
			if event.IsError {
				status = "failed"
			}
		}
		task := screen.AgentTask{
			ID:        event.ID,
			Name:      name,
			Status:    status,
			StartedAt: event.StartTime,
		}
		if event.Kind != "start" && !event.StartTime.IsZero() {
			if event.Duration > 0 {
				task.FinishedAt = event.StartTime.Add(event.Duration)
			} else {
				// Resumed transcripts do not retain tool durations. Suppress the
				// clock instead of showing a completed task whose elapsed time
				// keeps increasing forever.
				task.StartedAt = time.Time{}
			}
		}
		if event.IsError {
			task.Error = event.Output
		}
		eventTasks[key] = task
		eventOrder = append(eventOrder, key)
	}
	for _, event := range m.toolEvents {
		if event.SubAgentParentID == "" {
			continue
		}
		task, ok := eventTasks[event.SubAgentParentID]
		if !ok {
			continue
		}
		task.ToolsCount++
		task.LastTool = strings.TrimPrefix(event.ToolName, "sub: ")
		eventTasks[event.SubAgentParentID] = task
	}

	// Put currently running/recent agents first, then retain completed history
	// newest-first. This avoids opening a long session at an old, irrelevant
	// agent while still making prior results inspectable.
	out := make([]screen.AgentTask, 0, len(m.subAgents)+len(eventTasks))
	seen := make(map[string]bool, len(m.subAgents))
	for _, sa := range m.subAgents {
		task := screen.AgentTask{
			ID:         sa.ID,
			Name:       sa.Name,
			Status:     sa.Status,
			StartedAt:  sa.StartedAt,
			FinishedAt: sa.FinishedAt,
			ToolsCount: sa.ToolsCount,
			LastTool:   sa.LastTool,
		}
		// Retain an error string captured by the durable event snapshot.
		if historical, ok := eventTasks[sa.ID]; ok {
			task.Error = historical.Error
		}
		out = append(out, task)
		seen[sa.ID] = true
	}
	for i := len(eventOrder) - 1; i >= 0; i-- {
		key := eventOrder[i]
		task := eventTasks[key]
		if task.ID != "" && seen[task.ID] {
			continue
		}
		out = append(out, task)
	}
	return out
}
