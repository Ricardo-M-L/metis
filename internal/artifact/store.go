package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/google/uuid"
)

const manifestMaxBytes = 1 << 20

var (
	artifactIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	storeLocks        sync.Map // resolved root -> *sync.RWMutex
)

// Store persists artifacts below one private root. Stores constructed for the
// same resolved root share a process-wide lock, so registry rebuilds and
// concurrent Desktop/agent operations cannot allocate the same version.
type Store struct {
	root string
	mu   *sync.RWMutex
}

// NewStore creates (or opens) a private artifact root.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: empty store root", ErrInvalidPath)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("artifact: create store: %w", err)
	}
	absInfo, err := os.Lstat(abs)
	if err != nil || !absInfo.IsDir() || absInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: store root is not a real directory", ErrUnsafeFile)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("artifact: resolve store: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: store root is not a directory", ErrUnsafeFile)
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		return nil, fmt.Errorf("artifact: protect store: %w", err)
	}
	lock, _ := storeLocks.LoadOrStore(filepath.Clean(resolved), &sync.RWMutex{})
	return &Store{root: filepath.Clean(resolved), mu: lock.(*sync.RWMutex)}, nil
}

// DefaultStore opens the canonical per-user artifact store. Keeping this
// resolver in the shared package ensures CLI and Desktop cannot accidentally
// use different roots; METIS_HOME remains honored through config.Home().
func DefaultStore() (*Store, error) {
	return NewStore(filepath.Join(config.Home(), "artifacts"))
}

// Root returns the resolved private store path.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Create sanitizes html and writes the first immutable version.
func (s *Store) Create(sessionID, title, rawHTML string) (*Manifest, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	title, err := validateTitle(title)
	if err != nil {
		return nil, err
	}
	clean, err := SanitizeHTML(rawHTML)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.NewString()
	for {
		if _, err := os.Lstat(s.artifactDir(id)); errors.Is(err, os.ErrNotExist) {
			break
		}
		id = uuid.NewString()
	}
	now := time.Now().UTC()
	version := newVersion(1, clean, now)
	manifest := Manifest{
		SchemaVersion: SchemaVersion, ID: id, SessionID: sessionID, Title: title,
		MIMEType: MIMEType, CreatedAt: now, UpdatedAt: now, CurrentVersion: 1,
		Versions: []Version{version},
	}
	if err := s.writeNewArtifact(manifest, []byte(clean)); err != nil {
		return nil, err
	}
	result := cloneManifest(manifest)
	return &result, nil
}

