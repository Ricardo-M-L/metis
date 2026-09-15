package skills

import "testing"

func TestEmbedded_ComputerUseAvailableWithoutRestrictingDynamicTools(t *testing.T) {
	content, err := bundledFS.ReadFile("builtin/computer-use.md")
	if err != nil {
		t.Fatal(err)
	}
	sk, err := parseMarkdown(content, "computer-use")
	if err != nil {
		t.Fatalf("parse Computer Use skill: %v", err)
	}
	if sk.Name != "computer-use" || sk.Description == "" || sk.WhenToUse == "" || sk.Version == "" || sk.Prompt == "" {
		t.Fatalf("incomplete Computer Use skill: %+v", sk)
	}
	// CU tool names are discovered from the managed backend. A static
	// allowed_tools list would exclude available platform-specific tools.
	if len(sk.AllowedTools) != 0 {
		t.Fatalf("Computer Use must use the current dynamic tool surface: %v", sk.AllowedTools)
	}
	layer := bundledLayer()
	all, err := layer.Scan()
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, candidate := range all {
		if candidate.Name == sk.Name {
			matches++
			if candidate.Prompt != sk.Prompt {
				t.Fatal("bundled loader changed Computer Use instructions")
			}
		}
	}
	if matches != 1 {
		t.Fatalf("bundled Computer Use count = %d, want 1", matches)
	}
}
