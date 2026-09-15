package computeruse

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
)

// stageArtifact prefers the Desktop's original release archive. The bundle
// location is only a hint: its manifest is never trusted. Both archive bytes
// and the extracted executable must match the catalog compiled into METIS.
// Only an explicit install/enable (or a previously enabled managed startup)
// reaches this method; status and metadata queries never fetch or install.
func (m *Manager) stageArtifact(ctx context.Context, release Release, destination string) error {
	bundleDir := os.Getenv("METIS_CU_BUNDLE_DIR")
	if bundleDir == "" {
		return m.download(ctx, release, destination)
	}
	if !filepath.IsAbs(bundleDir) {
		return errors.New("computer-use bundle directory must be absolute")
	}
	u, err := url.Parse(release.URL)
	if err != nil {
		return err
	}
	name := path.Base(u.Path)
	if !safeRelative(name) || name == "." {
		return errors.New("invalid pinned computer-use archive basename")
	}
	filename := filepath.Join(bundleDir, name)
	if _, err := os.Lstat(filename); errors.Is(err, os.ErrNotExist) {
		return m.download(ctx, release, destination)
	} else if err != nil {
		return fmt.Errorf("inspect bundled computer-use archive: %w", err)
	}
	if err := copyRegular(ctx, filename, destination, 0600, maxArtifactBytes); err != nil {
		return fmt.Errorf("stage bundled computer-use archive: %w", err)
	}
	digest, err := fileDigest(ctx, destination)
	if err != nil {
		return err
	}
	if digest != release.SHA256 {
		return errors.New("bundled computer-use archive SHA256 mismatch; installation aborted without network fallback")
	}
	info, err := os.Stat(destination)
	if err != nil {
		return err
	}
	if release.Size > 0 && info.Size() != release.Size {
		return errors.New("bundled computer-use archive size differs from pinned size")
	}
	return nil
}
