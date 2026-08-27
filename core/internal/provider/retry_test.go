package provider_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// countingProvider fails a fixed number of times before succeeding, so a test
// can assert exactly how many attempts the policy made.
type countingProvider struct {
	failures int
	err      error
	attempts int
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *countingProvider) Generate(context.Context, provider.Request) (provider.Stream, error) {
	p.attempts++
	if p.attempts <= p.failures {
		return nil, p.err
	}
	return &emptyStream{}, nil
}

type emptyStream struct{}

func (*emptyStream) Recv(context.Context) (provider.Event, error) { return nil, io.EOF }
func (*emptyStream) Close() error                                 { return nil }

func instantRetry(p provider.Provider, attempts int) *provider.Retrying {
	return provider.WithRetry(p, provider.RetryPolicy{
		MaxAttempts: attempts,
		BaseDelay:   time.Microsecond,
		MaxDelay:    time.Millisecond,
	})
}

func TestRetriesTransientFailures(t *testing.T) {
	inner := &countingProvider{
		failures: 2,
		err:      provider.NewError(provider.KindOverloaded, "counting", "m", "busy", nil),
	}

	if _, err := instantRetry(inner, 4).Generate(context.Background(), provider.Request{}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if inner.attempts != 3 {
		t.Errorf("made %d attempts, want 3", inner.attempts)
	}
}

// Retrying a request the server has already rejected as malformed only burns
// quota; the answer will not change.
func TestDoesNotRetryPermanentFailures(t *testing.T) {
	for _, kind := range []provider.ErrorKind{
		provider.KindInvalidRequest,
		provider.KindAuth,
		provider.KindNotFound,
		provider.KindContextOverflow,
		provider.KindContentFiltered,
	} {
		t.Run(string(kind), func(t *testing.T) {
			inner := &countingProvider{
				failures: 10,
				err:      provider.NewError(kind, "counting", "m", "nope", nil),
			}

			if _, err := instantRetry(inner, 4).Generate(context.Background(), provider.Request{}); err == nil {
				t.Fatal("expected the error to surface")
			}
			if inner.attempts != 1 {
				t.Errorf("made %d attempts for %s, want 1", inner.attempts, kind)
			}
		})
	}
}

func TestStopsAfterMaxAttempts(t *testing.T) {
	inner := &countingProvider{
		failures: 100,
		err:      provider.NewError(provider.KindTransient, "counting", "m", "flaky", nil),
	}

	if _, err := instantRetry(inner, 3).Generate(context.Background(), provider.Request{}); err == nil {
		t.Fatal("expected the error to surface")
	}
	if inner.attempts != 3 {
		t.Errorf("made %d attempts, want 3", inner.attempts)
	}
}

// A cancelled context is the caller's decision. Retrying through it would make
// an interrupt take several backoffs to land.
func TestCancellationIsNotRetried(t *testing.T) {
	inner := &countingProvider{
		failures: 100,
		err:      context.Canceled,
	}

	_, err := instantRetry(inner, 4).Generate(context.Background(), provider.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if inner.attempts != 1 {
		t.Errorf("made %d attempts, want 1", inner.attempts)
	}
}

func TestCancellationDuringBackoffStops(t *testing.T) {
	inner := &countingProvider{
		failures: 100,
		err:      provider.NewError(provider.KindTransient, "counting", "m", "flaky", nil),
	}

	retrying := provider.WithRetry(inner, provider.RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   time.Second,
		MaxDelay:    time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := retrying.Generate(ctx, provider.Request{}); err == nil {
		t.Fatal("expected an error")
	}
	if inner.attempts > 1 {
		t.Errorf("kept retrying a cancelled context: %d attempts", inner.attempts)
	}
}

// The server knows when its quota resets; a locally computed backoff does not.
func TestRetryAfterOverridesBackoff(t *testing.T) {
	after := 7 * time.Second
	err := &provider.Error{
		Kind:       provider.KindRateLimited,
		RetryAfter: &after,
	}

	policy := provider.RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: time.Minute}
	if delay := policy.Delay(1, err); delay != after {
		t.Errorf("delay %v, want %v", delay, after)
	}
}

func TestRetryAfterIsCappedByMaxDelay(t *testing.T) {
	after := time.Hour
	err := &provider.Error{Kind: provider.KindRateLimited, RetryAfter: &after}

	policy := provider.RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: 30 * time.Second}
	if delay := policy.Delay(1, err); delay != 30*time.Second {
		t.Errorf("delay %v, want the max of 30s", delay)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	policy := provider.RetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second}
	transient := provider.NewError(provider.KindTransient, "p", "m", "", nil)

	first := policy.Delay(1, transient)
	second := policy.Delay(2, transient)

	if second <= first {
		t.Errorf("backoff did not grow: %v then %v", first, second)
	}
	if capped := policy.Delay(20, transient); capped > policy.MaxDelay {
		t.Errorf("delay %v exceeded the max of %v", capped, policy.MaxDelay)
	}
}

func TestErrorKindRetryability(t *testing.T) {
	retryable := []provider.ErrorKind{
		provider.KindRateLimited, provider.KindOverloaded, provider.KindTransient,
	}
	permanent := []provider.ErrorKind{
		provider.KindInvalidRequest, provider.KindAuth, provider.KindNotFound,
		provider.KindContextOverflow, provider.KindContentFiltered, provider.KindUnknown,
	}

	for _, kind := range retryable {
		if !kind.Retryable() {
			t.Errorf("%s should be retryable", kind)
		}
	}
	for _, kind := range permanent {
		if kind.Retryable() {
			t.Errorf("%s should not be retryable", kind)
		}
	}
}

func TestKindOfUnwrapsWrappedErrors(t *testing.T) {
	inner := provider.NewError(provider.KindRateLimited, "p", "m", "slow down", nil)
	wrapped := errors.Join(errors.New("context"), inner)

	if kind := provider.KindOf(wrapped); kind != provider.KindRateLimited {
		t.Errorf("got %s, want %s", kind, provider.KindRateLimited)
	}
	if !provider.IsRetryable(wrapped) {
		t.Error("wrapped retryable error reported as permanent")
	}
}

func TestLastUserTextFindsMostRecent(t *testing.T) {
	req := provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: provider.Text("first")},
		{Role: provider.RoleAssistant, Content: provider.Text("reply")},
		{Role: provider.RoleUser, Content: provider.Text("second")},
	}}

	if got := req.LastUserText(); got != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}
	if got := (provider.Request{}).LastUserText(); got != "" {
		t.Errorf("got %q for an empty request, want empty", got)
	}
}
