package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type viewImageSessionReader map[string]map[string][]byte

func (r viewImageSessionReader) ReadImageAttachment(sessionID, id string) ([]byte, error) {
	if data, ok := r[sessionID][id]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("image attachment is not referenced by current session")
}

func TestViewImageAttachmentIDReturnsNativeImageWithoutOriginalPath(t *testing.T) {
	path := writeTinyPNG(t, t.TempDir(), "original.png")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	id := blob.Sum(raw)
	ctx := agent.WithImageAttachmentSession(context.Background(), viewImageSessionReader{"current": {id: raw}}, "current")
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	input := map[string]any{"attachment_id": id}
	if p, why := v.CanUse(ctx, input); p != tools.PermissionAllow {
		t.Fatalf("permission=%v (%s)", p, why)
	}
	got, err := v.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsError || len(got.Images) != 1 || !strings.Contains(got.Output, id) {
		t.Fatalf("result=%+v", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Images[0].Data)
	if err != nil || !bytes.Equal(decoded, raw) || got.Images[0].MediaType != "image/png" {
		t.Fatalf("native image bytes or MIME changed: %v", err)
	}
}

func TestViewImageAttachmentIDRequiresExactlyOneSourceAndBoundSession(t *testing.T) {
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	for _, input := range []map[string]any{
		{},
		{"attachment_id": strings.Repeat("a", 64)},
		{"path": "image.png", "attachment_id": strings.Repeat("a", 64)},
		{"attachment_id": "https://example.com/private.png"},
	} {
		if _, err := v.Execute(context.Background(), input); err == nil {
			t.Fatalf("accepted invalid or unbound source: %#v", input)
		}
	}
	if p, _ := v.CanUse(context.Background(), map[string]any{"attachment_id": strings.Repeat("a", 64)}); p != tools.PermissionDeny {
		t.Fatalf("unbound attachment permission=%v", p)
	}
}

func TestViewImageAttachmentIDSchemaRequiresExactlyOneSource(t *testing.T) {
	schema := (ViewImage{}).InputSchema()
	id := strings.Repeat("a", 64)
	for _, input := range []map[string]any{{"path": "image.png"}, {"attachment_id": id}} {
		if err := pubtool.ValidateInput(schema, input); err != nil {
			t.Fatalf("valid source rejected: %v", err)
		}
	}
	for _, input := range []map[string]any{{}, {"path": "image.png", "attachment_id": id}} {
		if err := pubtool.ValidateInput(schema, input); err == nil {
			t.Fatalf("ambiguous or missing image source accepted: %#v", input)
		}
	}
}

func TestViewImageAttachmentIDConcurrentSessionsDoNotShareBinding(t *testing.T) {
	path := writeTinyPNG(t, t.TempDir(), "bound.png")
	raw, _ := os.ReadFile(path)
	id := blob.Sum(raw)
	reader := viewImageSessionReader{"owner": {id: raw}, "private": {}}
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		for _, sid := range []string{"owner", "private"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := agent.WithImageAttachmentSession(context.Background(), reader, sid)
				result, err := v.Execute(ctx, map[string]any{"attachment_id": id})
				if sid == "owner" && (err != nil || result == nil || len(result.Images) != 1) {
					t.Errorf("owner failed: %v, %+v", err, result)
				}
				if sid == "private" && err == nil {
					t.Errorf("private session read owner's image: %+v", result)
				}
			}()
		}
	}
	wg.Wait()
}
