package tool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

const (
	// ValidationCodeInputInvalid means the schema compiled successfully but the
	// proposed tool input did not satisfy it.
	ValidationCodeInputInvalid = "INVALID_TOOL_ARGS"

	// ValidationCodeSchemaInvalid means the tool published an invalid or unsafe
	// schema. Callers must not execute the tool when this code is returned.
	ValidationCodeSchemaInvalid = "TOOL_SCHEMA_INVALID"
)

// ValidationError describes either an invalid tool input or an invalid tool
// schema. Path uses compact JSONPath notation. Keyword identifies the failed
// JSON Schema rule so callers never need to parse Message.
//
// Message intentionally never contains the rejected input value. Tool
// arguments can include credentials or other sensitive data, and validation
// errors are routinely sent back to the model and persisted in transcripts.
type ValidationError struct {
	Code    string
	Path    string
	Keyword string
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	path := e.Path
	if path == "" {
		path = "$"
	}
	if e.Keyword == "" {
		return fmt.Sprintf("%s: %s", path, e.Message)
	}
	return fmt.Sprintf("%s: %s (keyword: %s)", path, e.Message, e.Keyword)
}

type compiledSchemaResult struct {
	schema *jsonschema.Schema
	err    *ValidationError
}

var compiledSchemas = struct {
	sync.RWMutex
	entries map[string]compiledSchemaResult
	order   []string
}{entries: make(map[string]compiledSchemaResult)}

const (
	maxToolSchemaBytes            = 1 << 20
	maxCompiledSchemaCacheEntries = 128
)

// ValidateInput validates input against JSON Schema. Draft 2020-12 is used
// when $schema is absent; an explicit supported draft is honored for MCP
// compatibility. Schemas are compiled from a JSON snapshot and cached by a
// stable content hash. Callers may mutate a schema between calls, but must not
// mutate the same map concurrently with validation. Local references
// (#/$defs/...) are supported. References to another resource are rejected
// before compilation, and the compiler has no external loader, so validation
// can never perform filesystem or network I/O.
//
// Invalid or missing schemas fail closed and return
// ValidationCodeSchemaInvalid. Invalid inputs return
// ValidationCodeInputInvalid. An explicit empty schema map remains the
// unconstrained JSON Schema {}.
func ValidateInput(schema map[string]any, input map[string]any) *ValidationError {
	if schema == nil {
		return schemaInvalid("schema", "tool schema is missing")
	}
	compiled, compileErr := loadCompiledSchema(schema)
	if compileErr != nil {
		return cloneValidationError(compileErr)
	}

	normalizedInput, err := normalizeJSON(input)
	if err != nil {
		return &ValidationError{
			Code:    ValidationCodeInputInvalid,
			Path:    "$",
			Keyword: "json",
			Message: "tool input is not valid JSON",
		}
	}

	if err := compiled.Validate(normalizedInput); err != nil {
		validationErr, ok := err.(*jsonschema.ValidationError)
		if !ok {
			// Validate currently returns *jsonschema.ValidationError for instance
			// failures. Keep this fail-closed fallback in case a future version
			// introduces a different validation error type.
			return &ValidationError{
				Code:    ValidationCodeInputInvalid,
				Path:    "$",
				Keyword: "validation",
				Message: "tool input does not satisfy the schema",
			}
		}
		return convertValidationError(validationErr, normalizedInput, declaredPropertyNames(schema))
	}
	return nil
}

