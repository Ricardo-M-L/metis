package eval

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LoadScenarios reads every *.md file under dir and parses each as a
// scenario. Subdirectories are walked. README.md (case-insensitive) is
// skipped so authors can put a how-to next to scenario files.
func LoadScenarios(dir string) ([]Scenario, error) {
	var out []Scenario
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".md") {
			return nil
		}
		base := strings.ToLower(filepath.Base(path))
		if base == "readme.md" {
			return nil
		}
		s, err := LoadScenario(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// LoadScenario parses one markdown file into a Scenario. The format
// is intentionally narrow: YAML-style front-matter (key: value lines)
// between two `---` fences, then sections marked by `# Prompt` /
// `# Setup` / `# Reward` headings. Anything else is ignored.
//
// Reward lines use a colon-separated mini-DSL:
//
//	contains_all: ["foo", "bar"]      weight=1.0
//	contains_any: ["alpha"]           weight=0.5
//	used_tool: Read                   weight=0.3
//	regex: \d{3}                      weight=0.2
//	length: 10..200                   weight=0.1
//	not_contains: ["error"]           weight=0.5
//
// Trailing `weight=N` is optional (default 1.0).
func LoadScenario(path string) (Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	s := Scenario{
		SourcePath: path,
		Timeout:    60 * time.Second,
	}
	body, fm, err := splitFrontmatter(string(raw))
	if err != nil {
		return Scenario{}, err
	}
	for k, v := range fm {
		switch k {
		case "id":
			s.ID = v
		case "description":
			s.Description = v
		case "tags":
			s.Tags = splitList(v)
		case "timeout_seconds":
			n, err := strconv.Atoi(v)
			if err == nil && n > 0 {
				s.Timeout = time.Duration(n) * time.Second
			}
		}
	}
	if s.ID == "" {
		stem := strings.TrimSuffix(filepath.Base(path), ".md")
		s.ID = stem
	}
	sections := splitSections(body)
	s.Prompt = strings.TrimSpace(sections["prompt"])
	s.Setup = strings.TrimSpace(sections["setup"])
	if s.Prompt == "" {
		return s, fmt.Errorf("scenario %s: missing # Prompt section", s.ID)
	}
	rewardBody := sections["reward"]
	if rewardBody == "" {
		return s, fmt.Errorf("scenario %s: missing # Reward section", s.ID)
	}
	s.Assertions, err = parseAssertions(rewardBody)
	if err != nil {
		return s, fmt.Errorf("scenario %s: %w", s.ID, err)
	}
	if len(s.Assertions) == 0 {
		return s, fmt.Errorf("scenario %s: # Reward section has no parsable assertions", s.ID)
	}
	return s, nil
}

// splitFrontmatter pulls out the YAML-ish key/value block between the
// first two `---` fences. Returns body (everything after the closing
// fence) and the kv map. If no front-matter, body = full input.
func splitFrontmatter(text string) (string, map[string]string, error) {
	fm := map[string]string{}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return text, fm, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", nil, fmt.Errorf("front-matter has no closing ---")
	}
	for i := 1; i < end; i++ {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		colon := strings.Index(ln, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(ln[:colon])
		val := strings.TrimSpace(ln[colon+1:])
		fm[key] = val
	}
	body := strings.Join(lines[end+1:], "\n")
	return body, fm, nil
}

// splitSections returns the body of each `# Heading` section. Heading
// names are lowercased and their leading `#` stripped, so `# Prompt`
// and `## Prompt` both index as "prompt". Body for a section is
// everything between its heading and the next `#` heading at column 0.
func splitSections(body string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var current string
	var buf strings.Builder
	flush := func() {
		if current != "" {
			out[current] = buf.String()
		}
		buf.Reset()
	}
	for scanner.Scan() {
		ln := scanner.Text()
		if strings.HasPrefix(ln, "#") {
			flush()
			head := strings.TrimLeft(ln, "#")
			head = strings.TrimSpace(head)
			current = strings.ToLower(head)
			continue
		}
		buf.WriteString(ln)
		buf.WriteByte('\n')
	}
	flush()
	return out
}

// parseAssertions reads `key: value` lines from the Reward section.
// Each line becomes one Assertion. See LoadScenario doc for syntax.
func parseAssertions(body string) ([]Assertion, error) {
	var out []Assertion
	for _, raw := range strings.Split(body, "\n") {
		ln := strings.TrimSpace(raw)
		if ln == "" || strings.HasPrefix(ln, "//") || strings.HasPrefix(ln, "#") {
			continue
		}
		// Strip optional list marker
		ln = strings.TrimPrefix(ln, "-")
		ln = strings.TrimPrefix(ln, "*")
		ln = strings.TrimSpace(ln)
		colon := strings.Index(ln, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(ln[:colon])
		rest := strings.TrimSpace(ln[colon+1:])
		// Pull off optional `weight=N` suffix
		weight := 1.0
		if idx := strings.LastIndex(rest, "weight="); idx >= 0 {
			tail := rest[idx+len("weight="):]
			tail = strings.TrimSpace(strings.TrimRight(tail, " ,;"))
			if w, err := strconv.ParseFloat(tail, 64); err == nil && w > 0 {
				weight = w
				rest = strings.TrimRight(rest[:idx], " ,;")
				rest = strings.TrimSpace(rest)
			}
		}
		a := Assertion{Weight: weight}
		switch key {
		case "contains_all":
			a.Type = AssertContainsAll
			a.Tokens = splitList(rest)
		case "contains_any":
			a.Type = AssertContainsAny
			a.Tokens = splitList(rest)
		case "not_contains":
			a.Type = AssertNotContains
			a.Tokens = splitList(rest)
		case "used_tool":
			a.Type = AssertUsedTool
			a.Tool = strings.Trim(rest, "\"'")
		case "regex":
			a.Type = AssertRegex
			a.Regex = strings.Trim(rest, "\"'")
		case "length":
			a.Type = AssertLength
			lo, hi, ok := parseRange(rest)
			if !ok {
				return nil, fmt.Errorf("length range %q must be N..M", rest)
			}
			a.LengthMin = lo
			a.LengthMax = hi
		case "max_input_tokens":
			a.Type = AssertMaxInputTokens
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("max_input_tokens %q must be a positive integer", rest)
			}
			a.MaxTokens = n
		case "max_output_tokens":
			a.Type = AssertMaxOutputTokens
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("max_output_tokens %q must be a positive integer", rest)
			}
			a.MaxTokens = n
		default:
			// Unknown key — skip silently so future scenarios with
			// new keys don't break old metis builds.
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// splitList parses ["foo", "bar", "baz"] OR foo,bar,baz into a slice.
// Quotes and brackets are stripped. Empty entries dropped.
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"'")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseRange parses "N..M" → (N, M, true). Returns (0,0,false) on
// parse error.
func parseRange(s string) (int, int, bool) {
	parts := strings.Split(s, "..")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	hi, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, false
	}
	return lo, hi, true
}
