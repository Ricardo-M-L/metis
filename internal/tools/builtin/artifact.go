package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	artifactstore "github.com/Ricardo-M-L/metis/internal/artifact"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Artifact gives the model a session-scoped way to create and inspect durable
// static HTML. Export and deletion deliberately remain user-facing CLI/Desktop
// operations; the model tool cannot turn a content request into a filesystem
// write outside Metis's private store or irreversibly remove saved work.
type Artifact struct {
	tools.BaseTool
	gate  *permission.Gate
	store *artifactstore.Store
}

// NewArtifact constructs the built-in. An optional Store keeps tests and
// embedded runtimes isolated; normal registration resolves DefaultStore lazily
// on first execution so merely assembling a registry does not touch disk.
func NewArtifact(gate *permission.Gate, stores ...*artifactstore.Store) Artifact {
	var store *artifactstore.Store
	if len(stores) > 0 {
		store = stores[0]
	}
	return Artifact{gate: gate, store: store}
}

func (Artifact) Name() string { return "Artifact" }

func (Artifact) ShortDescription() string {
	return "Create, update, list, or read versioned static HTML artifacts owned by the current session. Scripts, network URLs, forms, frames, and active content are removed."
}

func (Artifact) Description() string {
	return `Create and inspect durable local HTML artifacts for the current session.

Actions:
  - create: requires title and html; stores sanitized static HTML as version 1.
  - update: requires id and html; appends an immutable sanitized version. An optional title replaces the title.
  - list: lists artifact manifests owned by the current session.
  - read: requires id and optionally version; returns verified sanitized HTML. Omit version for the current version.

Artifacts are static documents, not applications. Scripts, event handlers, forms, frames, external URLs, active SVG/MathML, and network-capable CSS are removed. Content is limited to 2 MiB. The session is selected by the runtime and cannot be supplied or overridden in tool input. Export and deletion are intentionally available only through the CLI or Desktop UI.`
}

func (Artifact) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string", "enum": []string{"create", "update", "list", "read"},
				"description": "Artifact operation to perform.",
			},
			"id": map[string]any{
				"type": "string", "description": "Artifact ID; required for update and read.",
			},
			"title": map[string]any{
				"type": "string", "maxLength": 512, "description": "Required for create; optional replacement title for update.",
			},
			"html": map[string]any{
				"type": "string", "maxLength": artifactstore.MaxHTMLBytes, "description": "Complete HTML document or fragment; required for create and update.",
			},
			"version": map[string]any{
				"type": "integer", "minimum": 1, "description": "Immutable version to read; omit for current.",
			},
		},
		"additionalProperties": false,
	}
}

func (Artifact) IsReadOnly(in map[string]any) bool {
	action := artifactAction(in)
	return action == "list" || action == "read"
}

func (a Artifact) Concurrency(in map[string]any) tools.Concurrency {
	if a.IsReadOnly(in) {
		return tools.ConcurrencySafe
	}
	return tools.ConcurrencyExclusive
}

func (a Artifact) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if a.IsReadOnly(in) {
		return tools.PermissionAllow, "artifact:read-only"
	}
	if a.gate == nil {
		return tools.PermissionAllow, "artifact:no-gate"
	}
	payload, _ := json.Marshal(in)
	decision, source := a.gate.Check(ctx, a.Name(), string(payload))
	return mapDecision(decision), source
}

