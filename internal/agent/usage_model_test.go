package agent

import (
	"context"
	"io"
	"testing"
	"testing/synctest"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type mutableUsageTestProvider struct {
	*recoveryTestProvider
	model string
}

func (p *mutableUsageTestProvider) ModelID() string { return p.model }

type switchModelStream struct {
	llm.StreamReader
	switchModel func()
}

func (s *switchModelStream) Recv() (llm.StreamEvent, error) {
	if s.switchModel != nil {
		s.switchModel()
		s.switchModel = nil
	}
	return s.StreamReader.Recv()
}

func TestUsageModelUsesActualProviderAcrossStreamRetries(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			if call == 1 {
				return reportedRecoveryUsageStream(usageTotals{in: 4, out: 1}, io.ErrUnexpectedEOF, true), nil
			}
			return reportedRecoveryUsageStream(usageTotals{in: 6, out: 2}, nil, false), nil
		}}
		loop := newStreamRecoveryTestLoop(p, &recoveryTestTool{})
		loop.Model = "requested-alias"
		out := make(chan Event, 256)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatal(err)
		}
		close(out)
		var models []string
		for event := range out {
			if event.Kind == EventTokens {
				models = append(models, event.UsageModel)
			}
		}
		if len(models) != 2 || models[0] != "test-model" || models[1] != "test-model" {
			t.Fatalf("attempt model attribution = %v", models)
		}
	})
}

func TestUsageModelBecomesUnknownWhenProviderChangesMidStream(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		base := &recoveryTestProvider{t: t}
		provider := &mutableUsageTestProvider{recoveryTestProvider: base, model: "first"}
		base.stream = func(_ context.Context, call int) (llm.StreamReader, error) {
			if call == 1 {
				return &switchModelStream{StreamReader: reportedRecoveryUsageStream(usageTotals{in: 4, out: 1}, io.ErrUnexpectedEOF, true), switchModel: func() { provider.model = "second" }}, nil
			}
			return reportedRecoveryUsageStream(usageTotals{in: 6, out: 2}, nil, false), nil
		}
		loop := newStreamRecoveryTestLoop(base, &recoveryTestTool{})
		loop.Provider = provider
		out := make(chan Event, 256)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatal(err)
		}
		close(out)
		var models []string
		for event := range out {
			if event.Kind == EventTokens {
				models = append(models, event.UsageModel)
			}
		}
		if len(models) != 2 || models[0] != "" || models[1] != "second" {
			t.Fatalf("unsafe model attribution after switch: %v", models)
		}
	})
}
