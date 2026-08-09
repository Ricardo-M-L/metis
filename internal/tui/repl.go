package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/chzyer/readline"
	"golang.org/x/term"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/version"
)

type REPL struct {
	Loop        *agent.Loop
	Gate        *permission.Gate
	Slash       *slash.Registry
	Session     *session.Store
	SessionID   string
	Styles      *Styles
	Markdown    *glamour.TermRenderer
	UseMarkdown bool
	ShowTokens  bool
	model       string
	// providerName tracks which provider profile the running Loop.Provider
	// was built against — required for cmdModel to call rtpkg.BuildProvider
	// on the right profile when the user does /model <id> mid-session.
	// Empty for legacy callers that didn't thread it through; switchModel
	// then falls back to cfg.Provider.Default.
	providerName       string
	cfg                *config.Config
	skillDir           string
	cmds               *REPLCommandRegistry
	totalTokens        tokenTracker
	baseSystem         string
	baseSystemSections []llm.SystemSection
	// historyCursor is shared with the TUI bridge (when present) and tracks
	// the exact durable prefix of Loop.History across steering/tool turns.
	historyCursor *session.HistoryCursor
	// SessionSwitch rebinds runtime-owned Todo/Task/dump/checkpoint routers.
	SessionSwitch func(sessionID string)
	// SessionBoundary releases process-owned work tied to the session being
	// left. It runs only after the destination is durably created and before
	// the Loop is reset to that destination.
	SessionBoundary func()
	// FreshPermissionMode is the invocation-level baseline used by /new and
	// /branch; it must not be inherited from a resumed bypass session.
	FreshPermissionMode permission.Mode
	// sandbox is the runtime-owned command boundary. It is wired directly by
	// the composition layer so /sandbox and local !cmd remain protected even
	// when the model-facing Bash tool is disabled or hidden from the registry.
	sandbox *sandbox.Manager
	// Directory-scope callbacks share the runtime's live AllowedDirs backend
	// with the Bubble Tea surface. Nil means the embedder did not wire mutable
	// scope management; signal dispatch then says so explicitly.
	DirAdd    func(path string, persist bool) error
	DirRemove func(path string) error
	DirList   func() []string

	stdin io.Reader
	out   io.Writer

	// rl is the readline editor — handles prompt rendering, history (up/down),
	// and tab completion for slash commands. Nil when stdin is not a TTY
	// (CI, shell pipes); we fall back to a plain bufio.Reader in that case.
	rl *readline.Instance

	// streaming buffer: collect assistant text deltas to render markdown
	// once at end of turn rather than re-rendering line-by-line.
	streamBuf strings.Builder

	// outputStyle tracks the user's /output-style choice in the plain REPL.
	// The TUI bridge copies the live Model value into this field and installs
	// ApplyOutputStyle so a command handler updates both surfaces atomically.
	outputStyle string
	// ApplyOutputStyle is nil in the headless readline REPL. Model.asREPL sets
	// it to update the live chat renderer instead of only mutating this
	// short-lived bridge value.
	ApplyOutputStyle func(style string)

	// TUI-only bridges: slash handlers run in the REPL layer but a few
	// genuinely need access to TUI-side state (sub-agent roster) or
	// surfaces (the input textarea). asREPL() fills these closures with
	// live Model pointers; the plain readline-REPL leaves them nil so
	// the handlers fall back to a graceful "(not available in headless
	// REPL)" message instead of crashing. Closures rather than direct
	// pointers because internal/tui/repl.go must not import textarea
	// for the plain-readline path (no terminal in CI / pipes).
	//
	// SubAgentSnapshot returns the current sub-agent roster (Agent tool
	// spawns). Consumed by cmdAgents to print "◇ name [status]" rows.
	SubAgentSnapshot func() []SubAgentInfo
	// InsertInput appends text to the input textarea. Consumed by
	// cmdReview / cmdSecurityReview to pre-populate a review prompt
	// so the user can edit + Enter to submit — instead of the prior
	// "go prompt the model with this string" stub.
	InsertInput func(text string)
	// BgTurnSnapshot returns whether a turn is currently mid-stream
	// + the elapsed time since it started. Empty struct when idle.
	// Consumed by cmdBg to print the live state.
	BgTurnSnapshot func() BgTurnState
	// BypassCache flips a Loop flag that makes the next request skip
	// the prompt-cache breakpoint write. Consumed by cmdBreakCache.
	BypassCache func()
	// RecapSnapshot returns the latest structural turn recap already rendered
	// by the TUI. The headless REPL leaves it nil because it has no recap rows.
	RecapSnapshot func() string

	shouldQuit bool
}

