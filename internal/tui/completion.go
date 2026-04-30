package tui

import (
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

// candidateSource returns the set of slash-style commands a completer
// should match against. Implementations are pure / cheap so the readline
// hot path stays responsive.
type candidateSource interface {
	Candidates() []string
}

// replCandidates merges the global slash registry with the local
// REPLCommandRegistry. Aliases are intentionally excluded — completing
// to the canonical name keeps history searches unambiguous.
type replCandidates struct {
	Slash *slash.Registry
	REPL  *REPLCommandRegistry
}

func (rc *replCandidates) Candidates() []string {
	seen := make(map[string]struct{})
	var out []string
	if rc.Slash != nil {
		for _, c := range rc.Slash.All() {
			if _, ok := seen[c.Name]; !ok {
				seen[c.Name] = struct{}{}
				out = append(out, c.Name)
			}
		}
	}
	if rc.REPL != nil {
		for _, c := range rc.REPL.All() {
			if _, ok := seen[c.Name]; !ok {
				seen[c.Name] = struct{}{}
				out = append(out, c.Name)
			}
		}
	}
	sort.Strings(out)
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
