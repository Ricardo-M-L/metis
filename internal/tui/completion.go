package tui

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

// candidateSource returns the set of slash-style commands a completer should
// match against. Implementations are pure / cheap so readline stays responsive.
type candidateSource interface {
	Candidates() []string
}

// replCandidates adapts the same effective metadata catalog used by the TUI
// palette and /help. Aliases are intentionally excluded from returned strings
// so readline writes canonical names into history.
type replCandidates struct {
	Slash *slash.Registry
	REPL  *REPLCommandRegistry
}

func (rc *replCandidates) Candidates() []string {
	catalog := effectiveCommandCatalog(rc.REPL, rc.Slash)
	out := make([]string, 0, len(catalog))
	for _, cmd := range catalog {
		out = append(out, cmd.Name)
	}
	return out
}

// slashCompleter implements readline.AutoCompleter. We only complete after
// `/` to avoid surprising users when they're typing prose for the model.
type slashCompleter struct {
	source candidateSource
}

func (c *slashCompleter) Do(line []rune, pos int) ([][]rune, int) {
	text := string(line[:pos])
	if !strings.HasPrefix(text, "/") {
		return nil, 0
	}
	prefix := text[1:]
	if c.source == nil {
		return nil, len(prefix)
	}
	var out [][]rune
	for _, name := range c.source.Candidates() {
		if strings.HasPrefix(name, prefix) {
			out = append(out, []rune(name[len(prefix):]))
		}
	}
	return out, len(prefix)
}