// BgTurnState is the snapshot returned by BgTurnSnapshot for cmdBg.
// IsActive=false means no turn is running; the other fields are zero.
type BgTurnState struct {
	IsActive  bool
	StartTime time.Time
	Model     string
	// QueuedCount is the number of prompts queued up behind the active
	// turn (Phase F: user can keep typing while a turn runs; they
	// land in m.queuedPrompts).
	QueuedCount int
}

func NewREPL(loop *agent.Loop, sl *slash.Registry, st *session.Store, sid string, useMarkdown, showTokens bool, gate *permission.Gate, model, skillDir string) (*REPL, error) {
	w, _, _ := term.GetSize(int(os.Stdout.Fd()))
	md, err := MarkdownRenderer(w)
	if err != nil {
		md = nil
	}
	cursor := session.NewHistoryCursor(loop.History())
	return &REPL{
		Loop:        loop,
		Gate:        gate,
		Slash:       sl,
		Session:     st,
		SessionID:   sid,
		Styles:      NewStyles(),
		Markdown:    md,
		UseMarkdown: useMarkdown,
		ShowTokens:  showTokens,
		model:       model,
		baseSystem:  loop.System,
		baseSystemSections: append([]llm.SystemSection(nil),
			loop.SystemSections...),
		historyCursor: &cursor,
		skillDir:      skillDir,
		cmds:          BuildREPLCommands(),
		stdin:         os.Stdin,
		out:           os.Stdout,
	}, nil
}

// ConfigureProviderSwitch supplies the runtime context needed for a real
// provider rebuild in the plain (non-Bubble Tea) REPL. Keeping this separate
// from NewREPL preserves the public constructor used by embedders while the
// production composition layer can avoid a misleading string-only /model
// switch and can persist Provider correctly in /new headers.
func (r *REPL) ConfigureProviderSwitch(cfg *config.Config, providerName string) {
	if r == nil {
		return
	}
	r.cfg = cfg
	r.providerName = providerName
}

// ConfigureSandbox supplies the runtime-owned OS sandbox independently of
// tool visibility. Registry lookup remains as a compatibility fallback for
// embedders that construct a REPL around a sandboxed Bash tool directly.
func (r *REPL) ConfigureSandbox(manager *sandbox.Manager) {
	if r != nil {
		r.sandbox = manager
	}
}

