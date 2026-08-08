package builtin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// skillLifecycle is a project-published installation path that is safer and
// narrower than guessing a repository clone. Keep this registry deliberately
// small: an entry is added only when the project's own documentation exposes a
// stable non-interactive lifecycle command.
type skillLifecycle struct {
	Name     string
	Primary  string
	Fallback string
	Note     string
}

var officialSkillLifecycles = map[string]skillLifecycle{
	"anti-ui-slop": {
		Name:    "anti-ui-slop",
		Primary: "npx skills add https://uizze.com --skill anti-ui-slop --yes --global",
		Note:    "use UIZZE's published Agent Skills provider; do not reinterpret the domain as a GitHub repository",
	},
	"hyperframes": {
		Name:     "hyperframes",
		Primary:  "npx hyperframes skills update",
		Fallback: "npx skills add heygen-com/hyperframes --skill hyperframes --full-depth --yes --global",
		Note:     "run the fallback once only if the official HyperFrames lifecycle command fails",
	},
	"ui-radar": {
		Name:    "ui-radar",
		Primary: "npx skills add https://uizze.com --skill ui-radar --yes --global",
		Note:    "use UIZZE's published Agent Skills provider; do not reinterpret the domain as a GitHub repository",
	},
}

// canonicalSkillHints contains spell-check vocabulary, not installation
// sources. In particular, `handoff` has several unrelated publishers, so a
// correction to that name still requires source discovery/clarification.
var canonicalSkillHints = []string{"handoff"}

type installPlanCounts struct {
	installed     int
	ready         int
	discovery     int
	clarification int
}