// Update appends an immutable version. Only the owning session may update an
// artifact; an empty title preserves the current title.
func (s *Store) Update(sessionID, id, title, rawHTML string) (*Manifest, error) {
	if err := validateArtifactID(id); err != nil {
		return nil, err
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	clean, err := SanitizeHTML(rawHTML)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(title) != "" {
		title, err = validateTitle(title)
		if err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, err := s.loadManifestLocked(id)
	if err != nil {
		return nil, err
	}
	if manifest.SessionID != sessionID {
		return nil, ErrOwnerMismatch
	}
	now := time.Now().UTC()
	version := newVersion(manifest.CurrentVersion+1, clean, now)
	if err := s.writeVersionLocked(id, version.Number, []byte(clean)); err != nil {
		return nil, err
	}
	manifest.Versions = append(manifest.Versions, version)
	manifest.CurrentVersion = version.Number
	manifest.UpdatedAt = now
	if strings.TrimSpace(title) != "" {
		manifest.Title = title
	}
	if err := s.writeManifestLocked(id, manifest); err != nil {
		// The unreferenced immutable version is harmless and can be repaired by
		// a future maintenance pass; never expose it as committed metadata.
		return nil, err
	}
	result := cloneManifest(manifest)
	return &result, nil
}

// List returns manifests newest-first. A non-empty sessionID filters by exact
// owner; empty returns every local artifact.
func (s *Store) List(sessionID string) ([]Manifest, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]Manifest, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := validateArtifactID(entry.Name()); err != nil {
			return nil, fmt.Errorf("artifact: unsafe store entry %q: %w", entry.Name(), err)
		}
		manifest, err := s.loadManifestLocked(entry.Name())
		if err != nil {
			return nil, err
		}
		if manifest.SessionID == sessionID {
			out = append(out, cloneManifest(manifest))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// Get returns one manifest after checking exact session ownership. The owner
// check and manifest read happen under one lock so callers do not need an
// unsafe Get-then-check sequence.
func (s *Store) Get(sessionID, id string) (*Manifest, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if err := validateArtifactID(id); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	manifest, err := s.loadManifestLocked(id)
	if err != nil {
		return nil, err
	}
	if manifest.SessionID != sessionID {
		return nil, ErrOwnerMismatch
	}
	result := cloneManifest(manifest)
	return &result, nil
}

// ReadVersion returns a verified sanitized version after checking exact
// session ownership. version=0 selects the current immutable version.
func (s *Store) ReadVersion(sessionID, id string, version int) ([]byte, *Version, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, nil, err
	}
	if err := validateArtifactID(id); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	manifest, err := s.loadManifestLocked(id)
	if err != nil {
		return nil, nil, err
	}
	if manifest.SessionID != sessionID {
		return nil, nil, ErrOwnerMismatch
	}
	if version == 0 {
		version = manifest.CurrentVersion
	}
	meta, ok := findVersion(manifest, version)
	if !ok {
		return nil, nil, ErrNotFound
	}
	body, err := readPrivateRegularFile(s.versionPath(id, version), MaxHTMLBytes)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != meta.SHA256 || int64(len(body)) != meta.Size {
		return nil, nil, fmt.Errorf("%w: version digest mismatch", ErrUnsafeFile)
	}
	return body, &meta, nil
}

// Export atomically copies a verified version to a new absolute .html file.
// Existing destinations are refused to avoid an implicit destructive write.
func (s *Store) Export(sessionID, id string, version int, destination string) error {
	if !filepath.IsAbs(destination) || !strings.EqualFold(filepath.Ext(destination), ".html") {
		return fmt.Errorf("%w: export destination must be an absolute .html path", ErrInvalidPath)
	}
	if _, err := os.Lstat(destination); err == nil {
		return ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, _, err := s.ReadVersion(sessionID, id, version)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: export parent must exist", ErrInvalidPath)
	}
	file, err := os.CreateTemp(parent, ".metis-artifact-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Linking the complete temporary inode into place is an atomic no-replace
	// publish on every filesystem that supports os.Link. Unlike os.Rename on
	// Unix, it cannot silently overwrite a destination created by a contender
	// after the Lstat above.
	if err := os.Link(tmp, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

// Delete removes one artifact only when sessionID is its exact owner.
func (s *Store) Delete(sessionID, id string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	if err := validateArtifactID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, err := s.loadManifestLocked(id)
	if err != nil {
		return err
	}
	if manifest.SessionID != sessionID {
		return ErrOwnerMismatch
	}
	if err := os.RemoveAll(s.artifactDir(id)); err != nil {
		return fmt.Errorf("artifact: delete: %w", err)
	}
	return nil
}

// DeleteSession removes artifacts whose manifest has an exact owner match.
// It validates every manifest before deleting anything so corrupt state cannot
// make ownership guesses or produce a misleading partial-success result.
func (s *Store) DeleteSession(sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var targets []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := validateArtifactID(entry.Name()); err != nil {
			return fmt.Errorf("artifact: unsafe store entry %q: %w", entry.Name(), err)
		}
		manifest, err := s.loadManifestLocked(entry.Name())
		if err != nil {
			return err
		}
		if manifest.SessionID == sessionID {
			targets = append(targets, s.artifactDir(entry.Name()))
		}
	}
	for _, target := range targets {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("artifact: delete session artifact: %w", err)
		}
	}
	return nil
}

func (s *Store) writeNewArtifact(manifest Manifest, body []byte) error {
	staging, err := os.MkdirTemp(s.root, ".artifact-*.tmp")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	versions := filepath.Join(staging, "versions")
	if err := os.Mkdir(versions, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(versions, 0o700); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(versions, versionFilename(1)), body); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(staging, "manifest.json"), manifest); err != nil {
		return err
	}
	if err := os.Rename(staging, s.artifactDir(manifest.ID)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *Store) writeVersionLocked(id string, number int, body []byte) error {
	path := s.versionPath(id, number)
	if err := requirePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".version-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) writeManifestLocked(id string, manifest Manifest) error {
	path := filepath.Join(s.artifactDir(id), "manifest.json")
	file, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) loadManifestLocked(id string) (Manifest, error) {
	dir := s.artifactDir(id)
	_, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrNotFound
	}
	if err != nil {
		return Manifest{}, err
	}
	if err := requirePrivateDirectory(dir); err != nil {
		return Manifest{}, err
	}
	if err := requirePrivateDirectory(filepath.Join(dir, "versions")); err != nil {
		return Manifest{}, err
	}
	body, err := readPrivateRegularFile(filepath.Join(dir, "manifest.json"), manifestMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrNotFound
	}
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("artifact: decode %s manifest: %w", id, err)
	}
	if err := validateManifest(id, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateManifest(id string, manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || manifest.ID != id || manifest.MIMEType != MIMEType {
		return fmt.Errorf("%w: manifest identity or schema mismatch", ErrUnsafeFile)
	}
	if err := validateSessionID(manifest.SessionID); err != nil {
		return err
	}
	if _, err := validateTitle(manifest.Title); err != nil {
		return err
	}
	if len(manifest.Versions) == 0 || manifest.CurrentVersion < 1 {
		return fmt.Errorf("%w: manifest has no current version", ErrUnsafeFile)
	}
	for i, version := range manifest.Versions {
		if version.Number != i+1 || len(version.SHA256) != sha256.Size*2 || version.Size < 0 || version.Size > MaxHTMLBytes {
			return fmt.Errorf("%w: invalid version metadata", ErrUnsafeFile)
		}
		if _, err := hex.DecodeString(version.SHA256); err != nil {
			return fmt.Errorf("%w: invalid version digest", ErrUnsafeFile)
		}
	}
	if manifest.CurrentVersion != manifest.Versions[len(manifest.Versions)-1].Number {
		return fmt.Errorf("%w: current version is not latest", ErrUnsafeFile)
	}
	return nil
}

func validateArtifactID(id string) error {
	if !artifactIDPattern.MatchString(id) || filepath.Base(id) != id {
		return ErrInvalidID
	}
	return nil
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 256 {
		return ErrInvalidSession
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return ErrInvalidSession
		}
	}
	return nil
}

func validateTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 512 {
		return "", ErrInvalidTitle
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return "", ErrInvalidTitle
		}
	}
	return title, nil
}

