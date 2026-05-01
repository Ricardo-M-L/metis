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
				m.messages = append(m.messages, Message{Role: "info", Content: output, Timestamp: time.Now()})
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
				m.messages = append(m.messages, Message{Role: "info", Content: "(undid last turn)", Timestamp: time.Now()})
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
				m.messages = append(m.messages, Message{Role: "info", Content: "(title: no session store available)", Timestamp: time.Now()})
			} else if err := m.session.SetTitle(m.sessionID, title); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "title: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(title set: " + title + ")", Timestamp: time.Now()})
			}
		case slash.SignalBranch:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "info", Content: "(branch: no session store)", Timestamp: time.Now()})
			} else if newID, err := m.session.Branch(m.sessionID, m.loop.History()); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "branch: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.sessionID = newID
				m.messages = append(m.messages, Message{Role: "info", Content: "(branched → " + newID + ")", Timestamp: time.Now()})
			}
		case slash.SignalSave:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "info", Content: "(save: no session store)", Timestamp: time.Now()})
			} else if err := m.session.Sync(m.sessionID); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "save: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(session synced to disk)", Timestamp: time.Now()})
			}
		case slash.SignalTools:
			m.messages = append(m.messages, Message{Role: "info", Content: renderToolsList(m.loop), Timestamp: time.Now()})
		case slash.SignalSessions:
			m.messages = append(m.messages, Message{Role: "info", Content: renderSessionsList(m.session, 20), Timestamp: time.Now()})
		case slash.SignalSession:
			m.messages = append(m.messages, Message{Role: "info", Content: renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())), Timestamp: time.Now()})
		case slash.SignalStatus:
			// Reuse renderCurrentSession — same data the user wants from
			// /status. Was previously falling through to default which
			// only showed the placeholder "(status: see REPL)".
			m.messages = append(m.messages, Message{Role: "info", Content: renderCurrentSession(m.session, m.sessionID, m.loop, m.model, string(m.gate.Mode())), Timestamp: time.Now()})
		case slash.SignalSkills:
			m.messages = append(m.messages, Message{Role: "info", Content: renderSkillsList(m.skillDir), Timestamp: time.Now()})
		case slash.SignalVersion:
			m.messages = append(m.messages, Message{Role: "info", Content: renderVersion(), Timestamp: time.Now()})
		case slash.SignalAddDir:
			if m.ext.DirAdd == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "(/add-dir not wired in this build)", Timestamp: time.Now()})
			} else if err := m.ext.DirAdd(args, true); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "add-dir: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(added: " + args + ")", Timestamp: time.Now()})
			}
		case slash.SignalRemoveDir:
			if m.ext.DirRemove == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "(/rm-dir not wired in this build)", Timestamp: time.Now()})
			} else if err := m.ext.DirRemove(args); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "rm-dir: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(removed: " + args + ")", Timestamp: time.Now()})
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
			m.messages = append(m.messages, Message{Role: "info", Content: renderDiff(), Timestamp: time.Now()})
		case slash.SignalDoctor:
			m.messages = append(m.messages, Message{Role: "info", Content: renderDoctor(m), Timestamp: time.Now()})
		case slash.SignalStats:
			m.messages = append(m.messages, Message{Role: "info", Content: renderStats(m), Timestamp: time.Now()})
		case slash.SignalKeybindings:
			m.messages = append(m.messages, Message{Role: "info", Content: renderKeybindings(), Timestamp: time.Now()})
		case slash.SignalPermissions:
			m.messages = append(m.messages, Message{Role: "info", Content: renderPermissions(m), Timestamp: time.Now()})
		case slash.SignalHooks:
			m.messages = append(m.messages, Message{Role: "info", Content: renderHooksList(m.cfg), Timestamp: time.Now()})
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
					m.messages = append(m.messages, Message{Role: "info", Content: "(exported → " + p + ")", Timestamp: time.Now()})
				}
			}
		case slash.SignalReleaseNotes:
			m.messages = append(m.messages, Message{Role: "info", Content: renderReleaseNotes(), Timestamp: time.Now()})
		case slash.SignalTheme:
			m.messages = append(m.messages, Message{Role: "info", Content: renderTheme(args), Timestamp: time.Now()})
		case slash.SignalEffort:
			m.messages = append(m.messages, Message{Role: "info", Content: renderEffort(args), Timestamp: time.Now()})
		case slash.SignalPRComments:
			m.messages = append(m.messages, Message{Role: "info", Content: renderPRComments(args), Timestamp: time.Now()})
		case slash.SignalUpgrade:
			m.messages = append(m.messages, Message{Role: "info", Content: renderUpgrade(), Timestamp: time.Now()})
		case slash.SignalContext:
			m.messages = append(m.messages, Message{Role: "info", Content: renderContext(m), Timestamp: time.Now()})
		case slash.SignalResume:
			m.messages = append(m.messages, Message{Role: "info", Content: renderResumeHelp(m), Timestamp: time.Now()})
		case slash.SignalTag:
			if m.session == nil || m.sessionID == "" {
				m.messages = append(m.messages, Message{Role: "error", Content: "(tag: no session store)", Timestamp: time.Now()})
			} else if err := tagCurrentSession(m.session, m.sessionID, args); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "tag: " + err.Error(), Timestamp: time.Now()})
			} else {
				m.messages = append(m.messages, Message{Role: "info", Content: "(tagged: " + args + ")", Timestamp: time.Now()})
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