func (r *REPL) Run(ctx context.Context) (runErr error) {
	defer func() {
		if err := persistSessionState(r.Session, r.SessionID, r.Gate, r.providerName, r.model, r.Loop.System); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	r.printBanner()

	// Readline gives us history (up/down arrows) and tab completion for
	// slash commands. It only works against a TTY; if r.stdin has been
	// redirected (tests, scripts) we'll bypass it inside readInput.
	if f, ok := r.stdin.(*os.File); ok && readline.IsTerminal(int(f.Fd())) {
		histPath := historyFilePath()
		comp := &slashCompleter{source: &replCandidates{Slash: r.Slash, REPL: r.cmds}}
		rl, err := readline.NewEx(&readline.Config{
			Prompt:            r.Styles.UserPrompt.Render("> "),
			HistoryFile:       histPath,
			AutoComplete:      comp,
			InterruptPrompt:   "^C",
			EOFPrompt:         "exit",
			HistorySearchFold: true,
		})
		if err == nil {
			r.rl = rl
			defer rl.Close()
		}
	}

	for {
		if r.shouldQuit {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		text, err := r.readInput()
		if err == io.EOF {
			fmt.Fprintln(r.out)
			fmt.Fprintln(r.out, r.Styles.Hint.Render("bye."))
			return nil
		}
		if err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		turnCtx := ctx
		visibleInput := text
		submitSlashPrompt := false
		reviewInvocation := ""

		// Built-in REPL command? (parsed before LLM)
		if cmd := r.parseREPLCommand(text); cmd != nil {
			name, args := r.extractCommandName(text)
			_ = name // used for future command logging
			output := cmd.Handler(r, args)
			if output != "" {
				fmt.Fprintln(r.out, output)
			}
			if r.shouldQuit {
				return nil
			}
			continue
		}

		// Slash command?
		if handled, display, sig, args := r.Slash.Parse(text); handled {
			// SignalCustomPrompt display is provider-facing prompt material. It
			// must not be echoed to stdout (especially /review's internal frame),
			// and ordinary custom commands should not appear twice.
			if display != "" && sig != slash.SignalCustomPrompt {
				fmt.Fprintln(r.out, display)
			}
			_ = args // consumed by signal handlers below
			support := classifyPlainREPLSignal(sig)
			if support == plainREPLSignalTUIOnly {
				fmt.Fprintln(r.out, r.Styles.Hint.Render(plainREPLTUIOnlyMessage(sig)))
				continue
			}
			if support == plainREPLSignalUnknown {
				fmt.Fprintf(r.out, "slash command signal %d is not implemented in the plain readline REPL\n", sig)
				continue
			}
			switch sig {
			case slash.SignalNone:
				// Handler display (usage/help) was already printed above.
			case slash.SignalQuit:
				return nil
			case slash.SignalClear:
				if err := r.replaceHistory(nil); err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render("clear: "+err.Error()))
					break
				}
				r.Loop.Reset()
				fmt.Fprintln(r.out, r.Styles.Hint.Render("(history cleared)"))
			case slash.SignalCompact:
				fmt.Fprintln(r.out, cmdCompact(r, args))
			case slash.SignalNew:
				// Save daily note before starting new session
				if r.Loop.Memory != nil {
					summary := r.summarizeHistory()
					_ = r.Loop.Memory.SaveDailyNote(r.SessionID, "new", summary)
				}
				newID, err := r.startFreshSession()
				if err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render("new session: "+err.Error()))
				} else {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(started new session "+newID+")"))
				}
			case slash.SignalUndo:
				// Plain REPL mode has no rich-input prefill — show the
				// undone user text as a hint so the user can copy-paste
				// + edit if they want to retry. Matches kimi-cli's
				// non-TUI fallback for /undo prefill.
				if prefill, ok := r.Loop.UndoLastTurnWithPrefill(); ok {
					if err := r.replaceHistory(r.Loop.History()); err != nil {
						fmt.Fprintln(r.out, r.Styles.Err.Render("undo applied in memory but failed to persist: "+err.Error()))
					}
					if prefill != "" {
						p := strings.ReplaceAll(prefill, "\n", " ")
						if len(p) > 80 {
							p = p[:79] + "…"
						}
						hint := "(undid last turn — to retry, repeat: " + p + ")"
						fmt.Fprintln(r.out, r.Styles.Hint.Render(hint))
					} else {
						fmt.Fprintln(r.out, r.Styles.Hint.Render("(undid last turn — type a new prompt to continue)"))
					}
				} else {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(nothing to undo)"))
				}
			case slash.SignalRewind:
				fmt.Fprintln(r.out, r.rewindLastEditTurn())
			case slash.SignalRetry:
				r.retryLastResponse(ctx)
			case slash.SignalHistory:
				fmt.Fprintln(r.out, renderHistoryForREPL(r.Loop.History()))
			case slash.SignalTitle:
				title := strings.TrimSpace(args)
				if title == "" {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(title: type `/title <text>` to set)"))
				} else if r.Session == nil || r.SessionID == "" {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(title: no session store available)"))
				} else if err := r.Session.SetTitle(r.SessionID, title); err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render("title: "+err.Error()))
				} else {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(title set: "+title+")"))
				}
			case slash.SignalBranch:
				if r.Session == nil || r.SessionID == "" {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(branch: no session store)"))
				} else if newID, err := r.branchSession(); err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render("branch: "+err.Error()))
				} else {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(branched → "+newID+")"))
				}
			case slash.SignalSave:
				if r.Session == nil || r.SessionID == "" {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(save: no session store)"))
				} else if err := r.Session.Sync(r.SessionID); err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render("save: "+err.Error()))
				} else {
					fmt.Fprintln(r.out, r.Styles.Hint.Render("(session synced to disk)"))
				}
			case slash.SignalStatus:
				mode := ""
				if r.Gate != nil {
					mode = string(r.Gate.Mode())
				}
				fmt.Fprintln(r.out, renderCurrentSession(r.Session, r.SessionID, r.Loop, r.model, mode))
			case slash.SignalTools:
				fmt.Fprintln(r.out, renderToolsList(r.Loop))
			case slash.SignalSessions:
				fmt.Fprintln(r.out, renderSessionsList(r.Session, 20))
			case slash.SignalSession:
				mode := ""
				if r.Gate != nil {
					mode = string(r.Gate.Mode())
				}
				fmt.Fprintln(r.out, renderCurrentSession(r.Session, r.SessionID, r.Loop, r.model, mode))
			case slash.SignalSkills:
				fmt.Fprintln(r.out, renderSkillsList(r.Loop, r.skillDir))
			case slash.SignalVersion:
				fmt.Fprintln(r.out, renderVersion())
			case slash.SignalAddDir:
				if r.DirAdd == nil {
					fmt.Fprintln(r.out, "/add-dir: runtime directory-scope backend unavailable in this REPL build")
				} else if err := r.DirAdd(args, true); err != nil {
					fmt.Fprintln(r.out, "add-dir: "+err.Error())
				} else {
					fmt.Fprintln(r.out, "(added: "+strings.TrimSpace(args)+")")
				}
			case slash.SignalRemoveDir:
				if r.DirRemove == nil {
					fmt.Fprintln(r.out, "/rm-dir: runtime directory-scope backend unavailable in this REPL build")
				} else if err := r.DirRemove(args); err != nil {
					fmt.Fprintln(r.out, "rm-dir: "+err.Error())
				} else {
					fmt.Fprintln(r.out, "(removed: "+strings.TrimSpace(args)+")")
				}
			case slash.SignalListDirs:
				if r.DirList == nil {
					fmt.Fprintln(r.out, "/add-dir: runtime directory-scope backend unavailable in this REPL build")
				} else if dirs := r.DirList(); len(dirs) == 0 {
					fmt.Fprintln(r.out, "(no additional dirs — `/add-dir <path>` to add one)")
				} else {
					fmt.Fprintln(r.out, "additional dirs:\n  "+strings.Join(dirs, "\n  "))
				}
			case slash.SignalCost:
				fmt.Fprintln(r.out, cmdCost(r, args))
			case slash.SignalDiff:
				fmt.Fprintln(r.out, cmdGitDiff(r, args))
			case slash.SignalDoctor:
				fmt.Fprintln(r.out, cmdDoctor(r, args))
			case slash.SignalStats:
				fmt.Fprintln(r.out, renderREPLStats(r))
			case slash.SignalKeybindings:
				fmt.Fprintln(r.out, renderREPLKeybindings())
			case slash.SignalPermissions:
				if r.Gate == nil {
					fmt.Fprintln(r.out, "permissions: permission gate unavailable")
				} else {
					fmt.Fprintln(r.out, renderPermissions(&Model{gate: r.Gate}))
				}
			case slash.SignalHooks:
				fmt.Fprintln(r.out, renderHooksList(r.cfg))
			case slash.SignalExport:
				fmt.Fprintln(r.out, cmdExport(r, args))
			case slash.SignalReleaseNotes:
				fmt.Fprintln(r.out, renderReleaseNotes())
			case slash.SignalEffort:
				fmt.Fprintln(r.out, cmdEffort(r, args))
			case slash.SignalPRComments:
				fmt.Fprintln(r.out, renderPRComments(args))
			case slash.SignalUpgrade:
				fmt.Fprintln(r.out, renderUpgrade())
			case slash.SignalContext:
				fmt.Fprintln(r.out, renderREPLContext(r))
			case slash.SignalResume:
				id := r.SessionID
				if id == "" {
					id = "<session-id>"
				}
				fmt.Fprintf(r.out, "To resume this session later:\n\n  metis chat --resume %s\n", id)
			case slash.SignalTag:
				if r.Session == nil || r.SessionID == "" {
					fmt.Fprintln(r.out, "(tag: no session store)")
				} else if err := tagCurrentSession(r.Session, r.SessionID, args); err != nil {
					fmt.Fprintln(r.out, "tag: "+err.Error())
				} else {
					fmt.Fprintln(r.out, "(tagged: "+strings.TrimSpace(args)+")")
				}
			case slash.SignalPlan:
				r.setPermissionMode(permission.ModePlan, "plan")
			case slash.SignalAcceptEdits:
				r.setPermissionMode(permission.ModeAcceptEdits, "acceptEdits")
			case slash.SignalBypassPermissions:
				r.setPermissionMode(permission.ModeBypassPermissions, "bypassPermissions")
			case slash.SignalDefault:
				r.setPermissionMode(permission.ModeDefault, "default")
			case slash.SignalDontAsk:
				r.setPermissionMode(permission.ModeDontAsk, "dontAsk")
			case slash.SignalRename:
				title := strings.TrimSpace(args)
				if title == "" || r.Session == nil || r.SessionID == "" {
					fmt.Fprintln(r.out, "rename: no active session/title")
				} else if err := r.Session.SetTitle(r.SessionID, title); err != nil {
					fmt.Fprintln(r.out, "rename: "+err.Error())
				} else {
					fmt.Fprintln(r.out, "(title set: "+title+")")
				}
			case slash.SignalReload:
				fmt.Fprintln(r.out, r.reloadDiskCatalog())
			case slash.SignalBatch:
				// /batch is prompt rewriting, not TUI chrome. Submit the same
				// Research -> Plan -> Execute contract in readline mode that the
				// Bubble Tea dispatcher sends in interactive mode.
				text = slash.BatchPrompt(args)
				submitSlashPrompt = true
			case slash.SignalCustomPrompt:
				customCmd := customCommandFromInput(r.Slash, visibleInput)
				preparedCtx, warnings, err := prepareCustomCommandTurn(turnCtx, customCmd, r.Loop)
				for _, warning := range warnings {
					fmt.Fprintln(r.out, r.Styles.Hint.Render(warning))
				}
				if err != nil {
					fmt.Fprintln(r.out, r.Styles.Err.Render(err.Error()))
					break
				}
				if display == "" {
					break
				}
				turnCtx = preparedCtx
				if strings.EqualFold(slashName(visibleInput), "/review") {
					reviewInvocation = visibleInput
					text = wrapInternalReviewPrompt(display)
				} else {
					text = display
				}
				submitSlashPrompt = true
			default:
				// Known backend signals must never disappear if a future edit
				// forgets to add its concrete case here.
				fmt.Fprintf(r.out, "slash command signal %d is recognized but has no plain REPL backend handler\n", sig)
			}
			if !submitSlashPrompt {
				continue
			}
		}

		if reviewInvocation != "" {
			r.Loop.AppendUserBlocks([]llm.ContentBlock{
				{Type: "text", Text: text},
				{Type: "text", Text: reviewInvocation},
			})
		} else {
			r.Loop.AppendUser(text)
		}
		r.persistTail()
		_ = runtime.AppendHistory(runtime.HistoryEntry{
			SessionID: r.SessionID, Input: visibleInput, Source: "repl",
		})
		if err := r.runTurn(turnCtx); err != nil {
			errMsg := err.Error()
			// Provide helpful guidance for common errors
			if strings.Contains(errMsg, "API key not configured") {
				fmt.Fprintln(r.out, r.Styles.Err.Render("⚠️  "+errMsg))
				fmt.Fprintln(r.out, r.Styles.Hint.Render("  To fix: set ANTHROPIC_API_KEY or OPENAI_API_KEY environment variable,"))
				fmt.Fprintln(r.out, r.Styles.Hint.Render("  or edit ~/.metis/config.toml"))
			} else {
				fmt.Fprintln(r.out, r.Styles.Err.Render("error: "+errMsg))
			}
		}
	}
}