func loadCompiledSchema(raw map[string]any) (*jsonschema.Schema, *ValidationError) {
	if raw == nil {
		return nil, schemaInvalid("schema", "tool schema is missing")
	}
	key, schemaJSON, keyErr := schemaSnapshot(raw)
	if keyErr != nil {
		if _, tooLarge := keyErr.(schemaTooLargeError); tooLarge {
			return nil, schemaInvalid("maxSchemaSize", "tool schema exceeds the validation size limit")
		}
		return nil, schemaInvalid("json", "tool schema is not valid JSON")
	}

	compiledSchemas.RLock()
	result, ok := compiledSchemas.entries[key]
	compiledSchemas.RUnlock()
	if ok {
		return result.schema, cloneValidationError(result.err)
	}

	// Compile outside the global cache lock: an expensive MCP schema miss must
	// not stall unrelated tools whose validators are already warm.
	schemaDoc, decodeErr := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if decodeErr != nil {
		return nil, schemaInvalid("json", "tool schema is not valid JSON")
	}
	dialect := detectSchemaDialect(schemaDoc)
	if keyword, found := findExternalSchemaReference(schemaDoc, dialect); found {
		result.err = schemaInvalid(keyword, "external schema references are disabled")
		return storeCompiledSchema(key, result)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	loader := &blockedExternalLoader{}
	compiler.UseLoader(loader)
	// Built-in draft metaschemas are embedded by jsonschema/v6. Every other
	// resource reaches blockedExternalLoader, which records the attempt but
	// performs no filesystem or network I/O.
	resourceURL := "metis://schemas/" + key + "/root.json"
	if err := compiler.AddResource(resourceURL, schemaDoc); err != nil {
		result.err = schemaInvalid("schema", "tool schema could not be registered")
	} else if result.schema, err = compiler.Compile(resourceURL); err != nil {
		if loader.attempted {
			result.err = schemaInvalid("externalReference", "external schema references are disabled")
		} else {
			result.err = schemaInvalid("schema", "tool schema could not be compiled")
		}
	}
	return storeCompiledSchema(key, result)
}

// schemaCacheKey returns a stable content hash. encoding/json sorts string map
// keys, so independently allocated but equivalent schema maps share a cache
// entry. The schema is snapshotted before compilation; later caller mutation
// cannot change an already-compiled entry.
func schemaCacheKey(schema map[string]any) (string, error) {
	key, _, err := schemaSnapshot(schema)
	return key, err
}

type schemaTooLargeError struct{}

func (schemaTooLargeError) Error() string { return "tool schema exceeds size limit" }

func schemaSnapshot(schema map[string]any) (string, []byte, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return "", nil, err
	}
	if len(data) > maxToolSchemaBytes {
		return "", nil, schemaTooLargeError{}
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), data, nil
}

func storeCompiledSchema(key string, candidate compiledSchemaResult) (*jsonschema.Schema, *ValidationError) {
	compiledSchemas.Lock()
	defer compiledSchemas.Unlock()
	if existing, ok := compiledSchemas.entries[key]; ok {
		return existing.schema, cloneValidationError(existing.err)
	}
	if len(compiledSchemas.order) >= maxCompiledSchemaCacheEntries {
		oldest := compiledSchemas.order[0]
		compiledSchemas.order = compiledSchemas.order[1:]
		delete(compiledSchemas.entries, oldest)
	}
	compiledSchemas.entries[key] = candidate
	compiledSchemas.order = append(compiledSchemas.order, key)
	return candidate.schema, cloneValidationError(candidate.err)
}

