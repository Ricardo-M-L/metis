package agent

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

const invalidToolArgsCode = pubtool.ValidationCodeInputInvalid
const invalidToolJSONCode = "INVALID_JSON"

// invalidToolArgumentsBlock creates the provider-neutral failure returned
// when a model (or a PreToolUse hook) violates a tool's JSON Schema. The text
// is deliberately concise and contains no rejected value, so credentials in a
// malformed input cannot leak back into model context or UI logs.
func invalidToolArgumentsBlock(toolName, toolUseID string, verr *pubtool.ValidationError) llm.ContentBlock {
	if verr == nil {
		verr = &pubtool.ValidationError{
			Code:    pubtool.ValidationCodeInputInvalid,
			Path:    "$",
			Keyword: "validation",
			Message: "tool input does not satisfy the schema",
		}
	}
	if verr.Keyword == "normalization" {
		return llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  toolUseID,
			ToolResult: fmt.Sprintf("%s: tool %q input normalization failed. Rebuild the arguments and call the tool again; do not repeat the unchanged arguments.", invalidToolArgsCode, toolName),
			IsError:    true,
			Presentation: map[string]any{
				"kind":      "tool_error",
				"code":      invalidToolArgsCode,
				"path":      "$",
				"keyword":   "normalization",
				"retryable": true,
				"action":    "rebuild_arguments",
			},
		}
	}
	code := invalidToolArgsCode
	retryable := true
	action := "correct_arguments"
	message := fmt.Sprintf(
		"%s: tool %q input failed JSON Schema validation: %s. Correct the named field and call the tool again; do not repeat the unchanged arguments.",
		code, toolName, verr,
	)
	if verr != nil && verr.Code == pubtool.ValidationCodeSchemaInvalid {
		code = pubtool.ValidationCodeSchemaInvalid
		retryable = false
		action = "report_tool_schema"
		message = fmt.Sprintf(
			"%s: tool %q is unavailable because its published JSON Schema is invalid: %s. Do not retry this tool with different arguments; use an alternative tool or report the schema defect.",
			code, toolName, verr,
		)
	}
	return llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  toolUseID,
		ToolResult: message,
		IsError:    true,
		Presentation: map[string]any{
			"kind":      "tool_error",
			"code":      code,
			"path":      verr.Path,
			"keyword":   verr.Keyword,
			"retryable": retryable,
			"action":    action,
		},
	}
}

func normalizationValidationError() *pubtool.ValidationError {
	return &pubtool.ValidationError{
		Code:    pubtool.ValidationCodeInputInvalid,
		Path:    "$",
		Keyword: "normalization",
		Message: pubtool.ErrInputNormalization.Error(),
	}
}

// malformedToolJSONBlock reports the provider/parser sentinel used when a
// function-call arguments string is not valid JSON. Never include the raw
// bytes in either model text or Presentation: malformed arguments can still
// contain credentials and other secrets.
func malformedToolJSONBlock(toolName, toolUseID string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  toolUseID,
		ToolResult: fmt.Sprintf("%s: tool %q arguments are not a complete JSON object. Rebuild valid JSON arguments and call the tool again.", invalidToolJSONCode, toolName),
		IsError:    true,
		Presentation: map[string]any{
			"kind":      "tool_error",
			"code":      invalidToolJSONCode,
			"path":      "$",
			"keyword":   "json",
			"retryable": true,
			"action":    "rebuild_arguments",
		},
	}
}

func hasMalformedToolJSON(block llm.ContentBlock) bool {
	return hasMalformedToolJSONForSchema(block, nil)
}

// hasMalformedToolJSONForSchema distinguishes the authoritative in-memory
// parse marker from the legacy sole {_raw: string} sentinel. The latter is
// only a heuristic, so a tool that explicitly declares a top-level _raw
// property must be allowed to validate and execute it normally.
func hasMalformedToolJSONForSchema(block llm.ContentBlock, schema map[string]any) bool {
	if block.ToolInputMalformed {
		return true
	}
	if !hasLegacyMalformedToolJSON(block.ToolInput) {
		return false
	}
	return !schemaDeclaresTopLevelProperty(schema, "_raw")
}

func schemaDeclaresTopLevelProperty(schema map[string]any, name string) bool {
	return schemaNodeDeclaresTopLevelProperty(schema, schema, name, make(map[string]bool), 0)
}

