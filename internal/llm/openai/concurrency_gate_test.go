package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestOpenAIRequestSlotQueuesPastCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &OpenAI{requestSlots: make(chan struct{}, 1)}
		releaseFirst, err := o.acquireRequestSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		acquired := make(chan func(), 1)
		go func() {
			release, err := o.acquireRequestSlot(context.Background())
			if err == nil {
				acquired <- release
			}
		}()
		synctest.Wait()
		select {
		case <-acquired:
			t.Fatal("second request bypassed the one-slot concurrency gate")
		default:
		}

		releaseFirst()
		synctest.Wait()
		select {
		case releaseSecond := <-acquired:
			releaseSecond()
		default:
			t.Fatal("queued request did not acquire after the stream released")
		}
	})
}

type immediateEOFStream struct{}

func (immediateEOFStream) Recv() (provider.StreamEvent, error) { return provider.StreamEvent{}, io.EOF }
func (immediateEOFStream) Close() error                        { return nil }

func TestRequestSlotStreamReleasesExactlyOnce(t *testing.T) {
	releases := 0
	s := &requestSlotStream{
		StreamReader: immediateEOFStream{},
		release:      func() { releases++ },
	}
	if _, err := s.Recv(); err != io.EOF {
		t.Fatalf("Recv err = %v, want EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1", releases)
	}
}

func TestNewOpenAIRequestSlotsEnvOverride(t *testing.T) {
	t.Setenv("METIS_OPENAI_MAX_CONCURRENCY", "")
	if slots := newOpenAIRequestSlots(); cap(slots) != defaultOpenAIConcurrency {
		t.Fatalf("default slot capacity = %d, want %d", cap(slots), defaultOpenAIConcurrency)
	}
	t.Setenv("METIS_OPENAI_MAX_CONCURRENCY", "2")
	if slots := newOpenAIRequestSlots(); cap(slots) != 2 {
		t.Fatalf("slot capacity = %d, want 2", cap(slots))
	}
	t.Setenv("METIS_OPENAI_MAX_CONCURRENCY", "0")
	if slots := newOpenAIRequestSlots(); slots != nil {
		t.Fatal("zero override should disable the concurrency gate")
	}
}

type openAIRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAIRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAIStreamRetries429AndHonorsRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls < 3 {
				return &http.Response{
					Status:     "429 Too Many Requests",
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": []string{"1"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rpm exhausted"}}`)),
				}, nil
			}
			return &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})}
		o := &OpenAI{
			APIKey: "key", BaseURL: "https://token.sensenova.cn/v1",
			Model: "deepseek-v4-flash", MaxTokens: 128, httpClient: client,
		}
		start := time.Now()
		stream, err := o.Stream(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("elapsed = %v, want two Retry-After waits", elapsed)
		}
	})
}

func TestOpenAI429CooldownIsSharedWithWaitingRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &OpenAI{}
		if got := o.noteRateLimit(2 * time.Second); got != 2*time.Second {
			t.Fatalf("initial cooldown = %v, want 2s", got)
		}
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- o.waitRateCooldown(context.Background()) }()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("waiting sibling delayed %v, want shared 2s cooldown", elapsed)
		}
	})
}

func TestOpenAI429CooldownWaitCancelsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &OpenAI{}
		o.noteRateLimit(time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- o.waitRateCooldown(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-done; err != context.Canceled {
			t.Fatalf("cooldown cancellation err = %v, want context.Canceled", err)
		}
	})
}

func TestOpenAISharedCooldownWaitersStillRespectRequestSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &OpenAI{requestSlots: make(chan struct{}, 2)}
		o.noteRateLimit(time.Second)

		acquired := make(chan func(), 4)
		for range 4 {
			go func() {
				release, err := o.acquireRequestSlot(context.Background())
				if err != nil {
					return
				}
				if err := o.waitRateCooldown(context.Background()); err != nil {
					release()
					return
				}
				acquired <- release
			}()
		}
		// Drive the fake clock through the shared deadline explicitly. The
		// two other goroutines are blocked on the ordinary request-slot
		// channel, which synctest does not use to auto-advance time.
		time.Sleep(time.Second)
		synctest.Wait()

		// The shared deadline releases both admitted waiters together, but
		// the remaining two must stay behind the provider slot gate.
		if got := len(acquired); got != 2 {
			t.Fatalf("requests admitted after cooldown = %d, want slot cap 2", got)
		}
		first := <-acquired
		second := <-acquired
		first()
		second()
		synctest.Wait()
		if got := len(acquired); got != 2 {
			t.Fatalf("queued requests admitted after release = %d, want 2", got)
		}
		(<-acquired)()
		(<-acquired)()
	})
}

func TestOpenAI429WithoutHeaderEscalatesSharedCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &OpenAI{}
		want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
		for i, expected := range want {
			if got := o.noteRateLimit(0); got != expected {
				t.Fatalf("strike %d cooldown = %v, want %v", i+1, got, expected)
			}
		}
	})
}
