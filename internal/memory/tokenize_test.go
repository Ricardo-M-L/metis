package memory

// tokenize_test.go pins the 2026-05-15 CJK bigram + unigram extension
// to the BM25 tokenizer.
//
// Pre-fix: tokenize() only kept ASCII a-z / 0-9 with a >2-char floor.
// Chinese/Japanese/Korean queries returned [] tokens → BM25 hit list
// always empty → AutoRetrieve effectively disabled for non-Latin
// users. AutoRetrieve_test.go even had to use English-only queries
// to exercise the BM25 path.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestTokenize_AsciiBackwardCompat(t *testing.T) {
	t.Parallel()
	// 3+ char Latin words still picked up; 2-char drops; punctuation
	// is a separator. Match the original behavior so existing archival
	// passages tokenize identically and we don't invalidate IDF.
	got := tokenize("Hello, world! Go is ai")
	want := []string{"hello", "world"} // "go" + "ai" both <3 chars, dropped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ASCII regression:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestTokenize_CJKEmitsUnigramAndBigram(t *testing.T) {
	t.Parallel()
	got := tokenize("我喜欢猫")
	// Unigrams: 我, 喜, 欢, 猫
	// Bigrams:  我喜, 喜欢, 欢猫
	want := []string{"我", "喜", "欢", "猫", "我喜", "喜欢", "欢猫"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CJK tokens wrong:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestTokenize_SingleCJKChar_UnigramOnly(t *testing.T) {
	t.Parallel()
	// One char isolated by spaces/punctuation: unigram only, no bigram
	// possible. Important so a chat with a stray "好" doesn't crash.
	got := tokenize("猫")
	want := []string{"猫"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single CJK:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestTokenize_MixedScript(t *testing.T) {
	t.Parallel()
	// ASCII and CJK runs are flushed separately at the boundary —
	// no cross-script token like "abc我" should appear.
	got := tokenize("Python 写法 vs Go 写法")
	// Expected (order matters: ASCII flushed at " ", CJK flushed at " "/Latin):
	//   python, 写, 法, 写法, go, 写, 法, 写法 — but "go" <3 chars dropped.
	want := []string{"python", "写", "法", "写法", "写", "法", "写法"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mixed script:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestTokenize_CJKPunctuationIsSeparator(t *testing.T) {
	t.Parallel()
	// `、`、`。`、`「」` (U+3000 block) are NOT indexed — they're
	// delimiters between phrases.
	got := tokenize("你好、世界")
	// Unigrams: 你, 好, 世, 界 ; Bigrams within each run: "你好" + "世界"
	wantSet := map[string]bool{
		"你": true, "好": true, "世": true, "界": true,
		"你好": true, "世界": true,
	}
	for _, tok := range got {
		if !wantSet[tok] {
			t.Errorf("unexpected token %q (CJK punctuation should be separator)", tok)
		}
	}
	// Should NOT contain 4-char or cross-punctuation bigrams.
	for _, tok := range got {
		if strings.Contains(tok, "、") || strings.Contains(tok, "。") {
			t.Errorf("CJK punctuation leaked into token %q", tok)
		}
	}
	if len(got) != len(wantSet) {
		t.Errorf("token count mismatch: got %d, want %d (set=%v, got=%v)", len(got), len(wantSet), keys(wantSet), got)
	}
}

func TestTokenize_KanaAndHangul(t *testing.T) {
	t.Parallel()
	// Spot-check Japanese kana + Korean syllable blocks emit tokens —
	// the tokenizer's CJK detection covers more than just Han.
	jp := tokenize("こんにちは")
	if len(jp) == 0 {
		t.Errorf("hiragana returned no tokens")
	}
	// Should include unigrams for each kana.
	if !contains(jp, "こ") || !contains(jp, "は") {
		t.Errorf("hiragana unigrams missing: %v", jp)
	}
	// And at least one bigram.
	hasBigram := false
	for _, tok := range jp {
		if len([]rune(tok)) == 2 {
			hasBigram = true
			break
		}
	}
	if !hasBigram {
		t.Errorf("hiragana bigrams missing: %v", jp)
	}

	kr := tokenize("안녕하세요")
	if len(kr) == 0 {
		t.Errorf("hangul returned no tokens")
	}
}

// TestAutoRetrieve_CJKQueryNowHits — end-to-end check via the public
// AutoRetrieve API. Pre-fix this returned empty (the Chinese query
// tokenized to nothing). Post-fix the unigram "猫" lets BM25 hit
// the Chinese passage.
func TestAutoRetrieve_CJKQueryNowHits(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	mm, err := NewMemoryManager(dir)
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	for _, p := range []Passage{
		{Content: "我家的英短猫叫毛球，喜欢用爪子拍球。"},
		{Content: "Go 的 goroutine 是 M:N 调度。"},
		{Content: "Python の GIL は単一スレッド実行を強制する。"},
	} {
		if err := mm.archival.Insert(p); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// Chinese query — pre-fix returned "" because tokenize dropped
	// every char. Post-fix should hit the cat passage via the "猫"
	// unigram.
	got := mm.AutoRetrieve("我家的猫叫什么", 1)
	if got == "" {
		t.Fatal("CJK query returned empty — BM25 didn't tokenize Chinese")
	}
	if !strings.Contains(got, "毛球") {
		t.Errorf("expected cat passage in hit; got:\n%s", got)
	}

	// Japanese query → Japanese passage.
	got = mm.AutoRetrieve("Python の スレッド", 1)
	if got == "" {
		t.Fatal("JP query returned empty")
	}
	if !strings.Contains(got, "GIL") {
		t.Errorf("expected JP passage; got:\n%s", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