// schemaNodeDeclaresTopLevelProperty follows the schema constructs that
// unconditionally contribute assertions to the same instance object. This is
// deliberately more conservative than a general JSON Schema walker: a nested
// properties block must not make a legacy malformed {_raw: ...} payload look
// legitimate, and an anyOf branch that does not declare _raw could otherwise
// accept the sentinel through a different alternative.
func schemaNodeDeclaresTopLevelProperty(root, node any, name string, activeRefs map[string]bool, depth int) bool {
	if depth > 64 {
		return false
	}

	if refValue, present := schemaMapLookup(node, "$ref"); present {
		ref, _ := refValue.(string)
		if strings.HasPrefix(ref, "#") && !activeRefs[ref] {
			if target, ok := resolveLocalSchemaPointer(root, ref); ok {
				activeRefs[ref] = true
				declared := schemaNodeDeclaresTopLevelProperty(root, target, name, activeRefs, depth+1)
				delete(activeRefs, ref)
				if declared {
					return true
				}
			}
		}
	}

	if properties, present := schemaMapLookup(node, "properties"); present && schemaMapHasKey(properties, name) {
		return true
	}

	// Every allOf member applies to the root instance, so one explicit
	// declaration is sufficient; ValidateInput will still enforce all members.
	if allOf, present := schemaMapLookup(node, "allOf"); present {
		for _, child := range schemaSliceValues(allOf) {
			if schemaNodeDeclaresTopLevelProperty(root, child, name, activeRefs, depth+1) {
				return true
			}
		}
	}

	// For alternatives, require every branch to explicitly declare the field.
	// Otherwise a permissive sibling branch could accept a legacy malformed
	// sentinel even though the declaring branch was not the one that matched.
	for _, keyword := range []string{"anyOf", "oneOf"} {
		alternativesValue, present := schemaMapLookup(node, keyword)
		if !present {
			continue
		}
		alternatives := schemaSliceValues(alternativesValue)
		if len(alternatives) == 0 {
			continue
		}
		allDeclare := true
		for _, child := range alternatives {
			if !schemaNodeDeclaresTopLevelProperty(root, child, name, activeRefs, depth+1) {
				allDeclare = false
				break
			}
		}
		if allDeclare {
			return true
		}
	}
	return false
}

func schemaMapHasKey(object any, name string) bool {
	_, present := schemaMapLookup(object, name)
	return present
}

func schemaMapLookup(object any, name string) (any, bool) {
	value := reflect.ValueOf(object)
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return nil, false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Map || value.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	key := reflect.ValueOf(name)
	if key.Type() != value.Type().Key() {
		key = key.Convert(value.Type().Key())
	}
	result := value.MapIndex(key)
	if !result.IsValid() {
		return nil, false
	}
	return result.Interface(), true
}

func schemaSliceValues(array any) []any {
	value := reflect.ValueOf(array)
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if !value.IsValid() || (value.Kind() != reflect.Slice && value.Kind() != reflect.Array) {
		return nil
	}
	items := make([]any, value.Len())
	for i := 0; i < value.Len(); i++ {
		items[i] = value.Index(i).Interface()
	}
	return items
}

func resolveLocalSchemaPointer(root any, ref string) (any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	fragment, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
	if err != nil || !strings.HasPrefix(fragment, "/") {
		return nil, false
	}
	current := root
	for _, encoded := range strings.Split(strings.TrimPrefix(fragment, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		if child, ok := schemaMapLookup(current, segment); ok {
			current = child
			continue
		}
		value := reflect.ValueOf(current)
		for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
			if value.IsNil() {
				return nil, false
			}
			value = value.Elem()
		}
		if !value.IsValid() || (value.Kind() != reflect.Slice && value.Kind() != reflect.Array) {
			return nil, false
		}
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= value.Len() {
			return nil, false
		}
		current = value.Index(index).Interface()
	}
	return current, true
}

// hasLegacyMalformedToolJSON keeps already-loaded in-memory transcripts from
// pre-fix providers safe. New code never stores raw bytes in ToolInput.
func hasLegacyMalformedToolJSON(input map[string]any) bool {
	if len(input) != 1 {
		return false
	}
	_, exists := input["_raw"].(string)
	return exists
}
