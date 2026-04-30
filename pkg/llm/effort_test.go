package llm

import "testing"

func TestEffort_Valid(t *testing.T) {
	cases := map[Effort]bool{
		EffortDefault: true,
		EffortLow:     true,
		EffortMedium:  true,
		EffortHigh:    true,
		Effort("typo"): false,
	}
	for e, want := range cases {
		if got := e.Valid(); got != want {
			t.Errorf("Effort(%q).Valid() = %v, want %v", e, got, want)
		}
	}
}

func TestEffort_BudgetTokens(t *testing.T) {
	cases := map[Effort]int{
		EffortDefault: 0,
		EffortLow:     1024,
		EffortMedium:  4096,
		EffortHigh:    16384,
		Effort("garbage"): 0,
	}
	for e, want := range cases {
		if got := e.BudgetTokens(); got != want {
			t.Errorf("Effort(%q).BudgetTokens() = %d, want %d", e, got, want)
		}
	}
}

func TestEffort_OpenAI(t *testing.T) {
	cases := map[Effort]string{
		EffortDefault: "",
		EffortLow:     "low",
		EffortMedium:  "medium",
		EffortHigh:    "high",
	}
	for e, want := range cases {
		if got := e.OpenAI(); got != want {
			t.Errorf("Effort(%q).OpenAI() = %q, want %q", e, got, want)
		}
	}
}

func TestParseEffort_KnownAliases(t *testing.T) {
	good := map[string]Effort{
		"":       EffortDefault,
		"low":    EffortLow,
		"l":      EffortLow,
		"fast":   EffortLow,
		"medium": EffortMedium,
		"m":      EffortMedium,
		"mid":    EffortMedium,
		"high":   EffortHigh,
		"h":      EffortHigh,
		"deep":   EffortHigh,
		"max":    EffortHigh,
	}
	for in, want := range good {
		got, ok := ParseEffort(in)
		if !ok {
			t.Errorf("ParseEffort(%q) ok=false; want true", in)
		}
		if got != want {
			t.Errorf("ParseEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEffort_Unknown(t *testing.T) {
	_, ok := ParseEffort("ultra-mega-think")
	if ok {
		t.Error("ParseEffort should reject unknown values")
	}
}
