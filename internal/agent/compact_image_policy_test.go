package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/imageprep"
	"github.com/Ricardo-M-L/metis/internal/llm"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

func policyImage(tag string) llm.ContentBlock {
	return llm.ContentBlock{Type: "image", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(tag))}
}

type imagePolicySummarizer struct {
	fakeSummarizer
	vision pubprovider.VisionCapability
}

func (p *imagePolicySummarizer) VisionCapability() pubprovider.VisionCapability { return p.vision }

func countPolicyImages(messages []llm.Message) int { return len(compactImageCandidates(messages)) }

func TestImageContextPolicyPreservesMoreThanThreeWhenTheyFit(t *testing.T) {
	var messages []llm.Message
	for i := 0; i < 7; i++ {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{policyImage(istr(i))}})
	}
	before := cloneMessages(messages)
	out, removed := fitImagesToContextBudget(messages, estimateActiveHistoryTokens(messages)+1)
	if removed != 0 || countPolicyImages(out) != 7 || !reflect.DeepEqual(out, before) {
		t.Fatalf("fitting native images were rewritten: removed=%d images=%d", removed, countPolicyImages(out))
	}
}

func TestImageContextPolicyPrioritizesReferencedTaskAttachment(t *testing.T) {
	important := policyImage("original design")
	id := canonicalImageAttachmentID(important)
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Original design"}, important}},
		msg(llm.RoleAssistant, "Design received"),
		msg(llm.RoleUser, "Work on a different screen"),
		toolUseMsg("shot", "Screenshot"),
		nestedViewImageResult("shot", policyImage("old screen").Data, policyImage("newest screen").Data),
		msg(llm.RoleUser, "Compare against the original attachment_id="+id),
	}
	before := cloneMessages(messages)
	out, removed := fitImagesToContextBudget(messages, 2_300)
	if removed != 2 || countPolicyImages(out) != 1 || out[0].Content[1].Type != "image" {
		t.Fatalf("task reference lost to newer screenshots: removed=%d history=%+v", removed, out)
	}
	if rendered := buildSummaryTranscript(out, false, 0); !strings.Contains(rendered, "attachment_id="+canonicalImageAttachmentID(policyImage("newest screen"))) || !strings.Contains(rendered, "capacity is insufficient") {
		t.Fatal("offloaded image did not retain an explicit capacity reason and stable ID")
	}
	if !reflect.DeepEqual(messages, before) || out[4].Content[0].ToolUseID != "shot" {
		t.Fatal("selection mutated source history or tool pairing")
	}
	again, removedAgain := fitImagesToContextBudget(out, 2_300)
	if removedAgain != 0 || !reflect.DeepEqual(again, out) {
		t.Fatal("fitted image context was rewritten a second time")
	}
}

func TestCompactKeepsAllCurrentTaskImagesWhenNativeBudgetFits(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.RetainMinMessages = 1
	cfg.RetainMinUserMessages = 1
	c := NewCompactor(cfg, "image-test", 200_000, p)
	messages := []llm.Message{msg(llm.RoleUser, "earlier task"), msg(llm.RoleAssistant, strings.Repeat("earlier result ", 1_000))}
	blocks := []llm.ContentBlock{{Type: "text", Text: "Compare all six attached designs"}}
	for i := 0; i < 6; i++ {
		blocks = append(blocks, policyImage("design "+istr(i)))
	}
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: blocks})
	out, err := c.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if countPolicyImages(out) != 6 {
		t.Fatalf("fixed newest-N pruning removed fitting task images: got=%d", countPolicyImages(out))
	}
}