func normalizeJSON(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

type blockedExternalLoader struct {
	attempted bool
}

type schemaDialect uint8

const (
	dialectUnknown schemaDialect = iota
	dialectDraft4
	dialectDraft6
	dialectDraft7
	dialectDraft2019
	dialectDraft2020
)

func detectSchemaDialect(schema any) schemaDialect {
	object, _ := schema.(map[string]any)
	identifier, declared := object["$schema"].(string)
	if !declared || identifier == "" {
		return dialectDraft2020
	}
	switch {
	case strings.Contains(identifier, "draft-04"):
		return dialectDraft4
	case strings.Contains(identifier, "draft-06"):
		return dialectDraft6
	case strings.Contains(identifier, "draft-07"):
		return dialectDraft7
	case strings.Contains(identifier, "2019-09"):
		return dialectDraft2019
	case strings.Contains(identifier, "2020-12"):
		return dialectDraft2020
	default:
		return dialectUnknown
	}
}

// findExternalSchemaReference walks only locations that the selected draft
// define as subschemas. This rejects relative and absolute references without
// mistaking annotation data such as examples: [{"$ref":"..."}] for a schema.
func findExternalSchemaReference(schema any, dialect schemaDialect) (string, bool) {
	if dialect == dialectUnknown {
		// The compiler will resolve the custom metaschema through the blocked
		// loader before it can compile any reference-bearing vocabulary.
		return "", false
	}
	object, ok := schema.(map[string]any)
	if !ok {
		return "", false
	}
	referenceKeywords := []string{"$ref"}
	if dialect == dialectDraft2019 {
		referenceKeywords = append(referenceKeywords, "$recursiveRef")
	}
	if dialect == dialectDraft2020 {
		referenceKeywords = append(referenceKeywords, "$dynamicRef")
	}
	for _, keyword := range referenceKeywords {
		if ref, ok := object[keyword].(string); ok && ref != "" && !strings.HasPrefix(ref, "#") {
			return keyword, true
		}
	}

	singleSchemas := []string{"not", "items", "additionalProperties"}
	if dialect <= dialectDraft7 {
		singleSchemas = append(singleSchemas, "additionalItems")
	}
	if dialect >= dialectDraft6 {
		singleSchemas = append(singleSchemas, "propertyNames", "contains")
	}
	if dialect >= dialectDraft7 {
		singleSchemas = append(singleSchemas, "if", "then", "else")
	}
	if dialect >= dialectDraft2019 {
		singleSchemas = append(singleSchemas, "unevaluatedProperties", "unevaluatedItems", "contentSchema")
	}
	for _, keyword := range singleSchemas {
		if child, exists := object[keyword]; exists {
			if keywordFound, found := findExternalSchemaReference(child, dialect); found {
				return keywordFound, true
			}
			if children, ok := child.([]any); ok { // draft-04 tuple-form items
				for _, item := range children {
					if keywordFound, found := findExternalSchemaReference(item, dialect); found {
						return keywordFound, true
					}
				}
			}
		}
	}

	arraySchemas := []string{"allOf", "anyOf", "oneOf"}
	if dialect == dialectDraft2020 {
		arraySchemas = append(arraySchemas, "prefixItems")
	}
	for _, keyword := range arraySchemas {
		children, _ := object[keyword].([]any)
		for _, child := range children {
			if keywordFound, found := findExternalSchemaReference(child, dialect); found {
				return keywordFound, true
			}
		}
	}

	mapSchemas := []string{"properties", "patternProperties"}
	if dialect <= dialectDraft7 {
		mapSchemas = append(mapSchemas, "definitions", "dependencies")
	} else {
		mapSchemas = append(mapSchemas, "$defs", "dependentSchemas")
	}
	for _, keyword := range mapSchemas {
		children, _ := object[keyword].(map[string]any)
		keys := make([]string, 0, len(children))
		for name := range children {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if keywordFound, found := findExternalSchemaReference(children[name], dialect); found {
				return keywordFound, true
			}
		}
	}
	return "", false
}

func (l *blockedExternalLoader) Load(string) (any, error) {
	l.attempted = true
	return nil, fmt.Errorf("external schema references are disabled")
}

func schemaInvalid(keyword, message string) *ValidationError {
	return &ValidationError{
		Code:    ValidationCodeSchemaInvalid,
		Path:    "$",
		Keyword: keyword,
		Message: message,
	}
}

func cloneValidationError(err *ValidationError) *ValidationError {
	if err == nil {
		return nil
	}
	clone := *err
	return &clone
}

func convertValidationError(root *jsonschema.ValidationError, instance any, declaredProperties map[string]struct{}) *ValidationError {
	candidates := collectValidationCandidates(root, nil)
	if len(candidates) == 0 {
		return &ValidationError{
			Code:    ValidationCodeInputInvalid,
			Path:    "$",
			Keyword: "validation",
			Message: "tool input does not satisfy the schema",
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftLocation := validationLocation(candidates[i], instance, declaredProperties)
		rightLocation := validationLocation(candidates[j], instance, declaredProperties)
		if order := compareValidationLocations(leftLocation, rightLocation); order != 0 {
			return order < 0
		}
		leftKeyword := validationKeyword(candidates[i])
		rightKeyword := validationKeyword(candidates[j])
		if leftKeyword != rightKeyword {
			return leftKeyword < rightKeyword
		}
		leftMessage := safeValidationMessage(candidates[i], leftKeyword)
		rightMessage := safeValidationMessage(candidates[j], rightKeyword)
		if leftMessage != rightMessage {
			return leftMessage < rightMessage
		}
		return candidates[i].SchemaURL < candidates[j].SchemaURL
	})
	selected := candidates[0]
	keyword := validationKeyword(selected)
	return &ValidationError{
		Code:    ValidationCodeInputInvalid,
		Path:    validationPath(selected, instance, declaredProperties),
		Keyword: keyword,
		Message: safeValidationMessage(selected, keyword),
	}
}

func collectValidationCandidates(node *jsonschema.ValidationError, dst []*jsonschema.ValidationError) []*jsonschema.ValidationError {
	if node == nil {
		return dst
	}
	keyword := validationKeyword(node)
	if keyword != "" && keyword != "$ref" && keyword != "$dynamicRef" && keyword != "$recursiveRef" {
		// Composition errors describe why the whole alternative set failed. Do
		// not replace them with an arbitrary branch's leaf error.
		return append(dst, node)
	}
	if len(node.Causes) == 0 {
		if keyword != "" || isConcreteNilKeywordError(node.ErrorKind) {
			return append(dst, node)
		}
		return dst
	}
	for _, cause := range node.Causes {
		dst = collectValidationCandidates(cause, dst)
	}
	return dst
}

func validationKeyword(err *jsonschema.ValidationError) string {
	if err == nil || err.ErrorKind == nil {
		return ""
	}
	path := err.ErrorKind.KeywordPath()
	if len(path) > 0 {
		return path[0]
	}
	switch err.ErrorKind.(type) {
	case *kind.Not:
		return "not"
	case *kind.FalseSchema:
		return "falseSchema"
	case *kind.RefCycle:
		return "$ref"
	}
	return ""
}

func isConcreteNilKeywordError(errorKind jsonschema.ErrorKind) bool {
	switch errorKind.(type) {
	case *kind.FalseSchema, *kind.Not, *kind.RefCycle, *kind.InvalidJsonValue:
		return true
	default:
		return false
	}
}

func validationPath(err *jsonschema.ValidationError, instance any, declaredProperties map[string]struct{}) string {
	location := validationLocation(err, instance, declaredProperties)
	path := "$"
	for _, part := range location {
		if part.isIndex {
			path = fmt.Sprintf("%s[%d]", path, part.index)
		} else {
			path = appendObjectProperty(path, part.property)
		}
	}
	return path
}

type validationLocationPart struct {
	isIndex  bool
	index    int
	property string
}

func validationLocation(err *jsonschema.ValidationError, instance any, declaredProperties map[string]struct{}) []validationLocationPart {
	if err == nil {
		return nil
	}
	location := make([]validationLocationPart, 0, len(err.InstanceLocation)+1)
	current := instance
	for _, segment := range err.InstanceLocation {
		switch value := current.(type) {
		case []any:
			index, parseErr := strconv.Atoi(segment)
			if parseErr == nil && index >= 0 && index < len(value) {
				location = append(location, validationLocationPart{isIndex: true, index: index})
				current = value[index]
				continue
			}
		case map[string]any:
			location = append(location, validationLocationPart{property: safePathProperty(segment, declaredProperties)})
			current = value[segment]
			continue
		}
		location = append(location, validationLocationPart{property: safePathProperty(segment, declaredProperties)})
		current = nil
	}
	switch failure := err.ErrorKind.(type) {
	case *kind.Required:
		if len(failure.Missing) > 0 {
			missing := append([]string(nil), failure.Missing...)
			sort.Strings(missing)
			location = append(location, validationLocationPart{property: missing[0]})
		}
	}
	return location
}

func safePathProperty(property string, declaredProperties map[string]struct{}) string {
	if _, declared := declaredProperties[property]; declared {
		return property
	}
	return "<redacted-property>"
}

func declaredPropertyNames(schema any) map[string]struct{} {
	names := make(map[string]struct{})
	var visit func(any)
	visit = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				for name := range properties {
					names[name] = struct{}{}
				}
			}
			for _, required := range stringSlice(value["required"]) {
				names[required] = struct{}{}
			}
			for _, child := range value {
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(schema)
	return names
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func compareValidationLocations(left, right []validationLocationPart) int {
	// Prefer a more precise nested error over a root-level composition wrapper.
	if len(left) != len(right) {
		if len(left) > len(right) {
			return -1
		}
		return 1
	}
	for i := range left {
		if left[i].isIndex != right[i].isIndex {
			if !left[i].isIndex {
				return -1
			}
			return 1
		}
		if left[i].isIndex {
			if left[i].index < right[i].index {
				return -1
			}
			if left[i].index > right[i].index {
				return 1
			}
			continue
		}
		if left[i].property < right[i].property {
			return -1
		}
		if left[i].property > right[i].property {
			return 1
		}
	}
	return 0
}

var simplePropertyName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func appendObjectProperty(path, segment string) string {
	if simplePropertyName.MatchString(segment) {
		return path + "." + segment
	}
	quoted, _ := json.Marshal(segment)
	return path + "[" + string(quoted) + "]"
}

func safeValidationMessage(err *jsonschema.ValidationError, keyword string) string {
	switch failure := err.ErrorKind.(type) {
	case *kind.Type:
		return fmt.Sprintf("expected %s, got incompatible value", strings.Join(failure.Want, " or "))
	case *kind.Required:
		if len(failure.Missing) > 0 {
			missing := append([]string(nil), failure.Missing...)
			sort.Strings(missing)
			return fmt.Sprintf("missing required property %q", missing[0])
		}
		return "missing required property"
	case *kind.AdditionalProperties:
		return "unexpected property"
	case *kind.Enum:
		return "value is not one of the allowed values"
	case *kind.Const:
		return "value does not match the required constant"
	}

	messages := map[string]string{
		"anyOf":                 "value does not match any allowed schema",
		"oneOf":                 "value must match exactly one allowed schema",
		"allOf":                 "value does not satisfy all required schemas",
		"not":                   "value matches a disallowed schema",
		"falseSchema":           "value is rejected by the schema",
		"minProperties":         "object has too few properties",
		"maxProperties":         "object has too many properties",
		"minItems":              "array has too few items",
		"maxItems":              "array has too many items",
		"uniqueItems":           "array items must be unique",
		"contains":              "array does not contain a required item",
		"minContains":           "array contains too few matching items",
		"maxContains":           "array contains too many matching items",
		"minLength":             "string is shorter than allowed",
		"maxLength":             "string is longer than allowed",
		"pattern":               "string does not match the required pattern",
		"format":                "value does not match the required format",
		"minimum":               "number is below the allowed minimum",
		"maximum":               "number is above the allowed maximum",
		"exclusiveMinimum":      "number is not above the exclusive minimum",
		"exclusiveMaximum":      "number is not below the exclusive maximum",
		"multipleOf":            "number is not a valid multiple",
		"dependentRequired":     "dependent required properties are missing",
		"dependentSchemas":      "dependent schema validation failed",
		"propertyNames":         "object contains an invalid property name",
		"additionalItems":       "array contains disallowed additional items",
		"unevaluatedItems":      "array contains unevaluated items",
		"unevaluatedProperties": "object contains unevaluated properties",
	}
	if message := messages[keyword]; message != "" {
		return message
	}
	return "tool input does not satisfy the schema"
}
