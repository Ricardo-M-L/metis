package tui

// keybind_submit.go — handleSubmit + dispatch for slash commands and
// user prompts. Slash commands route through the REPL command registry
// or the slash-signal table; plain user text starts a new turn.

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

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
	"help":        true,
	"cost":        true,
	"tokens":      true,
	"doctor":      true,
	"context":     true,
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
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	// Expand `[Image #N]` placeholders inserted by Ctrl-V back to the
	// concrete cached path. The chat surface shows the friendly tag
	// while the underlying agent + downstream tools see the path so
	// `Read foo.png` and friends still work. Reset the index after
	// expansion so a fresh turn starts numbering from 1 again.
	if len(m.imagePaste) > 0 {
		text = expandPastedImages(text, m.imagePaste)
		m.imagePaste = nil
		m.imageCounter = 0
	}
	// Refuse to submit while a turn is in flight. Without this guard
	// the new submit clears m.streamingText / m.toolEvents and spawns a
	// second runTurnAsync goroutine that races on doneCh — which the
	// user saw as "I typed a message but it never appeared and the
	// previous reply got swallowed." Show a hint, leave the input
	// alone so the user doesn't lose their prompt to a stray Enter.
	if m.turnActive {
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "(turn still running — wait for it to finish, or Ctrl-C to interrupt)",
			Timestamp: time.Now(),
		})
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

		if cmd := m.cmds.Get(name); cmd != nil {
			if cmd.Name == "quit" || cmd.Name == "exit" {
				return m, tea.Quit
			}
			if cmd.Name == "clear" {
				m.loop.Reset()
				m.messages = nil
				m.toolEvents = nil
				m.totalTokens.Reset()
				return m, nil
			}
			output := cmd.Handler(m.asREPL(), args)
			if output != "" {
				if modalCommands[cmd.Name] {
					// Information-dense commands open as a full-window
					// modal overlay (claude-code parity) instead of
					// inlining into the chat scroll. Esc/q to dismiss.
					m.openBodyScreen("/"+cmd.Name, output)
				} else {
					m.messages = append(m.messages, Message{Role: "info", Content: output, Timestamp: time.Now()})
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
			m.loop.Reset()
			m.messages = nil
			m.toolEvents = nil
			m.totalTokens.Reset()
		case slash.SignalNew:
			// Save daily note before starting new session
			if m.loop.Memory != nil {
				summary := m.summarizeHistory()
				_ = m.loop.Memory.SaveDailyNote(m.sessionID, "new", summary)
			}
			m.loop.Reset()
			m.messages = nil
			m.toolEvents = nil
			m.totalTokens.Reset()
			m.firstRender = true // Show banner again for new session
			m.showBanner = true
		case slash.SignalUndo:
			if ok := m.loop.UndoLastTurn(); ok {
				// Trim the visible log to mirror the loop history:
				// drop everything after (and including) the last user
				// message, plus any tool events tied to that turn.
				m.messages = trimVisibleMessagesToLastUser(m.messages)
				m.toolEvents = nil
				m.messages = append(m.messages, Message{Role: "success", Content: "(undid last turn)", Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(nothing to undo)", Timestamp: time.Now()})
			}
		case slash.SignalHistory:
			hs := screen.NewHistoryScreen(m.loop.History(), m.width, m.height)
			hs.Title = "session history (" + m.sessionID + ")"
			m.activeScreen = hs
		case slash.SignalTitle:
			title := strings.TrimSpace(args)
			if title == "" {
				m.messages = append(m.messages, Message{Role: "info", Content: "(title: type `/title <text>` to set)", Timestamp: time.Now()})
			} else if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "warning", Content: "(title: no session store available)", Timestamp: time.Now()})
			} else if err := m.session.SetTitle(m.sessionID, title); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "title: " + err.Error(), Timestamp: time.Now()})
			} else {
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
			m.openBodyScreen("/tools", renderToolsList(m.loop))
		case slash.SignalSessions:
			m.openBodyScreen("/sessions", renderSessionsList(m.session, 20))
		case slash.SignalSession:
			m.openBodyScreen("/session", renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())))
		case slash.SignalStatus:
			// Reuse renderCurrentSession — same data the user wants from
			// /status. Was previously falling through to default which
			// only showed the placeholder "(status: see REPL)".
			m.openBodyScreen("/status", renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())))
		case slash.SignalSkills:
			m.openBodyScreen("/skills", renderSkillsList(m.skillDir))
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
			m.openBodyScreen("/diff", renderDiff())
		case slash.SignalDoctor:
			m.openBodyScreen("/doctor", renderDoctor(m))
		case slash.SignalStats:
			m.openBodyScreen("/stats", renderStats(m))
		case slash.SignalKeybindings:
			m.openBodyScreen("/keybindings", renderKeybindings())
		case slash.SignalPermissions:
			m.openBodyScreen("/permissions", renderPermissions(m))
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
			m.openBodyScreen("/context", renderContext(m))
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
		}
		// All slash commands EXCEPT /batch terminate the turn here. /batch
		// rewrites `text` above and re-enters the agent path below.
		if sig != slash.SignalBatch {
			return m, nil
		}
	}

	// User message → run agent
	m.loop.AppendUser(text)
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
	m.firstStreamAt = time.Time{}
	m.spinnerVerb = pickSpinnerVerb()
	m.spinnerSub = ""
	m.showBanner = false // Hide banner after first message

	go m.runTurnAsync()
	// Critical: must return tickCmd here so spinnerTick events start flowing,
	// otherwise the "thinking" frame and elapsed timer freeze at 0s and the
	// UI looks dead until the LLM replies.
	return m, tickCmd
}