// parseREPLCommand checks if input starts with a known REPL command.
func (r *REPL) parseREPLCommand(text string) *REPLCommand {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/") {
		text = text[1:]
	}
	name, _, _ := cut(text, " ")
	if name == "" {
		return nil
	}
	return r.cmds.Get(name)
}

// extractCommandName splits "cmd arg1 arg2" into ("cmd", "arg1 arg2").
func (r *REPL) extractCommandName(text string) (name, args string) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/") {
		text = text[1:]
	}
	name, args, _ = cut(text, " ")
	return
}

// summarizeHistory extracts a text summary from conversation history for daily notes.
func (r *REPL) summarizeHistory() string {
	history := r.Loop.History()
	if len(history) == 0 {
		return ""
	}
	// Collect recent user/assistant exchanges
	var lines []string
	for _, msg := range history {
		role := string(msg.Role)
		// Extract text content
		var text string
		for _, block := range msg.Content {
			if block.Type == "text" {
				text += block.Text
			}
		}
		if text != "" {
			// Truncate long messages
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			lines = append(lines, role+": "+text)
		}
	}
	// Join with newlines, limit total length
	result := strings.Join(lines, "\n")
	if len(result) > 2000 {
		result = result[:2000] + "\n...(truncated)"
	}
	return result
}

