package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
)

// A gateway that keeps failing is started again after a wait that doubles and
// stops growing at the limit.
func TestAFailingGatewayIsRetriedWithABoundedBackoff(t *testing.T) {
	p := pace{first: 20 * time.Millisecond, longest: 80 * time.Millisecond, healthy: time.Hour}

	var starts []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(context.Context) error {
		starts = append(starts, time.Now())
		if len(starts) == 6 {
			cancel()
		}
		return errors.New("connection lost")
	}

	var told []string
	keepChatUp(ctx, run, func(line string) { told = append(told, line) }, p)

	if len(starts) != 6 {
		t.Fatalf("started %d times, want 6", len(starts))
	}
	// 20, 40, 80, 80: each at least the wait it was due, and none far past the
	// limit, which is what "bounded" means.
	for i, due := range []time.Duration{20, 40, 80, 80} {
		due *= time.Millisecond
		gap := starts[i+1].Sub(starts[i])
		if gap < due {
			t.Errorf("retry %d came after %v, due after %v", i+1, gap, due)
		}
		if gap > due+400*time.Millisecond {
			t.Errorf("retry %d came after %v, far past the %v it was due", i+1, gap, due)
		}
	}
	if len(told) != 1 || !strings.Contains(told[0], "trying again") {
		t.Errorf("a streak of failures was told as %q, want one notice", told)
	}
}

// A run that lasted long enough counts as healthy, and the wait starts again.
func TestAGatewayThatRanForAWhileStartsTheBackoffOver(t *testing.T) {
	p := pace{first: 10 * time.Millisecond, longest: time.Hour, healthy: 30 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts []time.Time
	run := func(context.Context) error {
		starts = append(starts, time.Now())
		if len(starts) == 3 {
			cancel()
		}
		time.Sleep(40 * time.Millisecond)
		return errors.New("dropped")
	}

	var told int
	keepChatUp(ctx, run, func(string) { told++ }, p)

	// Both waits are the first one. Without the reset the second would be 20ms
	// and the notice would be given once, not twice.
	if told != 2 {
		t.Errorf("told %d times, want 2 (one per streak)", told)
	}
}

// A configuration failure is said once and is not tried again, and the panic of
// a gateway is an error like any other.
func TestOnlyAConfigurationFailureIsNotRetried(t *testing.T) {
	p := pace{first: time.Millisecond, longest: time.Millisecond, healthy: time.Hour}

	var told []string
	starts := 0
	keepChatUp(context.Background(), func(context.Context) error {
		starts++
		return gateway.Unconfigured(errors.New("the telegram bot token: no credential"))
	}, func(line string) { told = append(told, line) }, p)

	if starts != 1 {
		t.Errorf("started %d times", starts)
	}
	if len(told) != 1 || !strings.HasPrefix(told[0], "chat is off: the telegram bot token: no credential") {
		t.Errorf("told %q", told)
	}

	ctx, cancel := context.WithCancel(context.Background())
	starts = 0
	keepChatUp(ctx, func(context.Context) error {
		starts++
		if starts == 2 {
			cancel()
		}
		panic("boom")
	}, func(string) {}, p)
	if starts != 2 {
		t.Errorf("a panicking gateway was started %d times, want 2", starts)
	}
}

// A stop that is asked for is not a failure to retry.
func TestAStoppedGatewayIsNotStartedAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	starts := 0
	keepChatUp(ctx, func(ctx context.Context) error {
		starts++
		cancel()
		return context.Canceled
	}, func(string) { t.Error("something was said about a stop") }, chatPace)
	if starts != 1 {
		t.Errorf("started %d times", starts)
	}
}
