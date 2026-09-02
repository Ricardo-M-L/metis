package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEnforcer(t *testing.T, schema string) *OutputSchemaEnforcer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := NewOutputSchemaEnforcer(path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const personSchema = `{
  "type": "object",
  "required": ["name", "age"],
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "level": {"enum": ["junior", "senior"]}
  }
}`

func TestValidateAcceptsConformingOutput(t *testing.T) {
	e := newEnforcer(t, personSchema)
	got, err := e.Validate(`{"name": "ricardo", "age": 30, "tags": ["go"], "level": "senior"}`)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !strings.Contains(got, `"ricardo"`) {
		t.Errorf("validated JSON lost content: %s", got)
	}
}

func TestValidateRejections(t *testing.T) {
	e := newEnforcer(t, personSchema)
	cases := []struct {
		name, text, wantErr string
	}{
		{"missing required", `{"name": "x"}`, "missing required property"},
		{"wrong type", `{"name": "x", "age": "thirty"}`, "expected integer"},
		{"non-integer number", `{"name": "x", "age": 3.5}`, "expected integer"},
		{"bad enum", `{"name": "x", "age": 1, "level": "boss"}`, "not in enum"},
		{"bad array item", `{"name": "x", "age": 1, "tags": [1]}`, "expected string"},
		{"prose only", `I could not produce JSON, sorry.`, "JSON"},
	}
	for _, c := range cases {
		if _, err := e.Validate(c.text); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
		}
	}
}

// Model output rarely arrives clean — fences and prose around the
// JSON must not fail extraction.
func TestValidateExtractsFromNoise(t *testing.T) {
	e := newEnforcer(t, personSchema)
	noisy := "Here is the result:\n```json\n{\"name\": \"a\", \"age\": 1}\n```\nLet me know!"
	if _, err := e.Validate(noisy); err != nil {
		t.Errorf("fenced JSON should validate, got %v", err)
	}
	prose := `The answer is {"name": "b", "age": 2} as requested.`
	if _, err := e.Validate(prose); err != nil {
		t.Errorf("inline JSON should validate, got %v", err)
	}
	nested := `{"name": "c {with brace}", "age": 3}`
	if _, err := e.Validate(nested); err != nil {
		t.Errorf("braces inside strings must not break extraction, got %v", err)
	}
}

func TestAdditionalPropertiesFalse(t *testing.T) {
	e := newEnforcer(t, `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`)
	if _, err := e.Validate(`{"a":"x","b":"y"}`); err == nil || !strings.Contains(err.Error(), "unexpected property") {
		t.Errorf("additionalProperties=false must reject extras, got %v", err)
	}
	if _, err := e.Validate(`{"a":"x"}`); err != nil {
		t.Errorf("conforming object rejected: %v", err)
	}
}

func TestNewEnforcerEmptyPathIsOff(t *testing.T) {
	e, err := NewOutputSchemaEnforcer("")
	if e != nil || err != nil {
		t.Fatalf("empty path must be (nil, nil); got (%v, %v)", e, err)
	}
}

func TestInstructionCarriesSchema(t *testing.T) {
	e := newEnforcer(t, personSchema)
	ins := e.Instruction()
	if !strings.Contains(ins, "system-reminder") || !strings.Contains(ins, `"required"`) {
		t.Errorf("instruction missing schema body: %s", ins)
	}
}

func TestOutputSchemaEnforcerExposesNativeResponseFormat(t *testing.T) {
	e := newEnforcer(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	format := e.ResponseFormat()
	if format == nil || format.Name != "metis_output" || !format.Strict {
		t.Fatalf("format = %#v", format)
	}
	if format.JSONSchema["type"] != "object" {
		t.Fatalf("schema = %#v", format.JSONSchema)
	}
}
