package skills

// curator.go — agent-created skill lifecycle management. This closes the
// "self-evolution" loop on the procedural-memory side.
//
// The dreaming subagent SYNTHESIZES skills (synth.go) — it only ever
// *creates*. Without a counterpart that *retires* them, the user-skills
// dir grows without bound: one-off skills the agent wrote for a single
// task, and near-duplicates, pile up and bloat the skill list the model
// must scan every turn. The curator is the skills-side mirror of memdir's
// Ebbinghaus decay (internal/memdir/decay.go): a periodic sweep that
// archives idle agent-created skills.
//
// Semantics borrowed from hermes-agent's curator:
//   - Only agent-created user skills are touched. Installed (directory-
//     form), bundled, project, and pinned skills are never archived.
//   - Archiving is RECOVERABLE (move to <root>/.archive/), never deletion.
//     Auto-deletion never happens — a wrongly-retired skill is one
//     `skills curator restore <name>` away.
//   - Pins protect a skill indefinitely.
//
// Idle signal: file mtime. synth writes it on create; the Skill tool's
// `invoke` refreshes it via os.Chtimes on every use (see
// internal/tools/builtin/skill.go) — so a skill the agent keeps using
// stays fresh, exactly like memdir's LastAccessed reset on reuse. No new
// manifest field is required. (Invoke does NOT rewrite the .md body, so
// the prefix cache / content hash are untouched; only the mtime moves.)
//
// Provenance heuristic: agent-created skills are flat `<name>.md` files
// written directly into the user-skills root by synth.go. Installed
// marketplace skills are unpacked as directories (e.g. `docx/`, `pdf/`),
// so restricting the sweep to flat regular files keeps multi-file
// installed skills off-limits without needing a separate provenance store.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// CuratorIdleDaysDefault matches hermes' 90-day archive window. Tuned to
	// be conservative: a skill must go untouched (neither re-synthesized
	// nor invoked) for a full quarter before it's archived.
	CuratorIdleDaysDefault = 90

	// CuratorIdleStateDaysDefault is the boundary between ACTIVE and IDLE
	// lifecycle states — a skill quiet this long is "idle" (a retirement
	// candidate the user can see in `curator status`) but not yet archived.
	CuratorIdleStateDaysDefault = 21

	curatorArchiveDir = ".archive"          // <root>/.archive/
	curatorPinsFile   = ".curator-pins.json" // <root>/.curator-pins.json
)

// Curator archives idle agent-created skills under a user-skills root.
// Stateless apart from the root path + tuning; safe to construct per-call.
type Curator struct {
	root      string
	idleDays  int // ARCHIVE threshold: quiet this long → archived
	stateDays int // IDLE threshold: quiet this long → idle (but not yet archived)
	usage     *UsageStore
}

// NewCurator returns a Curator for the given user-skills root (typically
// ~/.metis/skills) with the default windows.
func NewCurator(root string) *Curator {
	return &Curator{
		root:      root,
		idleDays:  CuratorIdleDaysDefault,
		stateDays: CuratorIdleStateDaysDefault,
		usage:     NewUsageStore(root),
	}
}

// WithIdleDays overrides the archive window (days). Non-positive values are
// ignored so a misconfigured 0 can't archive every skill on the next sweep.
func (c *Curator) WithIdleDays(d int) *Curator {
	if d > 0 {
		c.idleDays = d
	}
	return c
}

// activity returns the freshness anchor for a skill: the newest of its
// usage-store activity (use/view/patch/created) and the file mtime. The
// usage store is the richer signal; the mtime fallback keeps legacy flat
// .md skills (created before the usage store) and hand-created files aging
// correctly.
func (c *Curator) activity(name string, modTime time.Time) time.Time {
	if c.usage != nil {
		if r, ok := c.usage.Get(name); ok {
			if act, ok2 := r.LatestActivity(); ok2 && act.After(modTime) {
				return act
			}
		}
	}
	return modTime
}

// CuratorResult summarises a Sweep.
type CuratorResult struct {
	Scanned  int      // eligible agent-created skills examined
	Archived []string // skill names moved to .archive/ this sweep
	Skipped  int      // eligible but fresh (recently used) — left in place
	Pinned   int      // eligible but pinned — protected
	Failed   int      // idle but the archive move errored (e.g. cross-device)
}

// errInvalidName guards the externally-supplied names (CLI args) that
// reach Restore / Pin / Unpin from os.Rename / os.WriteFile on a joined
// path. Without it, `restore ../../etc/foo` escapes the archive + root.
func validateSkillName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return fmt.Errorf("skills curator: invalid skill name %q (must be a single path segment)", name)
	}
	return nil
}

// skillFileExt reports whether `name` is a flat skill file and returns
// its base (name minus extension). Case-insensitive so every site — scan,
// archive, Restore, ListArchived — treats a hand-created `Foo.MD` exactly
// like synth's lowercase `foo.md`. (A prior version lowercased the ext in
// scan but compared it raw elsewhere, so an uppercase ext was scanned yet
// could never be archived.)
func skillFileExt(name string) (base string, ok bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return strings.TrimSuffix(name, filepath.Ext(name)), true
	}
	return "", false
}

