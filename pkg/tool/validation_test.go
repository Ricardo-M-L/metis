package tool

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestValidateInputReportsPreciseSchemaFailures(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []string{"path", "content"},
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"content": map[string]any{
				"type": "string",
				"enum": []string{"alpha", "beta"},
			},
			"options": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"retries": map[string]any{"type": "integer"},
				},
			},
		},
		"additionalProperties": false,
	}

	tests := []struct {
		name    string
		input   map[string]any
		path    string
		keyword string
		text    string
	}{
		{
			name:    "missing required",
			input:   map[string]any{"path": "/tmp/x"},
			path:    "$.content",
			keyword: "required",
			text:    "missing required property",
		},
		{
			name:    "wrong type",
			input:   map[string]any{"path": 12.0, "content": "alpha"},
			path:    "$.path",
			keyword: "type",
			text:    "expected string",
		},
		{
			name:    "enum",
			input:   map[string]any{"path": "/tmp/x", "content": "gamma"},
			path:    "$.content",
			keyword: "enum",
			text:    "allowed values",
		},
		{
			name:    "nested integer",
			input:   map[string]any{"path": "/tmp/x", "content": "alpha", "options": map[string]any{"retries": 1.5}},
			path:    "$.options.retries",
			keyword: "type",
			text:    "expected integer",
		},
		{
			name:    "unexpected property",
			input:   map[string]any{"path": "/tmp/x", "content": "alpha", "extra": true},
			path:    "$",
			keyword: "additionalProperties",
			text:    "unexpected property",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInput(schema, tc.input)
			if err == nil {
				t.Fatal("ValidateInput returned nil")
			}
			if err.Path != tc.path || err.Keyword != tc.keyword {
				t.Fatalf("error = %#v, want path=%q keyword=%q", err, tc.path, tc.keyword)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error %q missing %q", err, tc.text)
			}
		})
	}
}

func TestValidateInputSupportsArraysAndComposition(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":     "array",
				"minItems": 1.0,
				"items": map[string]any{
					"anyOf": []any{
						map[string]any{"type": "string"},
						map[string]any{"type": "integer"},
					},
				},
			},
		},
	}

	if err := ValidateInput(schema, map[string]any{"items": []any{"ok", 2.0}}); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if err := ValidateInput(schema, map[string]any{"items": []any{true}}); err == nil || err.Path != "$.items[0]" || err.Keyword != "anyOf" {
		t.Fatalf("composition error = %#v", err)
	}
	if err := ValidateInput(schema, map[string]any{"items": []any{}}); err == nil || err.Path != "$.items" || err.Keyword != "minItems" {
		t.Fatalf("minItems error = %#v", err)
	}
}

func TestValidateInputResolvesLocalDefinitions(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"options": map[string]any{
				"type":     "object",
				"required": []string{"retries"},
				"properties": map[string]any{
					"retries": map[string]any{"type": "integer", "minimum": 1},
				},
			},
		},
		"type": "object",
		"properties": map[string]any{
			"options": map[string]any{"$ref": "#/$defs/options"},
		},
	}

	if err := ValidateInput(schema, map[string]any{"options": map[string]any{"retries": 2}}); err != nil {
		t.Fatalf("valid local $ref input rejected: %v", err)
	}
	err := ValidateInput(schema, map[string]any{"options": map[string]any{"retries": 0}})
	if err == nil || err.Code != ValidationCodeInputInvalid || err.Path != "$.options.retries" || err.Keyword != "minimum" {
		t.Fatalf("local $ref validation error = %#v", err)
	}
}

func TestValidateInputFailsClosedForInvalidSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
	}{
		{
			name: "malformed pattern",
			schema: map[string]any{
				"type":       "string",
				"pattern":    "(",
				"properties": map[string]any{},
			},
		},
		{
			name: "invalid required keyword",
			schema: map[string]any{
				"type":     "object",
				"required": "path",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInput(tc.schema, map[string]any{})
			if err == nil {
				t.Fatal("invalid schema was accepted")
			}
			if err.Code != ValidationCodeSchemaInvalid {
				t.Fatalf("error code = %q, want %q: %v", err.Code, ValidationCodeSchemaInvalid, err)
			}
			if err.Path != "$" {
				t.Fatalf("schema error path = %q, want root", err.Path)
			}
		})
	}
}