func TestSummaryRequestIncludesNativeUserAndNestedImages(t *testing.T) {
	t.Setenv("METIS_TOOL_BATCH_TOKENS", "24000")
	p := &imagePolicySummarizer{vision: pubprovider.VisionSupported}
	c := NewCompactor(DefaultCompactionConfig(), "image-test", 40_000, p)
	first := policyImage(strings.Repeat("first image bytes", 25_000))
	second := policyImage(strings.Repeat("second image bytes", 25_000))
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Compare the lower bands of these two designs"}, first}},
		toolUseMsg("image-result", "ViewImage"),
		nestedViewImageResult("image-result", second.Data),
	}
	summary, err := c.summarize(context.Background(), messages, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || countPolicyImages(p.lastReq.Messages) != 2 {
		t.Fatalf("summary did not consume native images in one request: calls=%d images=%d", p.calls, countPolicyImages(p.lastReq.Messages))
	}
	for _, image := range []llm.ContentBlock{first, second} {
		id := canonicalImageAttachmentID(image)
		if !strings.Contains(summary, "attachment_id="+id) || !strings.Contains(summary, "native evidence supplied") {
			t.Fatal("checkpoint lacks stable re-readable image references")
		}
	}
	inputCap, outputCap := c.summaryWireLimits()
	fed := estimateStringTokens(p.lastReq.System) + estimateActiveHistoryTokens(p.lastReq.Messages)
	if fed > inputCap || p.lastReq.MaxTokens+estimateStringTokens(planSummaryImageEvidence(messages, p, 30_000).manifest) > outputCap {
		t.Fatalf("native summary request bypassed its budget: fed=%d cap=%d", fed, inputCap)
	}
	if estimateTokens(p.lastReq.Messages) <= inputCap {
		t.Fatal("fixture must exceed budget only if base64 is incorrectly tokenized")
	}
}

func imagePolicyMemoryHint(id string) map[string]string {
	raw, _ := json.Marshal(summaryMemoryDependencies{References: []recallReference{{ID: id, Hash: "source-hash"}}})
	return map[string]string{summaryMemoryHint: string(raw)}
}

func TestSummaryUnparsedImagesNeverInventFactsOrDonateOmittedImageSources(t *testing.T) {
	t.Setenv("METIS_TOOL_BATCH_TOKENS", "24000")
	for _, vision := range []pubprovider.VisionCapability{pubprovider.VisionSupported, pubprovider.VisionUnsupported} {
		t.Run(istr(int(vision)), func(t *testing.T) {
			p := &imagePolicySummarizer{vision: vision}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 7_000 // one native image fits the one-third visual share
			c := NewCompactor(cfg, "image-test", 40_000, p)
			older, recent := policyImage("old unknown contents"), policyImage("recent unknown contents")
			older.ProviderHint = imagePolicyMemoryHint("old-image-source")
			recent.ProviderHint = imagePolicyMemoryHint("recent-image-source")
			messages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Type: "text", Text: "Inspect the current images"}, older, recent,
			}}}
			var hints map[string]string
			summary, err := c.summarizeWithSources(context.Background(), messages, "", "", nil, &hints)
			if err != nil {
				t.Fatal(err)
			}
			wantImages := 1
			if vision == pubprovider.VisionUnsupported {
				wantImages = 0
			}
			if countPolicyImages(p.lastReq.Messages) != wantImages || !strings.Contains(summary, "unparsed image") {
				t.Fatalf("unadmitted image treated as parsed: images=%d summary=%q", countPolicyImages(p.lastReq.Messages), summary)
			}
			if strings.Contains(hints[summaryMemoryHint], "old-image-source") {
				t.Fatal("metadata for an omitted image donated source provenance")
			}
			if got := strings.Contains(hints[summaryMemoryHint], "recent-image-source"); got != (wantImages > 0) {
				t.Fatalf("native image provenance mismatch: %v hints=%v", got, hints)
			}
			if strings.Contains(summary, "old unknown contents") || strings.Contains(summary, "recent unknown contents") {
				t.Fatal("unparsed reference fabricated image contents")
			}
		})
	}
}

func TestSummaryUnknownWindowStillUsesBoundedNativeEvidence(t *testing.T) {
	p := &imagePolicySummarizer{vision: pubprovider.VisionSupported}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "unknown-window", 0, p)
	image := policyImage("native evidence without catalog limits")
	if _, err := c.summarize(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{image}}}, ""); err != nil {
		t.Fatal(err)
	}
	if countPolicyImages(p.lastReq.Messages) != 1 {
		t.Fatal("unknown window silently disabled native image evidence")
	}
}