// findSkillFile locates the flat skill file for `name` in dir, matching
// the base case-sensitively but the extension case-insensitively, and
// returns its full path + on-disk filename. Used by archive (dir=root)
// and Restore (dir=.archive) so the original filename (and its exact
// extension casing) is preserved through the move rather than re-derived.
func findSkillFile(dir, name string) (path, filename string, ok bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if base, isSkill := skillFileExt(e.Name()); isSkill && base == name {
			return filepath.Join(dir, e.Name()), e.Name(), true
		}
	}
	return "", "", false
}

// Sweep archives every idle, non-pinned, agent-created skill under the
// root. Returns a summary; never errors on a single bad file (it's
// skipped) so a malformed skill can't wedge the whole consolidation pass.
// An empty / missing root is a no-op.
func (c *Curator) Sweep(now time.Time) (CuratorResult, error) {
	var res CuratorResult
	if c.root == "" {
		return res, errors.New("skills curator: empty root")
	}
	cands, pins, err := c.scan()
	if err != nil {
		return res, err
	}
	cutoff := time.Duration(c.idleDays) * 24 * time.Hour
	for _, sk := range cands {
		res.Scanned++
		if pins[sk.name] {
			res.Pinned++
			continue
		}
		if now.Sub(c.activity(sk.name, sk.modTime)) < cutoff {
			res.Skipped++
			continue
		}
		if err := c.archive(sk.name); err != nil {
			// Best-effort: a single failed move (permissions, cross-device
			// rename, race with a concurrent restore) shouldn't abort the
			// rest of the sweep — but count it so `curator run` doesn't
			// report a clean pass while silently doing nothing.
			res.Failed++
			continue
		}
		res.Archived = append(res.Archived, sk.name)
	}
	sort.Strings(res.Archived)
	return res, nil
}

// IdleCandidates returns the names of skills that a Sweep at `now` WOULD
// archive — used by `skills curator status` for a dry-run preview.
func (c *Curator) IdleCandidates(now time.Time) ([]string, error) {
	cands, pins, err := c.scan()
	if err != nil {
		return nil, err
	}
	cutoff := time.Duration(c.idleDays) * 24 * time.Hour
	var out []string
	for _, sk := range cands {
		if pins[sk.name] || now.Sub(c.activity(sk.name, sk.modTime)) < cutoff {
			continue
		}
		out = append(out, sk.name)
	}
	sort.Strings(out)
	return out, nil
}

// LifecycleStates buckets eligible (agent-created, non-pinned) skills by
// derived state at `now`: active (recently touched), idle (quiet past the
// idle window but not yet old enough to retire), and candidates (quiet
// past the archive window — a Sweep will retire these). Powers the
// richer `curator status` view.
func (c *Curator) LifecycleStates(now time.Time) (active, idle, candidates []string, err error) {
	cands, pins, err := c.scan()
	if err != nil {
		return nil, nil, nil, err
	}
	idleAfter := time.Duration(c.stateDays) * 24 * time.Hour
	archiveAfter := time.Duration(c.idleDays) * 24 * time.Hour
	for _, sk := range cands {
		if pins[sk.name] {
			continue
		}
		// Same boundary logic as usage.DeriveState (via the shared
		// stateForAge helper), but anchored on the blended activity time
		// (usage record max file mtime) rather than the record alone.
		switch stateForAge(now.Sub(c.activity(sk.name, sk.modTime)), idleAfter, archiveAfter) {
		case StateArchived:
			candidates = append(candidates, sk.name)
		case StateIdle:
			idle = append(idle, sk.name)
		default:
			active = append(active, sk.name)
		}
	}
	sort.Strings(active)
	sort.Strings(idle)
	sort.Strings(candidates)
	return active, idle, candidates, nil
}

type curatorSkill struct {
	name    string
	modTime time.Time
}

// scan returns the agent-created (flat *.md) skills directly under root,
// plus the current pin set. Directory-form (installed) skills, symlinks
// (team-shared skills), dotfiles, and the archive dir itself are excluded.
func (c *Curator) scan() ([]curatorSkill, map[string]bool, error) {
	pins, err := c.loadPins()
	if err != nil {
		return nil, nil, err
	}
	ents, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, pins, nil
		}
		return nil, nil, err
	}
	var out []curatorSkill
	for _, e := range ents {
		name := e.Name()
		if isJunkFilename(name) {
			continue
		}
		base, ok := skillFileExt(name)
		if !ok {
			continue // not a flat .md/.markdown skill (dirs, .json, etc.)
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		// Regular files only, decided from Info().Mode() (lstat-like via
		// ReadDir) rather than DirEntry.Type(): on filesystems that leave
		// d_type unknown (some FUSE/network mounts) Type() is 0 and would
		// wrongly exclude a real file. Mode() is always populated. This
		// still excludes dir-form installed skills AND symlinks — a
		// symlink keeps ModeSymlink here, so archiving (which would move
		// and break the shared-skill link) never happens.
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, curatorSkill{
			name:    base,
			modTime: info.ModTime(),
		})
	}
	return out, pins, nil
}