func TestValidateInputRejectsExternalReferencesWithoutLoadingThem(t *testing.T) {
	localSchema := filepath.Join(t.TempDir(), "external-schema.json")
	if err := os.WriteFile(localSchema, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"https://example.invalid/tool-schema.json",
		"file://" + filepath.ToSlash(localSchema),
		"other.json#/$defs/value",
		"../other.json#/$defs/value",
		"//example.invalid/tool-schema.json",
	} {
		t.Run(ref, func(t *testing.T) {
			err := ValidateInput(map[string]any{"$ref": ref}, map[string]any{})
			if err == nil || err.Code != ValidationCodeSchemaInvalid {
				t.Fatalf("external ref error = %#v, want schema-invalid", err)
			}
			if !strings.Contains(strings.ToLower(err.Message), "external") {
				t.Fatalf("external ref error is not actionable: %v", err)
			}
		})
	}
}

func TestValidateInputFailsClosedForMissingSchema(t *testing.T) {
	err := ValidateInput(nil, map[string]any{"anything": "would otherwise pass"})
	if err == nil || err.Code != ValidationCodeSchemaInvalid {
		t.Fatalf("nil schema error = %#v, want schema-invalid", err)
	}
}

func TestValidateInputDoesNotTreatRefShapedAnnotationDataAsSchemaReference(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"examples": []any{
			map[string]any{"$ref": "https://example.invalid/this-is-example-data"},
		},
	}
	if err := ValidateInput(schema, map[string]any{}); err != nil {
		t.Fatalf("annotation data was mistaken for a schema reference: %v", err)
	}
}

func TestExternalReferenceScanHonorsExplicitDraftAnnotations(t *testing.T) {
	schema := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
		// prefixItems is only a schema keyword in 2020-12. Draft-07 treats it
		// as annotation data, so its ref-shaped content must remain ignored.
		"prefixItems": []any{
			map[string]any{"$ref": "https://example.invalid/annotation-data"},
		},
	}
	if err := ValidateInput(schema, map[string]any{}); err != nil {
		t.Fatalf("draft-07 annotation was scanned as 2020-12 schema: %v", err)
	}
}

func TestValidateInputNormalizesJSONAndDoesNotLeakValues(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count":  map[string]any{"type": "integer"},
			"secret": map[string]any{"enum": []string{"allowed"}},
		},
	}

	if err := ValidateInput(schema, map[string]any{"count": int64(3), "secret": "allowed"}); err != nil {
		t.Fatalf("JSON-compatible Go values rejected: %v", err)
	}
	const secret = "super-secret-input-value"
	err := ValidateInput(schema, map[string]any{"count": 3, "secret": secret})
	if err == nil || err.Code != ValidationCodeInputInvalid {
		t.Fatalf("enum error = %#v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked input value: %v", err)
	}

	err = ValidateInput(schema, map[string]any{"count": math.NaN(), "secret": "allowed"})
	if err == nil || err.Code != ValidationCodeInputInvalid || err.Keyword != "json" {
		t.Fatalf("non-JSON input error = %#v", err)
	}
}

func TestValidateInputDoesNotLeakUnexpectedPropertyNames(t *testing.T) {
	const secretKey = "sk-secret-as-an-object-key-123456789"
	err := ValidateInput(
		map[string]any{"type": "object", "additionalProperties": false},
		map[string]any{secretKey: true},
	)
	if err == nil || err.Keyword != "additionalProperties" {
		t.Fatalf("additionalProperties error = %#v", err)
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Fatalf("validation error leaked unexpected property name: %v", err)
	}
}

func TestValidateInputDoesNotLeakDynamicPropertyNames(t *testing.T) {
	const secretKey = "sk-secret-dynamic-key-123456789"
	for name, schema := range map[string]map[string]any{
		"patternProperties": {
			"type": "object",
			"patternProperties": map[string]any{
				".*": map[string]any{"type": "integer"},
			},
		},
		"additionalProperties schema": {
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "integer"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateInput(schema, map[string]any{secretKey: false})
			if err == nil {
				t.Fatal("invalid dynamic property passed")
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Fatalf("validation error leaked dynamic property name: %v", err)
			}
		})
	}
}

func TestValidateInputAssertsKnownFormats(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"email": map[string]any{"type": "string", "format": "email"},
		},
	}
	err := ValidateInput(schema, map[string]any{"email": "not-an-email"})
	if err == nil || err.Path != "$.email" || err.Keyword != "format" {
		t.Fatalf("format error = %#v", err)
	}
}

func TestValidationPathDistinguishesNumericObjectKeysFromArrayIndexes(t *testing.T) {
	objectSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"0": map[string]any{"type": "string"},
		},
	}
	err := ValidateInput(objectSchema, map[string]any{"0": false})
	if err == nil || err.Path != `$["0"]` {
		t.Fatalf("numeric object property path = %#v, want $[\"0\"]", err)
	}

	arraySchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}
	err = ValidateInput(arraySchema, map[string]any{"items": []any{false}})
	if err == nil || err.Path != "$.items[0]" {
		t.Fatalf("array item path = %#v, want $.items[0]", err)
	}
}

