package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// ErrAuthCancelled is returned by RunAuthWizard when the user aborts (Ctrl-C / Esc)
// before selecting a provider AND entering a key. setupRuntime treats it as a fatal
// "user said no" — we don't want to silently start chat with no key.
var ErrAuthCancelled = errors.New("auth wizard cancelled")

// authProvider is one entry in the provider picker.
//
// `id` is what we persist into auth.json and pass to cfg.ResolveAPIKey().
// `label` is what the user sees. `placeholder` is the example shown above
// the password field — purely cosmetic but it doubles as a "this is what
// the key looks like" sanity hint.
//
// `note` is an optional follow-up tip printed after a successful save.
// MiniMax / Gemini compat entries use it to remind the user they still
// need to point the base_url at the right host in config.toml.
type authProvider struct {
	id          string
	label       string
	keyHelp     string
	placeholder string
	note        string
}

// builtInAuthProviders mirrors opencode's autocomplete entries.
//
// IMPORTANT: the `id` field is what gets saved into auth.json AND looked
// up by cfg.ResolveAPIKey(). The runtime today only understands two
// transport ids ("anthropic" and "openai") plus anything declared under
// [provider.custom] in config.toml.
//
// So MiniMax (which is Anthropic-wire-compatible) and Gemini (OpenAI-
// wire-compatible) both save under the underlying transport id, NOT under
// "minimax" / "gemini" — otherwise the wizard would happily store a key
// the runtime then can't find. The note field tells the user the
// follow-up step (point the base_url at the compat endpoint).
var builtInAuthProviders = []authProvider{
	{
		id: "anthropic", label: "Anthropic",
		keyHelp:     "API key from https://console.anthropic.com",
		placeholder: "sk-ant-...",
	},
	{
		id: "openai", label: "OpenAI",
		keyHelp:     "API key from https://platform.openai.com",
		placeholder: "sk-...",
	},
	{
		id: "anthropic", label: "MiniMax (Anthropic-compat)",
		keyHelp:     "API key from https://platform.minimaxi.com — saved as the Anthropic key",
		placeholder: "eyJh... or sk-cp-...",
		note:        "set [provider.anthropic].base_url = \"https://api.minimaxi.com/anthropic\" in ~/.metis/config.toml",
	},
	{
		id: "openai", label: "Gemini (OpenAI-compat)",
		keyHelp:     "API key from https://aistudio.google.com — saved as the OpenAI key",
		placeholder: "AIza...",
		note:        "set [provider.openai].base_url = \"https://generativelanguage.googleapis.com/v1beta/openai\" in ~/.metis/config.toml",
	},
	{
		id: "custom", label: "Other / Custom provider…",
		keyHelp:     "Enter a provider id (must match [provider.custom.<id>] in config.toml)",
		placeholder: "",
	},
}

// authStep tracks where the wizard is in its little state machine.
// Clearer than a pile of bools and survives future steps (e.g., model picker).
type authStep int

const (
	stepPickProvider authStep = iota
	stepCustomID              // only reached when user selects "custom"
	stepEnterKey
	stepDone
)

// authModel is the bubbletea model behind RunAuthWizard.
//
// Why bubbletea here instead of just prompting on stdin: opencode's flow uses
// arrow-key navigation + masked input, both of which bash `read -s` can't do
// without ANSI hackery. We already pull bubbletea/lipgloss for the chat TUI;
// reusing them keeps the install closure small.
type authModel struct {
	step      authStep
	providers []authProvider
	cursor    int

	// custom-id path
	customID textinput.Model

	// chosen provider state (filled in once cursor commits)
	chosen authProvider

	// key input
	keyInput textinput.Model

	// output
	resultProvider string
	resultKey      string
	err            error
	cancelled      bool

	width int
}

func newAuthModel() authModel {
	ki := textinput.New()
	ki.EchoMode = textinput.EchoPassword
	ki.EchoCharacter = '•'
	ki.Prompt = "› "
	ki.CharLimit = 512
	ki.Focus()

	cid := textinput.New()
	cid.Prompt = "› "
	cid.CharLimit = 64
	cid.Placeholder = "e.g. groq, together, ollama"

	return authModel{
		step:      stepPickProvider,
		providers: builtInAuthProviders,
		keyInput:  ki,
		customID:  cid,
		width:     80,
	}
}

func (m authModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m authModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width

	case tea.KeyPressMsg:
		// v2: KeyPressMsg replaces KeyMsg's struct/.Type pattern; match
		// by .String() — handles named keys ("esc", "enter") + ASCII.
		// Ctrl-C / Esc bail out of the wizard from any step.
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}

		switch m.step {
		case stepPickProvider:
			return m.updatePickProvider(msg)
		case stepCustomID:
			return m.updateCustomID(msg)
		case stepEnterKey:
			return m.updateKey(msg)
		}
	}
	return m, nil
}

func (m authModel) updatePickProvider(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down":
		if m.cursor < len(m.providers)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen = m.providers[m.cursor]
		if m.chosen.id == "custom" {
			m.customID.Focus()
			m.step = stepCustomID
			return m, textinput.Blink
		}
		m.keyInput.Placeholder = m.chosen.placeholder
		m.keyInput.Focus()
		m.step = stepEnterKey
		return m, textinput.Blink
	}
	return m, nil
}

