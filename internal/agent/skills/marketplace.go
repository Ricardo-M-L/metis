package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// Source is an alias for the public marketplace contract; in-tree code
// keeps using `skills.Source` without import churn while plugin authors
// import pkg/skill directly.
type Source = pubskill.Source

// Hash returns the canonical content hash used by Verify. The hash is
// computed over a canonical JSON encoding that excludes:
//   - ContentHash itself (a hash can't include itself)
//   - Uses (changes on every invocation, would drift)
//   - Source (set by the client at fetch time; not part of the manifest)
func Hash(s *Skill) string {
	clone := *s
	clone.ContentHash = ""
	clone.Uses = 0
	clone.Source = ""
	b, err := json.Marshal(&clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Verify checks the manifest's ContentHash against the canonical hash of
// its other fields. If ContentHash is empty the skill is unsigned and we
// return nil — the caller decides whether unsigned skills are acceptable.
func Verify(s *Skill) error {
	if s.ContentHash == "" {
		return nil
	}
	got := Hash(s)
	if got != s.ContentHash {
		return fmt.Errorf("skill %q content hash mismatch (got %s, want %s)",
			s.Name, got[:12], s.ContentHash[:12])
	}
	return nil
}

// GitHubSource resolves `<owner>/<repo>:<name>` references against a public
// GitHub repo's `main` branch. Manifest path is `skills/<name>.json`.
type GitHubSource struct {
	HTTPClient *http.Client
	BaseURL    string // override for tests; defaults to raw.githubusercontent.com
}

func NewGitHubSource() *GitHubSource {
	return &GitHubSource{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		BaseURL:    "https://raw.githubusercontent.com",
	}
}

func (g *GitHubSource) Name() string { return "github" }

// Fetch parses ref of the form "<owner>/<repo>:<name>" and downloads
// `<base>/<owner>/<repo>/main/skills/<name>.json`. Branch is hardcoded to
// "main" — the rare repo using "master" can be adapted later via a longer
// reference syntax.
func (g *GitHubSource) Fetch(ctx context.Context, ref string) (*Skill, error) {
	owner, repo, name, err := parseGitHubRef(ref)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/%s/%s/main/skills/%s.json", g.BaseURL, owner, repo, name)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github fetch %s: %d — %s", url, resp.StatusCode, truncStr(string(body), 200))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var sk Skill
	if err := json.Unmarshal(body, &sk); err != nil {
		return nil, fmt.Errorf("decode skill manifest: %w", err)
	}
	if sk.Name == "" {
		sk.Name = name
	}
	sk.Source = fmt.Sprintf("github:%s/%s:%s", owner, repo, name)
	return &sk, nil
}

func parseGitHubRef(ref string) (owner, repo, name string, err error) {
	colon := strings.LastIndex(ref, ":")
	if colon <= 0 {
		return "", "", "", errors.New("expected <owner>/<repo>:<name>, got " + ref)
	}
	repoPart, name := ref[:colon], ref[colon+1:]
	slash := strings.IndexByte(repoPart, '/')
	if slash <= 0 {
		return "", "", "", errors.New("expected <owner>/<repo>:<name>, got " + ref)
	}
	owner, repo = repoPart[:slash], repoPart[slash+1:]
	if owner == "" || repo == "" || name == "" {
		return "", "", "", errors.New("empty owner/repo/name in " + ref)
	}
	return
}

// Install fetches a skill from src by ref, verifies its content hash, and
// saves it to the store. Returns the saved skill.
func (s *Store) Install(ctx context.Context, src Source, ref string) (*Skill, error) {
	sk, err := src.Fetch(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetch from %s: %w", src.Name(), err)
	}
	if err := Verify(sk); err != nil {
		return nil, fmt.Errorf("verify skill: %w", err)
	}
	if err := s.Save(sk); err != nil {
		return nil, fmt.Errorf("save skill: %w", err)
	}
	return sk, nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