func cut(s, sep string) (before, after string, found bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func (r *REPL) printBanner() {
	w := lipglossWidth(r.out)
	if w <= 0 {
		w = 80
	}
	// Nice ASCII banner - METIS
	banner := `
  _____  _____ _____ _____  __   _     _   _  __ _   _ ____
 |_   _|| ____|_   _| ____| |  \ | |   | | | |/ _` + "`" + ` | | |  _ \
   | |  |  _|   | | |  _|   |   \| |_  | |_| | (_| |_| | |_) |
   | |  | |___  | | | |___  | |\  |  _| |  _  |\___,_/|  __/
   |_|  |_____| |_| |_____| |_| \_|_|   |_| |_|    |/|_|
                      Metis · local-first agent CLI`
	// Color: purple for banner
	bannerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true)
	for _, line := range strings.Split(banner, "\n") {
		if len(line) > w {
			line = line[:w]
		}
		fmt.Fprintln(r.out, bannerStyle.Render(line))
	}

	// Info line
	fmt.Fprintln(r.out)
	infoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b"))

	half := (w - 40) / 2
	padding := strings.Repeat(" ", max(0, half))

	fmt.Fprint(r.out, infoStyle.Render(padding))
	// Banner version mirrors `metis version` — same source so the two never
	// drift. version.Version is build-time injected via -ldflags by the
	// Makefile (falls back to the committed VERSION file).
	// version.Version comes from -ldflags injection (Makefile). Makefile
	// uses `git describe --tags --always --dirty` which already prefixes
	// with "v" when the tag includes one (e.g. "v0.1.1-20-g0c8969d");
	// fallback paths use the bare VERSION file content (e.g. "0.1.1").
	// Add "v" only when missing so we don't render "vv0.1.1-...".
	verLabel := version.Version
	if !strings.HasPrefix(verLabel, "v") {
		verLabel = "v" + verLabel
	}
	fmt.Fprint(r.out, infoStyle.Render(verLabel), infoStyle.Render("  ·  "))
	fmt.Fprint(r.out, hintStyle.Render("model: "), statusStyle.Render(r.model))
	fmt.Fprint(r.out, infoStyle.Render("  ·  "))
	fmt.Fprint(r.out, hintStyle.Render("mode: "), statusStyle.Render(string(r.Gate.Mode())))
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, infoStyle.Render(padding+"(type ")+hintStyle.Render("help")+infoStyle.Render(" for commands, ")+hintStyle.Render("/quit")+infoStyle.Render(" to exit)"))
	fmt.Fprintln(r.out)
}

