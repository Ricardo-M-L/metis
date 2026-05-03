package overlay

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// BtwOverlay implements the /btw side-question modal as an Overlay.
//
// State machine:
//
//	loading=true  → spinner-only view ("· thinking…")
//	loading=false, err==""        → answer view
//	loading=false, err non-empty  → error view
//
// Esc dismisses (returns consumed=true so the host's main Esc handler
// doesn't also clear the input box).
type BtwOverlay struct {
	question string
	answer   string
	err      string
	loading  bool
	closed   bool

	// ask is the runtime callback that fires the actual LLM call. It
	// runs in a tea.Cmd goroutine; the result lands back via
	// BtwAnswerMsg, which the host routes to Apply().
	ask func(ctx context.Context, question string) (string, error)
	ctx context.Context
}

// BtwAnswerMsg is the tea.Msg the host emits when the LLM call returns.
// The host's Update path dispatches it to BtwOverlay.Apply() so the
// modal flips from loading state to answer/error.
type BtwAnswerMsg struct {
	Answer string
	Err    error
}

// NewBtwOverlay constructs a freshly-loading overlay for `question`.
// `ask` is the LLM-call backend (typically runtime.askSideQuestion).
// ctx is plumbed through so the host can cancel pending calls on Esc
// even though OnPop already does that bookkeeping in 95% of cases.
func NewBtwOverlay(ctx context.Context, question string, ask func(context.Context, string) (string, error)) *BtwOverlay {
	return &BtwOverlay{
		question: strings.TrimSpace(question),
		loading:  true,
		ask:      ask,
		ctx:      ctx,
	}
}

func (b *BtwOverlay) Name() string { return "btw" }
func (b *BtwOverlay) Active() bool { return !b.closed }

// Update only handles Esc — once the answer's in, there's nothing for
// the user to interact with. (claude-code's BtwSideQuestion has scroll
// support for long answers; we'll add that when an answer actually
// overflows the viewport.)
func (b *BtwOverlay) Update(msg tea.KeyMsg) (Overlay, tea.Cmd, bool) {
	// v2: tea.KeyMsg is an interface (fmt.Stringer + Key()); compare
	// via String() since the .Type field is gone.
	if msg.String() == "esc" {
		b.closed = true
		return b, nil, true
	}
	// Anything else: don't consume — let typing continue in the input
	// box behind the modal. claude-code's modal also lets typing
	// proceed; the user can keep drafting their next prompt while the
	// side-answer is rendered.
	return b, nil, false
}

func (b *BtwOverlay) View(width, _ int) string {
	if b.closed {
		return ""
	}
	w := width - 8
	if w < 30 {
		w = 30
	}
	if w > 80 {
		w = 80
	}
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	bodyStyle := lipgloss.NewStyle().Width(w - 4)
	hintStyle := lipgloss.NewStyle().Faint(true)

	var sb strings.Builder
	sb.WriteString(titleStyle.Render("/btw — side question"))
	sb.WriteString("\n\n")
	sb.WriteString(bodyStyle.Render(b.question))
	sb.WriteString("\n\n")
	switch {
	case b.err != "":
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("error: " + b.err))
	case b.loading:
		sb.WriteString(hintStyle.Render("· thinking…"))
	default:
		sb.WriteString(bodyStyle.Render(b.answer))
	}
	sb.WriteString("\n\n")
	sb.WriteString(hintStyle.Render("(esc to dismiss)"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("13")).
		Padding(1, 2).
		Width(w).
		Render(sb.String())
}

// OnPush kicks off the LLM call. The Cmd resolves to BtwAnswerMsg which
// the host routes back through Apply().
func (b *BtwOverlay) OnPush() tea.Cmd {
	if b.ask == nil {
		// No backend wired — surface error directly without firing a Cmd.
		b.err = "/btw is not wired in this build"
		b.loading = false
		return nil
	}
	ask := b.ask
	q := b.question
	ctx := b.ctx
	return func() tea.Msg {
		ans, err := ask(ctx, q)
		return BtwAnswerMsg{Answer: ans, Err: err}
	}
}

// OnPop is currently a no-op — the in-flight goroutine will produce a
// BtwAnswerMsg even after Esc, but the closed flag means we'll just
// drop the result. If we add real cancellation later it lives here.
func (b *BtwOverlay) OnPop() tea.Cmd {
	b.closed = true
	return nil
}

// Apply mutates the overlay with the result of the LLM call. The host
// fishes the BtwOverlay out of the Stack (by Name) and calls Apply()
// from its BtwAnswerMsg handler.
func (b *BtwOverlay) Apply(msg BtwAnswerMsg) {
	b.loading = false
	if msg.Err != nil {
		b.err = msg.Err.Error()
		return
	}
	b.answer = strings.TrimSpace(msg.Answer)
}
