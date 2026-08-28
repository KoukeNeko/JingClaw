package provider

import (
	"context"
	"errors"
	"io"
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

// Retrying wraps a Provider with retry.
//
// Retrying covers two moments, because providers fail at two different ones. A
// provider that makes its request inside Generate fails there. A provider that
// hands back a lazy stream — the Gemini adapter does, and it is the normal
// shape for a streaming SDK — has not contacted anybody by the time Generate
// returns, so every failure it has, including the retryable ones, arrives on
// the first Recv instead.
//
// Watching only Generate therefore meant that for such a provider nothing was
// ever retried: rate limits configured for four attempts got exactly one, and
// a 429 that named the second it would be free at was reported to the user as
// a dead run.
//
// What does not change is the boundary. A stream that has already produced
// output is never replayed: re-running a generation that emitted half an
// answer would duplicate that text, and with tools in play the second attempt
// can decide to do something different from what the first already started.
// Before the first event there is nothing to duplicate, which is exactly why
// that case is safe and the later one is not.
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
			// The attempts already spent are carried into the stream, so a
			// provider that fails at both moments cannot get more tries than
			// the policy allows by splitting them across the two.
			return &retryingStream{
				parent:  r,
				request: req,
				stream:  stream,
				attempt: attempt,
				sleep:   sleep,
			}, nil
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

// retryingStream resends a request that failed before it said anything.
type retryingStream struct {
	parent  *Retrying
	request Request
	sleep   func(context.Context, time.Duration) error

	stream  Stream
	attempt int

	// emitted latches once the caller has been given anything at all. From
	// that moment the request can never be resent, because the caller is
	// already holding part of an answer that a second attempt would not
	// produce again identically.
	emitted bool
}

func (s *retryingStream) Recv(ctx context.Context) (Event, error) {
	attempts := s.parent.Policy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	for {
		event, err := s.stream.Recv(ctx)
		if err == nil {
			s.emitted = true
			return event, nil
		}

		if s.emitted || errors.Is(err, io.EOF) {
			return nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if !IsRetryable(err) || s.attempt >= attempts {
			return nil, err
		}

		if waitErr := s.sleep(ctx, s.parent.Policy.Delay(s.attempt, err)); waitErr != nil {
			return nil, waitErr
		}

		// The abandoned stream is closed before another is opened, so a
		// provider holding a connection per stream does not accumulate one
		// per attempt.
		_ = s.stream.Close()

		s.attempt++
		next, genErr := s.parent.Provider.Generate(ctx, s.request)
		if genErr != nil {
			return nil, genErr
		}
		s.stream = next
	}
}

func (s *retryingStream) Close() error { return s.stream.Close() }

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
