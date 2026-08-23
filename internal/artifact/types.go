// Package artifact stores model-generated, local HTML artifacts as immutable
// versions. The package deliberately has no HTTP, CLI, or Desktop dependency
// so every surface uses the same validation and persistence boundary.
package artifact

import (
	"errors"
	"time"
)

const (
	// SchemaVersion is the on-disk manifest schema written by this package.
	SchemaVersion = 1
	// MaxHTMLBytes bounds both raw and sanitized artifact content.
	MaxHTMLBytes = 2 * 1024 * 1024
	// MIMEType is the only artifact media type supported by the local MVP.
	MIMEType = "text/html"
)

var (
	ErrInvalidID      = errors.New("artifact: invalid id")
	ErrInvalidPath    = errors.New("artifact: invalid path")
	ErrInvalidSession = errors.New("artifact: invalid session id")
	ErrInvalidTitle   = errors.New("artifact: invalid title")
	ErrNotFound       = errors.New("artifact: not found")
	ErrAlreadyExists  = errors.New("artifact: already exists")
	ErrTooLarge       = errors.New("artifact: HTML exceeds 2 MiB")
	ErrOwnerMismatch  = errors.New("artifact: session does not own artifact")
	ErrUnsafeFile     = errors.New("artifact: unsafe file")
)

// Version describes one immutable sanitized HTML snapshot.
type Version struct {
	Number    int       `json:"number"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// Manifest is the complete local metadata for one artifact. Versions are
// append-only; CurrentVersion selects the version presented by default.
type Manifest struct {
	SchemaVersion  int       `json:"schema_version"`
	ID             string    `json:"id"`
	SessionID      string    `json:"session_id"`
	Title          string    `json:"title"`
	MIMEType       string    `json:"mime_type"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	CurrentVersion int       `json:"current_version"`
	Versions       []Version `json:"versions"`
}
