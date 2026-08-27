package provider

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// RetryPolicy governs how a failed request is resent.
type RetryPolicy struct {
	// MaxAttempts counts the first try. Two means one retry.
	MaxAttempts int

	BaseDelay time.Duration
	MaxDelay  time.Duration

	// Jitter spreads retries so a fleet of clients recovering from the same
	// outage does not resend in lockstep.
	Jitter float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Jitter:      0.3,
	}
}

// Delay computes the wait before the given attempt, honouring a server's
// Retry-After over any locally computed schedule: the server knows when its
// quota resets and we do not.
func (p RetryPolicy) Delay(attempt int, err error) time.Duration {
	if after, ok := RetryAfter(err); ok {
		if after > p.MaxDelay {
			return p.MaxDelay
		}
		return after
	}

	backoff := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if backoff > float64(p.MaxDelay) {
		backoff = float64(p.MaxDelay)
	}

	if p.Jitter > 0 {
		// Full-width jitter around the computed backoff.
		spread := backoff * p.Jitter
		backoff += (rand.Float64()*2 - 1) * spread
	}
	if backoff < 0 {
		backoff = 0
	}

	return time.Duration(backoff)
}

// Retrying wraps a Provider with retry on the initial request.
//
// It deliberately does not retry a stream that has already produced output.
// Re-running a generation that emitted half an answer would duplicate that
// text, and once tool calls exist a re-run can decide to do something
// different from what the first attempt already started. A partially consumed
// stream that fails is surfaced, not silently replayed.
type Retrying struct {
	Provider Provider
	Policy   RetryPolicy

	// sleep is injectable so tests do not wait out real backoff.
	sleep func(context.Context, time.Duration) error
}

func WithRetry(p Provider, policy RetryPolicy) *Retrying {
	return &Retrying{Provider: p, Policy: policy, sleep: sleepContext}
}

func (r *Retrying) Name() string { return r.Provider.Name() }

func (r *Retrying) Models(ctx context.Context) ([]ModelInfo, error) {
	return r.Provider.Models(ctx)
}

func (r *Retrying) Generate(ctx context.Context, req Request) (Stream, error) {
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepContext
	}

	attempts := r.Policy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		stream, err := r.Provider.Generate(ctx, req)
		if err == nil {
			return stream, nil
		}
		lastErr = err

		// A cancelled context is the caller's decision, not a failure to
		// paper over.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if !IsRetryable(err) || attempt == attempts {
			return nil, err
		}

		if err := sleep(ctx, r.Policy.Delay(attempt, err)); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