func (m authModel) updateCustomID(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		id := strings.TrimSpace(m.customID.Value())
		if id == "" {
			return m, nil
		}
		m.chosen = authProvider{
			id:          id,
			label:       id,
			keyHelp:     "API key for " + id,
			placeholder: "",
		}
		m.keyInput.Focus()
		m.customID.Blur()
		m.step = stepEnterKey
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.customID, cmd = m.customID.Update(msg)
	return m, cmd
}

func (m authModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		key := strings.TrimSpace(m.keyInput.Value())
		if key == "" {
			return m, nil
		}
		if err := auth.Set(m.chosen.id, key); err != nil {
			m.err = err
			return m, tea.Quit
		}
		m.resultProvider = m.chosen.id
		m.resultKey = key
		m.step = stepDone
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return m, cmd
}

// Style helpers — kept local rather than reaching into the chat-room palette,
// since the wizard runs *before* a config is even loaded and shouldn't depend
// on user theming.
var (
	authTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#64b5f6"))
	authHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	authActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb74d"))
	authInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	authMuted    = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	authError    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e57373")).Bold(true)
)

func (m authModel) View() tea.View {
	// v2: View returns a tea.View struct, not a string. NewView wraps
	// the rendered content; AltScreen / MouseMode etc are declarative
	// fields on the View — auth wizard runs in inline mode (no alt
	// screen) so we don't set those.
	var b strings.Builder

	b.WriteString(authTitle.Render("Metis · sign in") + "\n")
	b.WriteString(authMuted.Render("Stored at "+auth.Path()+" (mode 0600). Esc to cancel.") + "\n\n")

	switch m.step {
	case stepPickProvider:
		b.WriteString(authHelp.Render("Select a provider:") + "\n\n")
		for i, p := range m.providers {
			marker := "  "
			line := p.label
			if i == m.cursor {
				marker = authActive.Render("› ")
				line = authActive.Render(line)
			} else {
				line = authInactive.Render(line)
			}
			b.WriteString(marker + line + "\n")
		}
		b.WriteString("\n" + authMuted.Render("↑/↓ to move, Enter to select, Esc to cancel"))

	case stepCustomID:
		b.WriteString(authHelp.Render("Provider id (lowercase, used as key in auth.json):") + "\n\n")
		b.WriteString(m.customID.View() + "\n\n")
		b.WriteString(authMuted.Render("Enter to continue, Esc to cancel"))

	case stepEnterKey:
		b.WriteString(authHelp.Render("Provider: ") +
			authActive.Render(m.chosen.label) + "\n")
		if m.chosen.keyHelp != "" {
			b.WriteString(authMuted.Render(m.chosen.keyHelp) + "\n")
		}
		b.WriteString("\n" + m.keyInput.View() + "\n\n")
		b.WriteString(authMuted.Render("Input is masked. Enter to save, Esc to cancel."))

	case stepDone:
		if m.err != nil {
			b.WriteString(authError.Render("Failed: "+m.err.Error()) + "\n")
		} else {
			b.WriteString(authActive.Render("✓ Saved "+m.chosen.label+" credentials") + "\n")
			if m.chosen.note != "" {
				b.WriteString(authMuted.Render("  next: "+m.chosen.note) + "\n")
			}
			b.WriteString(authMuted.Render("Continuing to chat…") + "\n")
		}
	}
	return tea.NewView(b.String())
}

// AuthResult is what RunAuthWizard returns on success.
//
// Provider is the id used in auth.json / cfg.ResolveAPIKey.
// Key is included so callers can plug it straight into a provider client without
// re-reading auth.json (this matters because, in the startup-detection path,
// setupRuntime has already loaded cfg before the wizard runs).
type AuthResult struct {
	Provider string
	Key      string
}

// RunAuthWizard launches the bubbletea picker + key prompt and returns the
// result. The wizard writes auth.json itself before returning, so even callers
// that ignore AuthResult will see the side effect on the next cfg reload.
//
// Returns ErrAuthCancelled if the user pressed Ctrl-C / Esc.
//
// Output goes to stderr so a `metis run "..." | jq` style pipeline doesn't
// interleave wizard ANSI with the assistant's stdout payload. (The wizard
// only fires when stderr is a tty anyway — see setupRuntime gating.)
func RunAuthWizard() (AuthResult, error) {
	p := tea.NewProgram(newAuthModel(), tea.WithOutput(os.Stderr))
	finalRaw, err := p.Run()
	if err != nil {
		return AuthResult{}, fmt.Errorf("auth wizard: %w", err)
	}
	final, ok := finalRaw.(authModel)
	if !ok {
		return AuthResult{}, fmt.Errorf("auth wizard: unexpected final model type")
	}
	if final.cancelled {
		return AuthResult{}, ErrAuthCancelled
	}
	if final.err != nil {
		return AuthResult{}, final.err
	}
	if final.resultProvider == "" || final.resultKey == "" {
		return AuthResult{}, ErrAuthCancelled
	}
	return AuthResult{Provider: final.resultProvider, Key: final.resultKey}, nil
}