func TestSummaryImageOnlyToolWrapperSourceRequiresAdmittedVisualEvidence(t *testing.T) {
	t.Setenv("METIS_TOOL_BATCH_TOKENS", "24000")
	for _, test := range []struct {
		name       string
		vision     pubprovider.VisionCapability
		inputCap   int
		body       string
		wantSource bool
	}{
		{"native image admitted", pubprovider.VisionSupported, 12_000, "", true},
		{"no vision", pubprovider.VisionUnsupported, 12_000, "", false},
		{"no visual budget", pubprovider.VisionSupported, 4_000, "", false},
		{"actual textual observation", pubprovider.VisionUnsupported, 12_000, "Observed: the submit button is disabled.", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &imagePolicySummarizer{vision: test.vision}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = test.inputCap
			c := NewCompactor(cfg, "image-test", 40_000, p)
			messages := []llm.Message{toolUseMsg("image", "ViewImage"), {Role: llm.RoleUser, Content: []llm.ContentBlock{{
				Type: "tool_result", ToolUseID: "image", ToolResult: test.body,
				ToolResultBlocks: []llm.ContentBlock{policyImage("source visual bytes")},
				ProviderHint:     imagePolicyMemoryHint("wrapper-image-source"),
			}}}}
			before := cloneMessages(messages)
			var hints map[string]string
			if _, err := c.summarizeWithSources(context.Background(), messages, "", "", nil, &hints); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(hints[summaryMemoryHint], "wrapper-image-source"); got != test.wantSource {
				t.Fatalf("image-only framing donated omitted visual source: got=%v want=%v hints=%v", got, test.wantSource, hints)
			}
			if !reflect.DeepEqual(messages, before) {
				t.Fatal("source filtering mutated original wrapper hints")
			}
		})
	}
}

func TestSummaryNativeEvidenceHasSeparateTransportAndVisualBounds(t *testing.T) {
	t.Setenv("METIS_TOOL_BATCH_TOKENS", "24000")
	p := &imagePolicySummarizer{vision: pubprovider.VisionSupported}
	blocks := []llm.ContentBlock{{Type: "text", Text: "Inspect these uploaded images"}}
	for i := 0; i < 5; i++ {
		// Five individually valid payloads exceed the local summary wire
		// allowance while all five still fit the native visual allowance.
		data := strings.Repeat("A", imageprep.MaxBase64Bytes-4) + base64.StdEncoding.EncodeToString([]byte{byte(i)})
		blocks = append(blocks, llm.ContentBlock{Type: "image", MediaType: "image/png", Data: data})
	}
	plan := planSummaryImageEvidence([]llm.Message{{Role: llm.RoleUser, Content: blocks}}, p, 100_000)
	if got := countPolicyImages([]llm.Message{{Content: plan.content}}); got != 4 {
		t.Fatalf("summary wire budget admitted %d images, want 4", got)
	}
	if !strings.Contains(plan.manifest, "unparsed image; summary image transport budget exhausted") {
		t.Fatal("wire-budget omission was not identified as unparsed")
	}
	plan = planSummaryImageEvidence([]llm.Message{{Content: blocks[:2]}}, p, 1_000)
	if len(plan.content) != 0 || !strings.Contains(plan.manifest, "summary visual budget exhausted") {
		t.Fatal("native visual bound was replaced by the transport-byte limit")
	}
}

func TestSummaryFailureLeavesImageHistoryAndCheckpointStateUntouched(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MaxSummaryRetries = 0
	cfg.RetainMinMessages = 1
	c := NewCompactor(cfg, "image-test", 40_000, &errSummarizer{err: errors.New("simulated summary failure")})
	image := policyImage("must survive failed summary")
	messages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Inspect this image"}, image}}}
	messages = append(messages, msg(llm.RoleAssistant, strings.Repeat("old result ", 1_000)))
	for i := 0; i < 5; i++ {
		messages = append(messages, msg(llm.RoleAssistant, "work result"), msg(llm.RoleUser, "next task "+istr(i)))
	}
	before := cloneMessages(messages)
	if _, err := c.Compact(context.Background(), messages); err == nil {
		t.Fatal("expected failed summary")
	}
	if !reflect.DeepEqual(messages, before) || c.LastSummary != "" {
		t.Fatal("failed summary changed original image history or checkpoint state")
	}
}