func (r *REPL) readInput() (string, error) {
	line, err := r.readSingleLine()
	if err != nil {
		return line, err
	}

	// Multi-line: triple-bracket fence
	if line == "<<<" {
		// Switch readline prompt to the continuation hint while collecting
		// follow-up lines so up/down still navigates history but the prompt
		// makes the mode obvious.
		if r.rl != nil {
			origPrompt := r.rl.Config.Prompt
			r.rl.SetPrompt(r.Styles.Hint.Render("… "))
			defer r.rl.SetPrompt(origPrompt)
		}
		var b strings.Builder
		for {
			more, err := r.readSingleLine()
			if err != nil && more == "" {
				return b.String(), err
			}
			if more == ">>>" {
				return b.String(), nil
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(more)
		}
	}
	return line, nil
}

// readSingleLine reads one line from readline (when active) or from stdin
// directly. It's the only place the editor and the bufio fallback
// converge, so future input changes (paste handling, etc) live here.
func (r *REPL) readSingleLine() (string, error) {
	if r.rl != nil {
		line, err := r.rl.Readline()
		if err == readline.ErrInterrupt {
			// Treat Ctrl-C on an empty buffer as EOF; on a non-empty buffer,
			// readline already cleared the line and we just loop.
			if line == "" {
				return "", io.EOF
			}
			return "", nil
		}
		return line, err
	}
	// Non-TTY fallback: write the prompt and read a line from stdin.
	fmt.Fprint(r.out, r.Styles.UserPrompt.Render("> "))
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := r.stdin.Read(tmp)
		if n == 0 {
			if err != nil {
				return string(buf), err
			}
			continue
		}
		if tmp[0] == '\n' {
			return strings.TrimRight(string(buf), "\r"), nil
		}
		buf = append(buf, tmp[0])
	}
}