func (a Artifact) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	sessionID := tasks.CurrentSessionID()
	if strings.TrimSpace(sessionID) == "" {
		return artifactFailure(artifactstore.ErrInvalidSession), nil
	}
	store, err := a.resolveStore()
	if err != nil {
		return artifactFailure(err), nil
	}

	switch action := artifactAction(in); action {
	case "create":
		title, _ := in["title"].(string)
		html, ok := in["html"].(string)
		if strings.TrimSpace(title) == "" {
			return artifactFailure(errors.New("title is required for create")), nil
		}
		if !ok || strings.TrimSpace(html) == "" {
			return artifactFailure(errors.New("html is required for create")), nil
		}
		manifest, err := store.Create(sessionID, title, html)
		if err != nil {
			return artifactFailure(err), nil
		}
		return artifactManifestResult("created", manifest, manifest.CurrentVersion)

	case "update":
		id := artifactID(in)
		title, _ := in["title"].(string)
		html, ok := in["html"].(string)
		if id == "" {
			return artifactFailure(errors.New("id is required for update")), nil
		}
		if !ok || strings.TrimSpace(html) == "" {
			return artifactFailure(errors.New("html is required for update")), nil
		}
		manifest, err := store.Update(sessionID, id, title, html)
		if err != nil {
			return artifactFailure(err), nil
		}
		return artifactManifestResult("updated", manifest, manifest.CurrentVersion)

	case "list":
		manifests, err := store.List(sessionID)
		if err != nil {
			return artifactFailure(err), nil
		}
		output, err := artifactJSON(map[string]any{"artifacts": manifests})
		if err != nil {
			return nil, err
		}
		return &tools.Result{
			Output:  output,
			Display: "Artifacts",
			Presentation: map[string]any{
				"kind": "artifact", "artifacts": manifests,
			},
		}, nil

	case "read":
		id := artifactID(in)
		if id == "" {
			return artifactFailure(errors.New("id is required for read")), nil
		}
		version, err := artifactVersion(in)
		if err != nil {
			return artifactFailure(err), nil
		}
		manifest, err := store.Get(sessionID, id)
		if err != nil {
			return artifactFailure(err), nil
		}
		body, metadata, err := store.ReadVersion(sessionID, id, version)
		if err != nil {
			return artifactFailure(err), nil
		}
		output, err := artifactJSON(map[string]any{
			"artifact": manifest, "version": metadata, "html": string(body),
		})
		if err != nil {
			return nil, err
		}
		return &tools.Result{
			Output:       output,
			Display:      "Local artifact",
			Presentation: artifactPresentation(manifest, metadata.Number),
		}, nil

	default:
		return artifactFailure(fmt.Errorf("unknown action %q; use create|update|list|read", action)), nil
	}
}

func (a Artifact) resolveStore() (*artifactstore.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	return artifactstore.DefaultStore()
}

func artifactManifestResult(action string, manifest *artifactstore.Manifest, version int) (*tools.Result, error) {
	output, err := artifactJSON(map[string]any{"action": action, "artifact": manifest})
	if err != nil {
		return nil, err
	}
	return &tools.Result{
		Output:       output,
		Display:      "Local artifact",
		Presentation: artifactPresentation(manifest, version),
	}, nil
}

func artifactPresentation(manifest *artifactstore.Manifest, version int) map[string]any {
	presentation := map[string]any{
		"kind": "artifact", "artifact_id": manifest.ID, "version": version,
		"artifact": *manifest, "mime_type": manifest.MIMEType,
	}
	if version > 0 && version <= len(manifest.Versions) {
		presentation["size_bytes"] = manifest.Versions[version-1].Size
	}
	return presentation
}

func artifactFailure(err error) *tools.Result {
	return &tools.Result{Output: "Artifact: " + err.Error(), IsError: true}
}

func artifactJSON(value any) (string, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func artifactAction(in map[string]any) string {
	if in == nil {
		return ""
	}
	action, _ := in["action"].(string)
	return strings.ToLower(strings.TrimSpace(action))
}

func artifactID(in map[string]any) string {
	id, _ := in["id"].(string)
	if strings.TrimSpace(id) == "" {
		id, _ = in["artifact_id"].(string)
	}
	return strings.TrimSpace(id)
}

func artifactVersion(in map[string]any) (int, error) {
	value, ok := in["version"]
	if !ok || value == nil {
		return 0, nil
	}
	var version int
	switch typed := value.(type) {
	case int:
		version = typed
	case int64:
		if typed > int64(math.MaxInt) {
			return 0, errors.New("version is out of range")
		}
		version = int(typed)
	case float64:
		if typed > float64(math.MaxInt) || typed != math.Trunc(typed) {
			return 0, errors.New("version must be a positive integer")
		}
		version = int(typed)
	default:
		return 0, errors.New("version must be a positive integer")
	}
	if version < 1 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}
