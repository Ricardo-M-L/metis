package memory

import (
	"strings"
	"testing"
)

func TestRecallRelevanceAndBudget(t *testing.T) {
	for _, tt := range []struct {
		query, body string
		want        bool
	}{
		{"你好", "我的项目使用 Go，喜欢中文", false},
		{"我的猫叫什么", "我的猫叫团子", true},
		{"请给我详细解释一下工程部署过程", "我在上海工作", false},
		{"Nebula", "Nebula uses Quartz", true},
	} {
		if got := RecallRelevant(tt.query, tt.body); got != tt.want {
			t.Errorf("query=%q got=%v", tt.query, got)
		}
	}
	huge := Passage{ID: "huge", Content: strings.Repeat("数据库", 2000)}
	small := Passage{ID: "small", Content: "Nebula uses Copper."}
	got := BudgetRecall([]Passage{huge, small}, 800, 1500)
	if len(got) != 1 || got[0].ID != "small" {
		t.Fatal("oversize passage not skipped intact")
	}
	if RecallTokens(FormatRetrieveSection(got)) > 1500 {
		t.Fatal("hard budget exceeded")
	}
}

func TestRecallGenerationIgnoresAccess(t *testing.T) {
	mm, err := NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Passage{ID: "one", Content: "Nebula uses Copper."}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	gen, _, err := mm.RecallSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.MarkRetrieved([]Passage{p}); err != nil {
		t.Fatal(err)
	}
	next, _, err := mm.RecallSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if gen != next {
		t.Fatal("mark access changed semantic generation")
	}
}