// historyFilePath returns the on-disk location for readline history.
// Lives under the unified ~/.metis/ home (or $METIS_HOME) — falls back
// to /tmp if HOME is unset so the editor still works.
func historyFilePath() string {
	dir := config.Home()
	if dir == "" {
		return filepath.Join(os.TempDir(), "metis_history")
	}
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "history")
}

func (r *REPL) runTurn(ctx context.Context) error {
	// EventLoopDone normally flushes eagerly below; the deferred retry covers
	// provider errors/cancellation paths that close the event stream first.
	defer r.persistTail()
	events := make(chan agent.Event, eventBufferSize())
	done := make(chan error, 1)

	go func() {
		done <- r.Loop.Run(ctx, events)
		close(events)
	}()

	r.streamBuf.Reset()
	turnStartedText := false

	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			if !turnStartedText {
				fmt.Fprintln(r.out)
				fmt.Fprint(r.out, r.Styles.AssistantTag.Render("● "))
				turnStartedText = true
			}
			fmt.Fprint(r.out, ev.TextDelta)
			r.streamBuf.WriteString(ev.TextDelta)
		case agent.EventToolStart:
			r.flushTextBeforeTool(turnStartedText)
			turnStartedText = false
			argsJSON, _ := json.Marshal(ev.ToolInput)
			fmt.Fprintf(r.out, "%s %s %s\n",
				r.Styles.ToolTag.Render("⚙"),
				r.Styles.ToolName.Render(ev.ToolName),
				r.Styles.ToolArgs.Render(truncate1(string(argsJSON), 200)),
			)
		case agent.EventToolResult:
			if ev.ToolResult == nil {
				continue
			}
			out := truncate1(strings.TrimRight(ev.ToolResult.Output, "\n"), 600)
			indented := indent("    ", out)
			style := r.Styles.ToolResult
			if ev.ToolResult.IsError {
				style = r.Styles.ToolError
			}
			fmt.Fprintln(r.out, style.Render(indented))
		case agent.EventPermissionRequest:
			decision := r.askPermission(ev)
			ev.PermissionReply <- decision
		case agent.EventAskUser:
			// Plain-REPL fallback for the AskUser tool: render question
			// + numbered options, read one line. Empty input = dismiss
			// (the tool surfaces that as IsError so the model can
			// fallback). Non-interactive stdin returns dismiss too.
			answer := r.askUser(ev)
			if ev.AskUserReply != nil {
				ev.AskUserReply <- answer
			}
		case agent.EventTurnEnd:
			r.flushTextBeforeTool(turnStartedText)
			turnStartedText = false
		case agent.EventTokens:
			r.totalTokens.add(ev.InputTokens, ev.OutputTokens, ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
			if r.ShowTokens {
				fmt.Fprintln(r.out, r.Styles.Tokens.Render(
					fmt.Sprintf("    [tokens in=%d out=%d]", ev.InputTokens, ev.OutputTokens),
				))
			}
		case agent.EventLoopDone:
			r.flushTextBeforeTool(turnStartedText)
			r.persistTail()
			fmt.Fprintln(r.out)
			return <-done
		case agent.EventError:
			fmt.Fprintln(r.out, r.Styles.Err.Render("error: "+ev.Err.Error()))
		case agent.EventInfo:
			fmt.Fprintln(r.out, r.Styles.Info.Render("    "+ev.Info))
		case agent.EventPlan:
			_ = runtime.ArchivePlan(runtime.ArchivedPlan{
				SessionID: r.SessionID,
				ToolCalls: ev.ToolCalls,
			})
			fmt.Fprintln(r.out, r.Styles.Info.Render(
				fmt.Sprintf("    (plan archived to ~/.metis/plans · %d tool calls)", len(ev.ToolCalls))))
		}
	}
	return <-done
}

