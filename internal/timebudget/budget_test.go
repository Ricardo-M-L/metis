package timebudget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{"", 0, false}, {"0", 0, false}, {" 0 ", 0, false},
		{"1", time.Second, false}, {"21600", 6 * time.Hour, false},
		{"-1", 0, true}, {"wrong", 0, true}, {"1.5", 0, true},
		{"9223372037", 0, true}, {"99999999999999999999", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("TEST_WALL_BUDGET", tc.value)
			got, err := FromEnv("TEST_WALL_BUDGET")
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("FromEnv(%q) = %v, %v; want %v, bad=%v", tc.value, got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestUnlimitedSurvivesSixHoursAndStillCancels(t *testing.T) {
	for _, value := range []string{"", "0"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_WALL_BUDGET", value)
			synctest.Test(t, func(t *testing.T) {
				parent, stop := context.WithCancel(context.Background())
				ctx, cancel, err := WithEnv(parent, "TEST_WALL_BUDGET")
				if err != nil {
					t.Fatal(err)
				}
				defer cancel()
				if _, ok := ctx.Deadline(); ok {
					t.Fatal("unlimited context has a deadline")
				}
				time.Sleep(6 * time.Hour)
				if ctx.Err() != nil {
					t.Fatalf("six-hour context stopped: %v", ctx.Err())
				}
				stop()
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatal("parent cancellation lost")
				}
			})
		})
	}
}

func TestExplicitDeadlineAndCauseInheritedByChild(t *testing.T) {
	t.Setenv("TEST_WALL_BUDGET", "2")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel, err := WithEnv(context.Background(), "TEST_WALL_BUDGET")
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		child, cancelChild := context.WithTimeout(ctx, time.Hour)
		defer cancelChild()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		cause := CauseError(child, child.Err())
		if !errors.Is(cause, context.DeadlineExceeded) || !strings.Contains(cause.Error(), "TEST_WALL_BUDGET") {
			t.Fatalf("child deadline cause = %v", cause)
		}
		providerError := errors.New("provider error")
		if CauseError(child, providerError) != providerError {
			t.Fatal("unrelated error replaced")
		}
		checkpointError := errors.New("checkpoint failed")
		joined := CauseError(child, errors.Join(child.Err(), checkpointError))
		if !errors.Is(joined, checkpointError) || !errors.Is(joined, context.DeadlineExceeded) {
			t.Fatalf("cause conversion discarded a checkpoint error: %v", joined)
		}
	})
}

func TestEarlierParentDeadlineWins(t *testing.T) {
	t.Setenv("TEST_WALL_BUDGET", "21600")
	synctest.Test(t, func(t *testing.T) {
		parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
		defer cancelParent()
		ctx, cancel, err := WithEnv(parent, "TEST_WALL_BUDGET")
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		pd, _ := parent.Deadline()
		cd, _ := ctx.Deadline()
		if !cd.Equal(pd) {
			t.Fatal("local budget extended parent's deadline")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("parent deadline not inherited")
		}
	})
}