// archive moves <root>/<name>.md (or .markdown) into <root>/.archive/,
// creating the archive dir on demand. Refuses to clobber an existing
// archived copy of the same name — the agent may have re-created and the
// old archive is still wanted.
func (c *Curator) archive(name string) error {
	src, filename, ok := findSkillFile(c.root, name)
	if !ok {
		return fmt.Errorf("skills curator: %q not found in root", name)
	}
	archDir := filepath.Join(c.root, curatorArchiveDir)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(archDir, filename)
	if _, err := os.Stat(dst); err == nil {
		// Disambiguate with a timestamp so we never silently overwrite a
		// prior archive of a re-created-then-re-retired skill.
		ext := filepath.Ext(filename)
		dst = filepath.Join(archDir, fmt.Sprintf("%s.%d%s", name, time.Now().Unix(), ext))
	}
	return os.Rename(src, dst)
}

// Archive retires a live skill on demand (validated). Exposed so the
// dreaming subagent's consolidation pass can retire a skill it merged into
// an umbrella, and so callers other than Sweep (which uses the internal
// archive) can trigger a single recoverable retirement.
//
// Refuses to archive a pinned skill — pins are the user's explicit "never
// auto-retire this" and must hold on the on-demand path too, not just in
// Sweep. Without this a consolidation pass could archive a pinned skill.
func (c *Curator) Archive(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if c.root == "" {
		return errors.New("skills curator: empty root")
	}
	pins, err := c.loadPins()
	if err != nil {
		return err
	}
	if pins[name] {
		return fmt.Errorf("skills curator: %q is pinned — unpin before archiving", name)
	}
	return c.archive(name)
}

// Restore moves an archived skill back into the active root. Refuses to
// overwrite a live skill of the same name.
func (c *Curator) Restore(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	archDir := filepath.Join(c.root, curatorArchiveDir)
	src, filename, ok := findSkillFile(archDir, name)
	if !ok {
		return fmt.Errorf("skills curator: no archived skill %q", name)
	}
	dst := filepath.Join(c.root, filename)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("skills curator: %q already exists in root — remove or rename it first", name)
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	// Reset the idle clock so a freshly-restored skill isn't re-archived
	// on the very next sweep just because its archived copy kept a stale
	// mtime. The user restored it because they want it back; give it a
	// full idle window to prove its worth.
	now := time.Now()
	_ = os.Chtimes(dst, now, now)
	return nil
}

// ListArchived returns the names of archived skills (without extension),
// sorted. Timestamp-disambiguated archives keep their suffix so the user
// can tell duplicates apart.
func (c *Curator) ListArchived() ([]string, error) {
	archDir := filepath.Join(c.root, curatorArchiveDir)
	ents, err := os.ReadDir(archDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || isJunkFilename(e.Name()) {
			continue
		}
		if base, ok := skillFileExt(e.Name()); ok {
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Pin protects a skill from ever being archived. Idempotent.
func (c *Curator) Pin(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	pins, err := c.loadPins()
	if err != nil {
		return err
	}
	if pins[name] {
		return nil
	}
	pins[name] = true
	return c.savePins(pins)
}

// Unpin removes a pin. Idempotent (unpinning an unpinned skill is fine).
func (c *Curator) Unpin(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	pins, err := c.loadPins()
	if err != nil {
		return err
	}
	if !pins[name] {
		return nil
	}
	delete(pins, name)
	return c.savePins(pins)
}

// Pins returns the sorted set of pinned skill names.
func (c *Curator) Pins() ([]string, error) {
	pins, err := c.loadPins()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pins))
	for n := range pins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (c *Curator) loadPins() (map[string]bool, error) {
	path := filepath.Join(c.root, curatorPinsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		// Fail loudly rather than treating a corrupt pins file as empty:
		// an empty pin set would (a) let the next Pin/Unpin overwrite and
		// permanently lose every real pin, and (b) let Sweep archive a
		// skill the user pinned. Returning the error makes Sweep no-op
		// (fail-safe: never archive when protection state is unreadable)
		// and surfaces the problem on the CLI so the user can repair it.
		return nil, fmt.Errorf("skills curator: corrupt pins file %s: %w (repair or delete it)", path, err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (c *Curator) savePins(pins map[string]bool) error {
	names := make([]string, 0, len(pins))
	for n := range pins {
		names = append(names, n)
	}
	sort.Strings(names)
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write (shared helper): pins are a safety gate — a torn read
	// that reset them to empty would un-protect every pinned skill right
	// before a Sweep, worse than losing advisory usage history.
	return atomicWriteFile(filepath.Join(c.root, curatorPinsFile), data)
}
