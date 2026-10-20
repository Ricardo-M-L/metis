package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/learning"
	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// StageCreateSkill is the automatic Dream write path. The proposal remains
// invisible to normal queries until the host evaluator promotes its digest.
func (s *Synth) StageCreateSkill(meta SynthSkillMeta, body string) (learning.Version, error) {
	name, err := sanitizeName(meta.Name)
	if err != nil {
		return learning.Version{}, err
	}
	meta.Name = name
	meta.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	if s.dir == "" {
		return learning.Version{}, fmt.Errorf("synth: no target dir")
	}
	if baseline, err := LearningBaseline(s.dir, name); err != nil {
		return learning.Version{}, err
	} else if len(baseline) > 0 {
		return learning.Version{}, fmt.Errorf("skill %q already exists; use update_skill", name)
	}
	doc, err := renderFrontmatterDoc(meta, body)
	if err != nil {
		return learning.Version{}, err
	}
	return s.stageManifest(name, nil, []byte(doc))
}

func (s *Synth) StageUpdateSkill(name, body string) (learning.Version, error) {
	clean, err := sanitizeName(name)
	if err != nil {
		return learning.Version{}, err
	}
	existing, err := LearningBaseline(s.dir, clean)
	if err != nil {
		return learning.Version{}, err
	}
	if len(existing) == 0 {
		return learning.Version{}, fmt.Errorf("skill %q does not exist", clean)
	}
	header, _, hasFM := splitFrontmatter(existing)
	out := []byte(strings.TrimSpace(body) + "\n")
	if hasFM {
		out = []byte(fmt.Sprintf("---\n%s\n---\n%s\n", strings.TrimRight(string(header), "\n"), strings.TrimSpace(body)))
	}
	return s.stageManifest(clean, existing, out)
}

// LearningBaseline freezes the actual canonical user source on first use.
// Other supported catalog formats keep their current behavior; versioning
// refuses them explicitly until their auxiliary resources can be evaluated.
// A missing canonical file must never silently hide a .json/tree skill.
func LearningBaseline(dir, name string) ([]byte, error) {
	if err := learning.ValidateName(name); err != nil {
		return nil, err
	}
	if st, err := learning.NewStore(dir).Read(name); err == nil {
		if st.ActiveVersion == "" {
			return nil, nil
		}
		content := []byte(st.Versions[st.ActiveVersion].Content)
		return content, ValidateLearningManifest(name, content)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	all, err := dirLayer("user", 10, dir, pubskill.TrustUser).Scan()
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.Abs(filepath.Join(dir, name+".md"))
	if err != nil {
		return nil, err
	}
	matches := 0
	for _, sk := range all {
		if sk.Name != name {
			continue
		}
		matches++
		if sk.LocalPath != canonical {
			return nil, fmt.Errorf("learning versioning requires canonical %s.md; existing source %s remains active", name, sk.LocalPath)
		}
	}
	if matches > 1 {
		return nil, fmt.Errorf("multiple user sources define skill %q; resolve the ambiguity before versioning", name)
	}
	content, err := os.ReadFile(canonical)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := ValidateLearningManifest(name, content); err != nil {
		return nil, fmt.Errorf("invalid learning baseline: %w", err)
	}
	return content, nil
}

func (s *Synth) stageManifest(name string, baseline, candidate []byte) (learning.Version, error) {
	if err := ValidateLearningManifest(name, candidate); err != nil {
		return learning.Version{}, err
	}
	v, err := learning.NewStore(s.dir).Stage(context.Background(), name, baseline, candidate, SourceAgentSynth)
	if err == nil && s.loader != nil {
		s.loader.Invalidate()
	}
	return v, err
}

// Reject inline shell expansion as well as filesystem tool access during
// evaluation. A candidate cannot obtain an execution surface through Skill.
func ValidateLearningManifest(name string, candidate []byte) error {
	if err := learning.ValidateName(name); err != nil {
		return err
	}
	if len(candidate) == 0 || len(candidate) > learning.MaxContentBytes {
		return fmt.Errorf("learning manifest size invalid")
	}
	sk, err := parseMarkdown(candidate, name)
	if err != nil {
		return err
	}
	if sk.Name != name {
		return fmt.Errorf("candidate manifest name differs from %q", name)
	}
	if sk.Disabled || len(sk.ActivateOn) > 0 {
		return fmt.Errorf("learning requires an enabled skill without activate_on filters")
	}
	if strings.Contains(sk.Prompt, "!`") || strings.Contains(sk.Prompt, "```!") {
		return fmt.Errorf("learning candidates cannot execute inline shell")
	}
	sk.TrustLevel = pubskill.TrustUser
	if report := ScanSkill(sk); !report.IsClean() {
		return fmt.Errorf("candidate rejected by skill scanner: %s", strings.Join(report.Issues, ", "))
	}
	return nil
}

func learningUserLayer(dir string) Layer {
	base := dirLayer("user", 10, dir, pubskill.TrustUser)
	return Layer{Name: base.Name, Priority: base.Priority, Trust: base.Trust, Scan: func() ([]Skill, error) {
		all, err := base.Scan()
		if err != nil {
			return nil, err
		}
		states, err := learning.NewStore(dir).List()
		if err != nil {
			return nil, err
		}
		managed := map[string]bool{}
		for _, st := range states {
			managed[st.Name] = true
		}
		out := make([]Skill, 0, len(all)+len(states))
		for _, sk := range all {
			if !managed[sk.Name] {
				out = append(out, sk)
			}
		}
		for _, st := range states {
			if st.ActiveVersion == "" || st.Suspended {
				continue
			}
			sk, err := parseMarkdown([]byte(st.Versions[st.ActiveVersion].Content), st.Name)
			if err != nil {
				return nil, err
			}
			if sk.Name != st.Name {
				return nil, fmt.Errorf("active skill name mismatch")
			}
			sk.LocalPath = filepath.Join(dir, st.Name+".md")
			sk.LearningVersionSHA256 = st.ActiveVersion
			sk.LearningActivationEpoch = st.ActivationEpoch
			out = append(out, *sk)
		}
		return out, nil
	}}
}

// A revision stamp is checked before every cached lookup. Promotions and
// rollbacks from another process therefore update a resident worker too.
func learningStamp(dir string) string {
	states, err := learning.NewStore(dir).List()
	if err != nil {
		return "error:" + err.Error()
	}
	var b strings.Builder
	for _, st := range states {
		fmt.Fprintf(&b, "%s:%d:%s;", st.Name, st.Revision, st.ActiveVersion)
	}
	return b.String()
}
