package tui

// keybind_submit.go — handleSubmit + dispatch for slash commands and
// user prompts. Slash commands route through the REPL command registry
// or the slash-signal table; plain user text starts a new turn.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/runtime"
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
	"cost":   true,
	"tokens": true,
	"doctor": true,
	// "context" intentionally NOT modal — claude-code parity (2026-05-11
	// user request, image #1): /context renders inline as a chat-style
	// info message so it stays in the transcript and the user doesn't
	// need to press Esc to return to typing. The renderer
	// (render_info.go::renderContext) is grid-shaped but fits inline
	// fine; long output is just scrolled past like any other long reply.
	"stats":       true,
	"keybindings": true,
	"permissions": true,
	"hooks":       true,
	"tools":       true,
	"toolsets":    true,
	"skills":      true,
	"version":     true,
	"env":         true,
	"agents":      true,
	"files":       true,
	"memory":      true,
	"mcp":         true,
	"sessions":    true,
	"statusline":  true,
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	// `[Image #N]` placeholders STAY in the displayed text — the user
	// pasted those and expects to see them in the transcript. The
	// real image bytes are split out into ContentBlocks at the
	// AppendUserBlocks call site below; here we leave the text alone.
	// Refuse to submit while a turn is in flight. Without this guard
	// the new submit clears m.streamingText / m.toolEvents and spawns a
	// second runTurnAsync goroutine that races on doneCh — which the
	// user saw as "I typed a message but it never appeared and the
	// previous reply got swallowed." Show a hint, leave the input
	// alone so the user doesn't lose their prompt to a stray Enter.
	if m.turnActive {
		// Steering (Task #78) + slash-during-steer (Task #87): mid-turn
		// input is queued onto the agent loop and folded into the next
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
		raw := strings.TrimSpace(m.input.Value())
		if raw == "" {
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
					if display == "" {
						return m, nil
					}
					m.loop.SteerInject(display)
					// user-steer (not info) so the steered prompt is
					// visible in the same lane as a normal user message.
					m.messages = append(m.messages, Message{
						Role:      "user-steer",
						Content:   display,
						Timestamp: time.Now(),
					})
					// Mi1 (2026-05-18) — make the steer-vs-queue
					// asymmetry visible. Plain text mid-turn QUEUES
					// (runs as next turn); custom slash commands
					// STEER (fold into running turn). The previous
					// silent steer surprised users who expected
					// queue semantics across the board. Cheap one-
					// line note solves the WTF without changing the
					// documented dispatch contract.
					m.messages = append(m.messages, Message{
						Role:      "info",
						Content:   "(steered into current turn · plain text would have queued — press Esc first to queue this instead)",
						Timestamp: time.Now(),
					})
					m.input.Reset()
					return m, nil
				}
				// MidTurnSafe falls through to the literal-steer path
				// below — same as plain text. Future work: route safe
				// signals through their overlay handlers without re-
				// entering runTurnAsync.
			}
		}

		// Plain text mid-turn: claude-code's queued-messages behavior.
		// Push to FIFO; finalizeTurn dequeues the head and submits it
		// as the next user turn. Distinct from steering (mid-turn
		// injection into the running turn) — that path is now
		// reserved for slash commands above and the explicit /steer
		// alias users can still call.
		m.queuedPrompts = append(m.queuedPrompts, raw)
		// Match claude-code: the queued-message count lives only in
		// the status bar `◷ N queued` chip (render_chrome.go). The
		// in-stream notice row was redundant — every queue add added
		// another scroll-back line, which the user flagged as clutter.
		// (claude-code carries the same model via CoordinatorAgentStatus
		// `· N queued` suffix on the spinner line.)
		m.input.Reset()
		return m, nil
	}
	// claude-code parity: when the palette is open with at least one
	// match, treat Enter as "select the highlighted candidate then
	// submit". Without this, typing /effo + Enter dispatches a literal
	// /effo, which fails as "unknown: /effo — try /help" — even though
	// the palette below the input is showing /effort highlighted.
	// Only auto-promote when the typed name is NOT itself a registered
	// command (so /help typed verbatim still goes to /help, even if the
	// cursor happened to land on /history).
	if m.showPalette && len(m.palMatched) > 0 && strings.HasPrefix(text, "/") {
		typedName, restArgs, hasArgs := cut(text[1:], " ")
		exactREPL := m.cmds.Get(typedName) != nil
		_, exactSlash := m.slash.Get(typedName)
		if !exactREPL && !exactSlash {
			cursor := m.palCursor
			if cursor < 0 || cursor >= len(m.palMatched) {
				cursor = 0
			}
			promoted := "/" + m.palMatched[cursor].Name
			if hasArgs {
				promoted += " " + restArgs
			}
			text = promoted
		}
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
		m.runBashLocal(cmd)
		return m, nil
	}

	// Built-in commands
	if strings.HasPrefix(text, "/") {
		cmdText := text[1:]
		name, args, _ := cut(cmdText, " ")

		// Phase C1: bare `/effort` opens the slider widget BEFORE the
		// REPL command lookup runs cmdEffort (which would inline a
		// renderInfoBox). Explicit `/effort high` falls through to the
		// REPL path so scripted usage stays the same.
		if name == "effort" && strings.TrimSpace(args) == "" {
			eff := screen.NewEffortScreen(string(m.loop.Effort))
			eff.Resize(m.width, m.height)
			m.activeScreen = eff
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
			mp := screen.NewModelScreen(m.model, builtinModelChoices)
			mp.Resize(m.width, m.height)
			m.activeScreen = mp
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
		case "sessions":
			items := m.sessionsPickerItems(20)
			ps := screen.NewPickerScreen("/sessions", pickerSubtitle("recent sessions", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		case "skills":
			items := m.skillsPickerItems()
			ps := screen.NewPickerScreen("/skills", pickerSubtitle("skills loaded", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		case "tools":
			items := m.toolsPickerItems()
			ps := screen.NewPickerScreen("/tools", pickerSubtitle("tools registered", len(items)), items)
			ps.Resize(m.width, m.height)
			m.activeScreen = ps
			return m, nil
		}

		if cmd := m.cmds.Get(name); cmd != nil {
			if cmd.Name == "quit" || cmd.Name == "exit" {
				return m, tea.Quit
			}
			if cmd.Name == "clear" {
				// Consolidated path — see internal/tui/reload.go.
				m.Reload(ReloadOpts{})
				return m, nil
			}
			output := cmd.Handler(m.asREPL(), args)
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
		if display != "" {
			m.messages = append(m.messages, Message{Role: "info", Content: display, Timestamp: time.Now()})
		}
		_ = args // many signals don't need it; the ones that do read below
		switch sig {
		case slash.SignalQuit:
			return m, tea.Quit
		case slash.SignalClear:
			m.Reload(ReloadOpts{})
		case slash.SignalNew:
			m.Reload(ReloadOpts{
				SaveDailyNote: true,
				ResetReason:   "new",
				ShowBanner:    true,
			})
		case slash.SignalUndo:
			// Prefill behaviour: pop the last turn AND drop the user's
			// original text into the input box so they can edit-and-resend
			// instead of retyping. Mirrors kimi-cli's /undo UX. Empty
			// prefill (synthetic turn) preserves the input box untouched.
			if prefill, ok := m.loop.UndoLastTurnWithPrefill(); ok {
				m.messages = trimVisibleMessagesToLastUser(m.messages)
				m.toolEvents = nil
				if prefill != "" {
					m.input.SetValue(prefill)
					m.messages = append(m.messages, Message{Role: "success", Content: "(undid last turn — original text in input)", Timestamp: time.Now()})
				} else {
					m.messages = append(m.messages, Message{Role: "success", Content: "(undid last turn)", Timestamp: time.Now()})
				}
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(nothing to undo)", Timestamp: time.Now()})
			}
		case slash.SignalHistory:
			hs := screen.NewHistoryScreen(m.loop.History(), m.width, m.height)
			hs.Title = "session history (" + m.sessionID + ")"
			m.activeScreen = hs
		case slash.SignalThinkingDisplay:
			// /thinking [show|hide|auto] — flip the transcript-mode
			// preference for reasoning rows. Args are pre-validated
			// to one of the three accepted values by the slash handler.
			arg := strings.TrimSpace(strings.ToLower(args))
			m.thinkingDisplay = arg
			// Invalidate the per-row render cache: the change flips
			// the "collapse / hide / show" branch in buildChatItems,
			// so previously-cached frames must be re-rendered. Pre-
			// invalidation the user sees stale folded rows even after
			// /thinking show until they scroll.
			if m.renderCache != nil {
				m.renderCache.InvalidateAll()
			}
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "thinking display: " + arg,
				Timestamp: time.Now(),
			})
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
			} else if newID, err := m.session.Branch(m.sessionID, m.loop.History()); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "branch: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.sessionID = newID
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
			m.openBodyScreen("/session", renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())))
		case slash.SignalStatus:
			// Reuse renderCurrentSession — same data the user wants from
			// /status. Was previously falling through to default which
			// only showed the placeholder "(status: see REPL)".
			m.openBodyScreen("/status", renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())))
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
			m.openBodyScreen("/cost", renderCost(m))
		case slash.SignalDiff:
			m.openBodyScreen("/diff", renderDiff(m))
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
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "error", Content: "(export: no session store)", Timestamp: time.Now()})
			} else {
				p, err := exportSessionToFile(m.session, m.sessionID)
				if err != nil {
					m.messages = append(m.messages, Message{Role: "error", Content: "export: " + err.Error(), Timestamp: time.Now()})
				} else {
					m.messages = append(m.messages, Message{Role: "success", Content: "(exported → " + p + ")", Timestamp: time.Now()})
				}
			}
		case slash.SignalReleaseNotes:
			m.openBodyScreen("/release-notes", renderReleaseNotes())
		case slash.SignalTheme:
			// /theme stays inline for now — short toggle confirmation,
			// not browseable. Phase C4 will replace with a cycle widget.
			m.messages = append(m.messages, Message{Role: "info", Content: renderTheme(args), Timestamp: time.Now()})
		case slash.SignalEffort:
			// Phase C1: bare `/effort` opens the interactive slider
			// widget (claude-code parity, see image #6 in user's TUI
			// feedback). Explicit form `/effort high` stays inline so
			// scripted / palette-autocomplete usage still works without
			// hijacking the screen.
			if strings.TrimSpace(args) == "" {
				eff := screen.NewEffortScreen(string(m.loop.Effort))
				eff.Resize(m.width, m.height)
				m.activeScreen = eff
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
				text = display
				display = ""
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

		// P2 (2026-05-18) — vision capability gate. If the active
		// provider doesn't advertise vision support, strip image
		// blocks and warn ONCE rather than send a request the model
		// will reject with a cryptic 400. crush parity:
		// filterFileParts() drops non-text on non-vision providers
		// silently. We're slightly louder: a one-line warning so the
		// user knows the image didn't reach the model.
		if m.loop != nil && m.loop.Provider != nil && !pubprov.ProviderSupportsVision(m.loop.Provider) {
			stripped, kept := splitOffImageBlocks(blocks)
			if stripped > 0 {
				m.messages = append(m.messages, Message{
					Role: "warning",
					Content: fmt.Sprintf(
						"image: %d block(s) dropped — current model (%s) doesn't accept vision input. Switch to a vision-capable model (e.g. claude-sonnet, gpt-4o) and re-paste.",
						stripped, m.loop.Model,
					),
					Timestamp: time.Now(),
				})
				blocks = kept
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
	} else {
		m.loop.AppendUser(llmTextRewritten)
	}
	if m.session != nil && m.sessionID != "" {
		_ = m.session.AppendMessage(m.sessionID, lastUserMessage(m.loop.History()))
	}
	// Mirror to ~/.metis/history.jsonl for cross-session prompt search.
	// Fire-and-forget — disk hiccups must not block the chat.
	_ = runtime.AppendHistory(runtime.HistoryEntry{
		SessionID: m.sessionID, Input: text, Source: "tui",
	})
	m.messages = append(m.messages, Message{Role: "user", Content: text, Timestamp: time.Now()})
	m.toolEvents = nil
	m.streamingText = ""
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

	// Snapshot what runTurnAsync needs BEFORE the `go` — see the
	// comment on runTurnAsync for why. m.turnCancel must be written
	// on this (main) thread, not from inside the goroutine; cleared
	// in finalizeTurn when doneCh fires.
	turnCtx, cancel := context.WithCancel(m.ctx)
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