func newVersion(number int, html string, now time.Time) Version {
	digest := sha256.Sum256([]byte(html))
	return Version{Number: number, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(html)), CreatedAt: now}
}

func findVersion(manifest Manifest, number int) (Version, bool) {
	if number < 1 || number > len(manifest.Versions) {
		return Version{}, false
	}
	version := manifest.Versions[number-1]
	return version, version.Number == number
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Versions = append([]Version(nil), manifest.Versions...)
	return manifest
}

func (s *Store) artifactDir(id string) string {
	return filepath.Join(s.root, id)
}

func (s *Store) versionPath(id string, version int) string {
	return filepath.Join(s.artifactDir(id), "versions", versionFilename(version))
}

func versionFilename(number int) string {
	return fmt.Sprintf("%06d.html", number)
}

func writePrivateFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeJSONFile(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readPrivateRegularFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() > max {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 || openedInfo.Size() > max {
		return nil, fmt.Errorf("%w: %s changed while opening", ErrUnsafeFile, filepath.Base(path))
	}
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, ErrTooLarge
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(openedInfo, afterInfo) || afterInfo.Size() != int64(len(body)) || afterInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: %s changed while reading", ErrUnsafeFile, filepath.Base(path))
	}
	return body, nil
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must be a private directory", ErrUnsafeFile, filepath.Base(path))
	}
	return nil
}