func TestValidateInputPrefersPreciseErrorOverRootCompositionWrapper(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "integer"},
		},
		"anyOf": []any{
			map[string]any{"required": []string{"alpha"}},
			map[string]any{"required": []string{"beta"}},
		},
	}
	err := ValidateInput(schema, map[string]any{"x": "wrong"})
	if err == nil || err.Path != "$.x" || err.Keyword != "type" {
		t.Fatalf("precise independent error was hidden by root anyOf: %#v", err)
	}
}

func TestValidateInputOrdersArrayErrorsByNumericIndex(t *testing.T) {
	items := make([]any, 11)
	for i := range items {
		items[i] = "ok"
	}
	items[2] = false
	items[10] = false
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}
	err := ValidateInput(schema, map[string]any{"items": items})
	if err == nil || err.Path != "$.items[2]" {
		t.Fatalf("array validation chose non-numeric order: %#v", err)
	}
}

func TestValidationPathUsesJSONEscaping(t *testing.T) {
	const property = "bell\a"
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			property: map[string]any{"type": "string"},
		},
	}
	err := ValidateInput(schema, map[string]any{property: false})
	if err == nil || err.Path != `$["bell\u0007"]` {
		t.Fatalf("control-character path = %#v", err)
	}
}

func TestValidateInputTieBreakIsDeterministic(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"patternProperties": map[string]any{
			"^x": map[string]any{"type": "string"},
			"x$": map[string]any{"type": "integer"},
		},
	}
	const runs = 1000
	var first string
	for i := 0; i < runs; i++ {
		err := ValidateInput(schema, map[string]any{"x": false})
		if err == nil {
			t.Fatal("invalid overlapping patternProperties input passed")
		}
		if i == 0 {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("nondeterministic error: first=%q current=%q", first, err)
		}
	}
}

func TestValidateInputRejectsOversizedSchemaAndBoundsCache(t *testing.T) {
	oversized := map[string]any{
		"type":        "object",
		"description": strings.Repeat("x", maxToolSchemaBytes),
	}
	err := ValidateInput(oversized, map[string]any{})
	if err == nil || err.Code != ValidationCodeSchemaInvalid || err.Keyword != "maxSchemaSize" {
		t.Fatalf("oversized schema error = %#v", err)
	}

	for i := 0; i < maxCompiledSchemaCacheEntries+10; i++ {
		schema := map[string]any{
			"type":        "object",
			"description": fmt.Sprintf("bounded-cache-%d", i),
		}
		if err := ValidateInput(schema, map[string]any{}); err != nil {
			t.Fatalf("cache seed %d failed: %v", i, err)
		}
	}
	compiledSchemas.RLock()
	cacheLen := len(compiledSchemas.entries)
	compiledSchemas.RUnlock()
	if cacheLen > maxCompiledSchemaCacheEntries {
		t.Fatalf("compiled schema cache has %d entries, max %d", cacheLen, maxCompiledSchemaCacheEntries)
	}
}

func TestSchemaCacheKeyIsStableAndValidationIsConcurrentSafe(t *testing.T) {
	schemaA := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
	}
	schemaB := map[string]any{}
	schemaB["properties"] = map[string]any{
		"age":  map[string]any{"type": "integer"},
		"name": map[string]any{"type": "string"},
	}
	schemaB["type"] = "object"

	keyA, err := schemaCacheKey(schemaA)
	if err != nil {
		t.Fatalf("schemaCacheKey(A): %v", err)
	}
	keyB, err := schemaCacheKey(schemaB)
	if err != nil {
		t.Fatalf("schemaCacheKey(B): %v", err)
	}
	if keyA != keyB {
		t.Fatalf("equivalent schemas have different cache keys: %q != %q", keyA, keyB)
	}

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan *ValidationError, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ValidateInput(schemaA, map[string]any{"name": "metis", "age": 1})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent valid input rejected: %v", err)
		}
	}
}

func TestValidateInputSnapshotsMutableSchemaByContent(t *testing.T) {
	countSchema := map[string]any{"type": "string"}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": countSchema,
		},
	}

	if err := ValidateInput(schema, map[string]any{"count": "three"}); err != nil {
		t.Fatalf("initial schema rejected valid input: %v", err)
	}
	countSchema["type"] = "integer"

	err := ValidateInput(schema, map[string]any{"count": "three"})
	if err == nil || err.Code != ValidationCodeInputInvalid || err.Path != "$.count" || err.Keyword != "type" {
		t.Fatalf("mutated schema was not recompiled by content: %#v", err)
	}
	if err := ValidateInput(schema, map[string]any{"count": 3}); err != nil {
		t.Fatalf("mutated schema rejected valid integer: %v", err)
	}
}
