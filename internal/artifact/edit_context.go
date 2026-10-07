package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ArtifactEditPromptPrefix is the server's canonical point-selection request.
// A quoted reference or a mention elsewhere in a prompt is not an edit binding.
const ArtifactEditPromptPrefix = "请根据用户在 HTML Artifact 上的点选，修改现有产物。"

var (
	ErrInvalidEditReference = errors.New("artifact: invalid selected edit reference")
	editDigestPattern       = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	editTargetPattern       = regexp.MustCompile(`^target-[1-9][0-9]{0,3}$`)
)

// EditReference captures a selected immutable snapshot, never a model-chosen
// replacement base. It has no agent or UI dependency.
type EditReference struct {
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
	Digest     string `json:"digest"`
	TargetID   string `json:"targetId"`
}

type editContextKey struct{}
type selectedEdit struct {
	owner     string
	reference EditReference
	bound     bool
}

// ParseEditReference recognizes only the first JSON fence in a leading
// canonical request. Invalid canonical input fails closed; ordinary prompts
// return nil without changing their semantics. Reference text cannot choose
// an owner or provide context/runtime options.
func ParseEditReference(prompt string) (*EditReference, error) {
	if prompt != ArtifactEditPromptPrefix && !strings.HasPrefix(prompt, ArtifactEditPromptPrefix+"\n") {
		return nil, nil
	}
	invalid := func(reason string) (*EditReference, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidEditReference, reason)
	}
	if len(prompt) > 64<<10 || !utf8.ValidString(prompt) {
		return invalid("invalid or oversized request")
	}
	rest := prompt[len(ArtifactEditPromptPrefix):]
	fence := strings.Index(rest, "```")
	if fence < 0 || (fence > 0 && rest[fence-1] != '\n') || !strings.HasPrefix(rest[fence:], "```json\n") {
		return invalid("the first fence must contain the annotation JSON")
	}
	jsonText := rest[fence+len("```json\n"):]
	end := strings.Index(jsonText, "\n```")
	if end < 0 {
		return invalid("missing annotation fence end")
	}
	jsonText = jsonText[:end]
	if err := validateEditJSONFields(json.NewDecoder(strings.NewReader(jsonText)), 0); err != nil {
		return invalid("ambiguous annotation JSON")
	}
	var payload struct {
		Annotation *struct {
			EditReference
			Instruction string `json:"instruction"`
			Title       string `json:"title"`
			Selection   struct {
				ID       string `json:"id"`
				Tag      string `json:"tag"`
				Selector string `json:"selector"`
				Text     string `json:"text"`
			} `json:"selection"`
		} `json:"metis_artifact_annotation"`
	}
	decoder := json.NewDecoder(strings.NewReader(jsonText))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Annotation == nil {
		return invalid("missing or malformed annotation")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("trailing annotation JSON")
	}
	annotation := payload.Annotation
	ref := annotation.EditReference
	if validateArtifactID(ref.ArtifactID) != nil || ref.Version < 1 || !editDigestPattern.MatchString(ref.Digest) || !editTargetPattern.MatchString(ref.TargetID) {
		return invalid("invalid snapshot identity")
	}
	targetNumber, err := strconv.Atoi(strings.TrimPrefix(ref.TargetID, "target-"))
	if err != nil || targetNumber > 2000 || annotation.Selection.ID != ref.TargetID {
		return invalid("invalid selected target")
	}
	if strings.TrimSpace(annotation.Instruction) == "" || utf8.RuneCountInString(annotation.Instruction) > 4000 {
		return invalid("invalid user instruction")
	}
	return &ref, nil
}

// BindEditReference replaces an inherited binding at every Run boundary, even
// for an ordinary prompt. No global state, files or environment are involved.
func BindEditReference(ctx context.Context, owner, prompt string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ref, err := ParseEditReference(prompt)
	if err != nil {
		return ctx, err
	}
	edit := selectedEdit{}
	if ref != nil {
		if err := validateSessionID(owner); err != nil {
			return ctx, err
		}
		edit = selectedEdit{owner: owner, reference: *ref, bound: true}
	}
	return context.WithValue(ctx, editContextKey{}, edit), nil
}

// SelectedEditVersion returns the fixed base only for the selected artifact
// and originating owner. The same request cannot append a second revision:
// its original base will no longer be current after a successful CAS update.
func SelectedEditVersion(ctx context.Context, owner, id string) (version int, matched bool, err error) {
	if ctx == nil {
		return 0, false, nil
	}
	edit, ok := ctx.Value(editContextKey{}).(selectedEdit)
	if !ok || !edit.bound || edit.reference.ArtifactID != id {
		return 0, false, nil
	}
	if owner != edit.owner {
		return 0, true, ErrOwnerMismatch
	}
	return edit.reference.Version, true, nil
}

// JSON struct decoding permits duplicate and case-insensitive field names.
// Reject both before decoding the canonical schema so its base is unambiguous.
func validateEditJSONFields(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalidEditReference
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delim != '{' {
		return ErrInvalidEditReference
	}
	seen := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return ErrInvalidEditReference
		}
		switch key {
		case "metis_artifact_annotation", "artifactId", "version", "digest", "targetId", "instruction", "title", "selection", "id", "tag", "selector", "text":
		default:
			return ErrInvalidEditReference
		}
		seen[key] = true
		if err := validateEditJSONFields(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