// planSkillInstall is deliberately read-only. It turns an underspecified
// natural-language install request into a bounded next step without WebSearch,
// speculative GitHub owners, manual clones, or silent substitute skills.
// Running the returned command remains a separate Bash call governed by the
// user's normal permission posture.
func (s Skill) planSkillInstall(requested []string) (*tools.Result, error) {
	if len(requested) == 0 {
		return &tools.Result{
			Output:  "Skill plan_install: names required (pass every name exactly as the user typed it)",
			IsError: true,
		}, nil
	}
	if s.loader == nil {
		return &tools.Result{
			Output:  "Skill: loader not initialized (this is a metis bug — please report)",
			IsError: true,
		}, nil
	}

	// Installers write while Metis is running. Force a fresh catalog scan so a
	// second plan_install call is also the verification step and can produce a
	// truthful final status without restarting the session.
	s.loader.Invalidate()
	catalog, err := s.loader.List()
	if err != nil {
		return &tools.Result{Output: "skill catalog refresh: " + err.Error(), IsError: true}, nil
	}

	active := make(map[string]skills.Skill, len(catalog))
	for _, sk := range catalog {
		key := normalizeSkillName(sk.Name)
		if key != "" {
			active[key] = sk
		}
	}

	// Installation state is deliberately broader than the active catalog.
	// Disabled, ActivateOn-mismatched, and safety-quarantined manifests are
	// still present and must not trigger an install loop. ListDeclared bypasses
	// those availability filters but is never used for invocation.
	declaredCatalog, err := s.loader.ListDeclared()
	if err != nil {
		return &tools.Result{Output: "skill declaration refresh: " + err.Error(), IsError: true}, nil
	}
	declared := make(map[string]skills.Skill, len(declaredCatalog))
	for _, sk := range declaredCatalog {
		key := normalizeSkillName(sk.Name)
		if key != "" {
			declared[key] = sk
		}
	}

	// Candidate names power typo detection only. Sources are never inferred
	// from this set. Include installed names, official lifecycle names, and a
	// tiny vocabulary for user-visible common typos such as hadoff/handoff.
	candidateSet := make(map[string]string, len(declared)+len(officialSkillLifecycles)+len(canonicalSkillHints))
	for key, sk := range declared {
		candidateSet[key] = sk.Name
	}
	for key, lifecycle := range officialSkillLifecycles {
		candidateSet[key] = lifecycle.Name
	}
	for _, name := range canonicalSkillHints {
		candidateSet[normalizeSkillName(name)] = name
	}

	var b strings.Builder
	b.WriteString("skill install plan (local catalog checked first):\n")
	counts := installPlanCounts{}
	seenRequested := map[string]bool{}
	for _, raw := range requested {
		name := strings.TrimSpace(raw)
		key := normalizeSkillName(name)
		if key == "" || seenRequested[key] {
			continue
		}
		seenRequested[key] = true

		if sk, ok := active[key]; ok {
			counts.installed++
			fmt.Fprintf(&b, "- %s: installed", name)
			if sk.Name != name {
				fmt.Fprintf(&b, " as %s", sk.Name)
			}
			if sk.LocalPath != "" {
				fmt.Fprintf(&b, " (%s)", compactSkillPath(sk.LocalPath))
			}
			b.WriteByte('\n')
			continue
		}
		if sk, ok := declared[key]; ok {
			counts.installed++
			fmt.Fprintf(&b, "- %s: installed but currently unavailable to the live catalog; do not reinstall", name)
			if sk.LocalPath != "" {
				fmt.Fprintf(&b, " (%s)", compactSkillPath(sk.LocalPath))
			}
			b.WriteString(". Inspect its disabled/activation/safety status instead.\n")
			continue
		}

		if suggestions := closeSkillNames(key, candidateSet); len(suggestions) > 0 {
			counts.clarification++
			if len(suggestions) == 1 {
				fmt.Fprintf(&b, "- %s: clarification required — did the user mean %q? Do not correct or install it until confirmed.\n", name, suggestions[0])
			} else {
				fmt.Fprintf(&b, "- %s: ambiguous — possible names: %s. Ask once; do not choose for the user.\n", name, strings.Join(quoteNames(suggestions), ", "))
			}
			continue
		}

		if lifecycle, ok := officialSkillLifecycles[key]; ok {
			counts.ready++
			fmt.Fprintf(&b, "- %s: not installed; official lifecycle ready\n", name)
			fmt.Fprintf(&b, "  primary: %s\n", lifecycle.Primary)
			if lifecycle.Fallback != "" {
				fmt.Fprintf(&b, "  fallback: %s\n", lifecycle.Fallback)
			}
			fmt.Fprintf(&b, "  rule: %s; do not WebSearch or git clone first.\n", lifecycle.Note)
			continue
		}

		counts.discovery++
		fmt.Fprintf(&b, "- %s: source unresolved; run exactly one discovery: npx skills find %s\n", name, shellQuote(name))
		b.WriteString("  rule: continue only from the exact source/id returned by that registry. If it returns multiple candidates, no exact match, or a domain-style id that is not an actual GitHub owner/repo, stop and ask the user; never reinterpret the domain as a repository and never substitute a different skill.\n")
	}

	total := counts.installed + counts.ready + counts.discovery + counts.clarification
	if total == 0 {
		return &tools.Result{
			Output:  "Skill plan_install: names were empty after normalization",
			IsError: true,
		}, nil
	}

	switch {
	case counts.installed == total:
		b.WriteString("final: all requested skills are already installed; run no installation commands.\n")
	case counts.clarification > 0:
		fmt.Fprintf(&b, "next: ask one concise clarification for %d ambiguous item(s); only those items are paused. Independently execute the %d exact lifecycle command(s) and at most one registry discovery for each of the %d unresolved-but-unambiguous item(s), then call Skill plan_install again to verify.\n", counts.clarification, counts.ready, counts.discovery)
	default:
		fmt.Fprintf(&b, "next: %d installed, %d lifecycle-ready, %d need one registry discovery. Execute only the bounded steps above, then call Skill plan_install again with the same names to verify and report the final status.\n", counts.installed, counts.ready, counts.discovery)
	}
	return &tools.Result{Output: b.String()}, nil
}

func normalizeSkillName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// closeSkillNames returns only a uniquely closest edit-distance tier. A fuzzy
// hit is a clarification, never an automatic correction. Short names allow one
// edit; names of six or more characters allow two.
func closeSkillNames(requested string, candidates map[string]string) []string {
	if requested == "" {
		return nil
	}
	limit := 1
	if len([]rune(requested)) >= 6 {
		limit = 2
	}
	best := limit + 1
	var hits []string
	for key, display := range candidates {
		if key == requested {
			continue
		}
		d := editDistance(requested, key)
		switch {
		case d < best && d <= limit:
			best = d
			hits = []string{display}
		case d == best && d <= limit:
			hits = append(hits, display)
		}
	}
	sort.Strings(hits)
	return dedupeStrings(hits)
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range ar {
		cur := make([]int, len(br)+1)
		cur[0] = i + 1
		for j, rb := range br {
			cost := 0
			if ra != rb {
				cost = 1
			}
			cur[j+1] = min3(cur[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if a < b {
		b = a
	}
	if b < c {
		return b
	}
	return c
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func quoteNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = fmt.Sprintf("%q", name)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func compactSkillPath(path string) string {
	clean := filepath.Clean(path)
	// Keep verification output concise while still exposing which catalog won.
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if len(parts) > 4 {
		return strings.Join(parts[len(parts)-4:], "/")
	}
	return clean
}
