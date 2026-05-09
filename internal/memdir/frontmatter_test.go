package memdir

import (
	"strings"
	"testing"
)

func TestParseFile_HappyPath(t *testing.T) {
	raw := []byte(`---
name: user_role
description: Backend lead at goalfy
type: user
originSessionId: abc-123
---

Body line one.
Body line two.
`)
	fm, body, err := ParseFile(raw)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "user_role" {
		t.Errorf("Name = %q", fm.Name)
	}
	if fm.Type != TypeUser {
		t.Errorf("Type = %q", fm.Type)
	}
	if fm.OriginSessionID != "abc-123" {
		t.Errorf("OriginSessionID = %q", fm.OriginSessionID)
	}
	if !strings.Contains(string(body), "Body line one") {
		t.Errorf("body missing line one: %q", body)
	}
}

func TestParseFile_NoFrontmatter(t *testing.T) {
	raw := []byte("Just body, no frontmatter.\n")
	fm, body, err := ParseFile(raw)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "" {
		t.Errorf("Name should be empty, got %q", fm.Name)
	}
	if !strings.Contains(string(body), "Just body") {
		t.Errorf("body wrong: %q", body)
	}
}

func TestParseFile_BadYaml(t *testing.T) {
	raw := []byte(`---
name: [unclosed
type: user
---

body
`)
	_, _, err := ParseFile(raw)
	if err == nil {
		t.Fatalf("expected YAML error")
	}
}

func TestFrontmatter_Validate(t *testing.T) {
	tests := []struct {
		name    string
		fm      Frontmatter
		wantErr bool
	}{
		{"complete", Frontmatter{Name: "x", Description: "y", Type: TypeUser}, false},
		{"missing name", Frontmatter{Description: "y", Type: TypeUser}, true},
		{"missing description", Frontmatter{Name: "x", Type: TypeUser}, true},
		{"missing type", Frontmatter{Name: "x", Description: "y"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fm.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMemoryType_IsValid(t *testing.T) {
	for _, ok := range []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference} {
		if !ok.IsValid() {
			t.Errorf("%q should be valid", ok)
		}
	}
	if MemoryType("misc").IsValid() {
		t.Errorf("misc should not be valid")
	}
}

func TestRenderFile_RoundTrip(t *testing.T) {
	fm := &Frontmatter{
		Name:        "feedback_chinese",
		Description: "Reply in Chinese",
		Type:        TypeFeedback,
	}
	raw, err := RenderFile(fm, "Body content here.")
	if err != nil {
		t.Fatalf("RenderFile: %v", err)
	}
	got, body, err := ParseFile(raw)
	if err != nil {
		t.Fatalf("ParseFile after RenderFile: %v", err)
	}
	if got.Name != fm.Name || got.Description != fm.Description || got.Type != fm.Type {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, fm)
	}
	if !strings.Contains(string(body), "Body content here") {
		t.Fatalf("body missing: %q", body)
	}
}

func TestRenderFile_RejectsInvalid(t *testing.T) {
	fm := &Frontmatter{Name: "x"} // missing desc + type
	if _, err := RenderFile(fm, "body"); err == nil {
		t.Fatalf("RenderFile should reject incomplete frontmatter")
	}
}
