package tool

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// ErrInputNormalization is the fixed public error returned when a tool's
// custom normalizer rejects an invocation. A normalizer is third-party code
// and its error text may contain rejected arguments or credentials, so the
// original error must not cross into model-visible tool results or logs.
var ErrInputNormalization = errors.New("tool input normalization failed")

// InputNormalizer is an optional tool capability for canonicalizing a small,
// explicitly documented set of backwards-compatible input aliases before
// schema validation and permission checks. Implementations must be pure: they
// return a new top-level map and must not mutate input.
//
// Normalization is deliberately separate from validation. It is only for
// lossless spelling migrations (for example file_path -> path), never for
// coercing types or inventing missing business values.
type InputNormalizer interface {
	NormalizeInput(input map[string]any) (map[string]any, error)
}

// NormalizeToolInput invokes a tool's optional InputNormalizer without
// exposing the caller's map to mutation. Tools without the capability still
// receive a fresh top-level copy so downstream code can treat the returned map
// as invocation-owned.
func NormalizeToolInput(t Tool, input map[string]any) (map[string]any, error) {
	owned, err := cloneInput(input)
	if err != nil {
		return nil, err
	}
	normalizer, ok := t.(InputNormalizer)
	if !ok {
		return owned, nil
	}
	normalized, err := callInputNormalizer(normalizer, owned)
	if err != nil {
		return nil, ErrInputNormalization
	}
	return cloneInput(normalized)
}

// callInputNormalizer keeps third-party normalizer panics inside the same
// provider-neutral error boundary as ordinary normalization failures. Tool
// execution has its own panic recovery, but normalization intentionally runs
// earlier (before hooks, permission checks, and schema validation), so it
// needs an equivalent guard here.
func callInputNormalizer(normalizer InputNormalizer, input map[string]any) (normalized map[string]any, err error) {
	defer func() {
		if recover() != nil {
			normalized = nil
			err = ErrInputNormalization
		}
	}()
	return normalizer.NormalizeInput(input)
}

// NormalizeAliases returns a top-level copy of input with each legacy alias
// moved to its canonical key. When both spellings are present, equal values are
// accepted and the alias is removed; unequal values are rejected rather than
// silently choosing one. Values are intentionally compared without coercion.
func NormalizeAliases(input map[string]any, aliases map[string]string) (map[string]any, error) {
	normalized, err := cloneInput(input)
	if err != nil {
		return nil, err
	}
	legacyKeys := make([]string, 0, len(aliases))
	for legacy := range aliases {
		legacyKeys = append(legacyKeys, legacy)
	}
	sort.Strings(legacyKeys)

	for _, legacy := range legacyKeys {
		canonical := aliases[legacy]
		if legacy == "" || canonical == "" || legacy == canonical {
			continue
		}
		legacyValue, hasLegacy := normalized[legacy]
		if !hasLegacy {
			continue
		}
		canonicalValue, hasCanonical := normalized[canonical]
		if hasCanonical && !reflect.DeepEqual(canonicalValue, legacyValue) {
			return nil, fmt.Errorf("tool input has conflicting %q and legacy alias %q", canonical, legacy)
		}
		if !hasCanonical {
			normalized[canonical] = legacyValue
		}
		delete(normalized, legacy)
	}
	return normalized, nil
}

const maxInputCloneDepth = 128

func cloneInput(input map[string]any) (map[string]any, error) {
	cloned, err := cloneInputValue(input, 0)
	if err != nil {
		return nil, err
	}
	if cloned == nil {
		return map[string]any{}, nil
	}
	return cloned.(map[string]any), nil
}

// cloneInputValue recursively detaches the JSON graph a normalizer receives.
// Tool input normally comes from encoding/json (map[string]any / []any), with
// []string and map[string]string also common in direct tests and embedders.
// A depth cap turns cyclic or pathological direct-call values into a clean
// normalization error rather than exposing the caller's nested objects.
func cloneInputValue(value any, depth int) (any, error) {
	if depth > maxInputCloneDepth {
		return nil, fmt.Errorf("tool input nesting exceeds %d levels or contains a cycle", maxInputCloneDepth)
	}
	switch value := value.(type) {
	case nil, string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return value, nil
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, child := range value {
			copy, err := cloneInputValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[key] = copy
		}
		return cloned, nil
	case []any:
		cloned := make([]any, len(value))
		for i, child := range value {
			copy, err := cloneInputValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[i] = copy
		}
		return cloned, nil
	case map[string]string:
		cloned := make(map[string]string, len(value))
		for key, child := range value {
			cloned[key] = child
		}
		return cloned, nil
	case []string:
		return append([]string(nil), value...), nil
	default:
		// Preserve unusual scalar aliases for the validator, which will decide
		// whether they are JSON-compatible. They contain no mutable JSON graph a
		// normalizer can traverse through the ordinary type assertions above.
		return value, nil
	}
}
