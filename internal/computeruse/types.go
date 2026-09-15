// Package computeruse manages the private METIS computer-use component.
package computeruse

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
)

const ProtocolVersion = 1

var (
	ErrNotInstalled      = errors.New("computer-use helper is not installed")
	ErrNoOfficialRelease = errors.New("compatible official computer-use release not published; use explicit local install")
)

type Description struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	ProtocolVersion int               `json:"protocolVersion"`
	Platform        string            `json:"platform"`
	Arch            string            `json:"arch"`
	Capabilities    []string          `json:"capabilities"`
	Permissions     map[string]string `json:"permissions"`
}

type Status struct {
	Installed   bool         `json:"installed"`
	Enabled     bool         `json:"enabled"`
	Running     bool         `json:"running"`
	Path        string       `json:"path"`
	Version     string       `json:"version"`
	Source      string       `json:"source"`
	Description *Description `json:"description"`
	Message     string       `json:"message"`
}

// Release describes a pinned archive, not an unverified executable from PATH.
// SHA256 pins the downloaded artifact; BinarySHA256 independently pins the
// executable used at runtime. BinaryPath is relative to the extracted root.
type Release struct {
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocolVersion"`
	Target          string `json:"target"`
	URL             string `json:"url"`
	SHA256          string `json:"sha256"`
	BinarySHA256    string `json:"binarySha256"`
	Format          string `json:"format"`
	BinaryPath      string `json:"binaryPath"`
	Size            int64  `json:"size,omitempty"`
}

type Manifest struct {
	Releases []Release `json:"releases"`
}

// Options permits a separately pinned catalog and HTTP transport to be supplied
// by tests or embedders. It does not enable discovery from a remote manifest.
type Options struct {
	Manifest *Manifest
	Client   *http.Client
}

type Manager struct {
	root     string
	manifest Manifest
	client   *http.Client
}

// New stores components beneath METIS_HOME/components/computer-use.
func New(root string) *Manager { return NewWithOptions(root, Options{}) }

func NewWithOptions(root string, options Options) *Manager {
	manifest := PinnedManifest()
	if options.Manifest != nil {
		manifest.Releases = append([]Release(nil), options.Manifest.Releases...)
	}
	client := options.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Manager{root: root, manifest: manifest, client: client}
}

func (m *Manager) directory() (string, error) {
	if m.root == "" {
		return "", errors.New("METIS_HOME must not be empty")
	}
	return filepath.Abs(filepath.Join(m.root, "components", "computer-use"))
}

// Resolve never searches PATH and never downloads implicitly.
func (m *Manager) Resolve(ctx context.Context) (string, error) {
	status, err := m.Status(ctx)
	if err != nil {
		return "", err
	}
	if !status.Installed {
		return "", ErrNotInstalled
	}
	return status.Path, nil
}