func (r *REPL) flushTextBeforeTool(turnStartedText bool) {
	if !turnStartedText {
		return
	}
	// If we want markdown, re-render the buffered streaming text now that
	// the turn (or sub-turn) is done.
	if r.UseMarkdown && r.Markdown != nil && r.streamBuf.Len() > 0 {
		// move cursor up to overwrite the streamed plain text? Too hacky
		// for v1. Just append a markdown-rendered version below.
		md, err := r.Markdown.Render(r.streamBuf.String())
		if err == nil && strings.TrimSpace(md) != strings.TrimSpace(r.streamBuf.String()) {
			fmt.Fprintln(r.out)
			fmt.Fprintln(r.out, r.Styles.Hint.Render("    ── rendered ──"))
			fmt.Fprint(r.out, indent("    ", strings.TrimRight(md, "\n")))
			fmt.Fprintln(r.out)
		} else {
			fmt.Fprintln(r.out)
		}
	} else {
		fmt.Fprintln(r.out)
	}
	r.streamBuf.Reset()
}

// askUser is the readline-REPL counterpart of the TUI's AskUser
// prompt. Renders the question + numbered options, reads one line of
// input, and resolves numbers to option strings ("2" → options[1]).
// Anything else is returned verbatim so the user can type a freeform
// answer when the tool was dispatched with allow_freeform.
//
// Empty input (or EOF) returns "" → ask.go treats that as "user
// dismissed" and surfaces an IsError result.
func (r *REPL) askUser(ev agent.Event) string {
	var b strings.Builder
	b.WriteString("askuser: ")
	b.WriteString(ev.AskUserQuestion)
	if len(ev.AskUserOptions) > 0 {
		b.WriteString("\n")
		for i, opt := range ev.AskUserOptions {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, opt))
		}
		b.WriteString("pick 1-")
		b.WriteString(fmt.Sprintf("%d", len(ev.AskUserOptions)))
		if ev.AskUserAllowFreeform {
			b.WriteString(" (or type your own answer)")
		}
	} else if ev.AskUserAllowFreeform {
		b.WriteString("\n(type your answer)")
	}
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, r.Styles.PermissionBox.Render(b.String()))
	in := bufio.NewReader(r.stdin)
	fmt.Fprint(r.out, r.Styles.UserPrompt.Render("? "))
	line, err := in.ReadString('\n')
	if err != nil {
		return ""
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return ""
	}
	// Numeric shortcut → option lookup. A bare digit that maps to a
	// valid option returns the option string; otherwise the input is
	// passed through as-is (so "3rd choice please" stays verbatim).
	if n, perr := strconv.Atoi(answer); perr == nil && n >= 1 && n <= len(ev.AskUserOptions) {
		return ev.AskUserOptions[n-1]
	}
	return answer
}

func (r *REPL) askPermission(ev agent.Event) agent.PermissionDecision {
	argsJSON, _ := json.MarshalIndent(ev.PermissionInput, "  ", "  ")
	prompt := fmt.Sprintf("permission: tool=%s\n  %s\nallow? (y)es / (n)o / (a)lways", ev.PermissionTool, string(argsJSON))
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, r.Styles.PermissionBox.Render(prompt))
	in := bufio.NewReader(r.stdin)
	for {
		fmt.Fprint(r.out, r.Styles.UserPrompt.Render("? "))
		line, err := in.ReadString('\n')
		if err != nil {
			return agent.PermissionDecisionDeny
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return agent.PermissionDecisionAllow
		case "n", "no", "":
			return agent.PermissionDecisionDeny
		case "a", "always":
			return agent.PermissionDecisionAlwaysAllow
		}
	}
}

func (r *REPL) persistTail() {
	if r.Session == nil || r.SessionID == "" || r.Loop == nil {
		return
	}
	if r.historyCursor == nil {
		cursor := session.NewHistoryCursor(nil)
		r.historyCursor = &cursor
	}
	_ = r.Session.AppendHistoryTail(r.SessionID, r.Loop.History(), r.historyCursor)
}

func (r *REPL) replaceHistory(history []llm.Message) error {
	if r.historyCursor == nil {
		cursor := session.NewHistoryCursor(nil)
		r.historyCursor = &cursor
	}
	if r.Session == nil || r.SessionID == "" {
		r.historyCursor.Mark(history)
		return nil
	}
	return r.Session.ReplaceHistoryAndMark(r.SessionID, history, r.historyCursor)
}

func lastUserMessage(hist []llm.Message) llm.Message {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == llm.RoleUser {
			return hist[i]
		}
	}
	return llm.Message{}
}

// indent prefixes every line of s with prefix.
func indent(prefix, s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func truncate1(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func lipglossWidth(_ io.Writer) int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
