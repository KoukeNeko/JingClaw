package provider

import (
	"context"
	"errors"
	"fmt"
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

	// Budget bounds the total time spent waiting across all attempts for one
	// request. Somebody is watching a chat channel while this happens, and a
	// server may ask for a delay longer than they are willing to sit through;
	// the honest answer then is to stop and say when it would be free, not to
	// retry early and fail again.
	//
	// Zero means the only bound is MaxAttempts.
	Budget time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Jitter:      0.3,
		Budget:      90 * time.Second,
	}
}

// Delay computes the wait before the given attempt.
//
// A server's own figure wins outright and is never rounded down. It is the
// earliest moment a retry may be sent, not an estimate to be improved on: the
// server knows when its window reopens and this process does not, and asking
// again before then spends an attempt to be told the same thing. MaxDelay
// bounds what this code invents, not what the server states — a delay too long
// to wait out is refused by the budget rather than shortened into a request
// certain to fail.
func (p RetryPolicy) Delay(attempt int, err error) time.Duration {
	if after, ok := RetryAfter(err); ok {
		// Jittered upward only. Every client told "twenty seconds" returning
		// at exactly twenty seconds rebuilds the spike that caused the limit.
		if p.Jitter > 0 {
			after += time.Duration(rand.Float64() * p.Jitter * float64(after))
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

// withinBudget reports whether another wait of this length is affordable, and
// how much would remain after it.
func (p RetryPolicy) withinBudget(spent, next time.Duration) bool {
	if p.Budget <= 0 {
		return true
	}
	return spent+next <= p.Budget
}

// ErrRetryBudgetExhausted reports that a failure was retryable but the wait
// the server asked for is longer than this request may spend.
//
// A distinct error because it is a different thing to tell somebody. The
// request did not fail on its merits; it ran out of patience, and the
// underlying error says when it would have been worth trying again.
type ErrRetryBudgetExhausted struct {
	Waited time.Duration
	Needed time.Duration
	Cause  error
}

func (e *ErrRetryBudgetExhausted) Error() string {
	return fmt.Sprintf("provider: gave up after waiting %s; the next attempt was %s away: %v",
		e.Waited.Round(time.Second), e.Needed.Round(time.Second), e.Cause)
}

func (e *ErrRetryBudgetExhausted) Unwrap() error { return e.Cause }

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

	var (
		lastErr error
		spent   time.Duration
	)
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
				spent:   spent,
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

		wait := r.Policy.Delay(attempt, err)
		if !r.Policy.withinBudget(spent, wait) {
			return nil, &ErrRetryBudgetExhausted{Waited: spent, Needed: wait, Cause: err}
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		spent += wait
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
	spent   time.Duration

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

		wait := s.parent.Policy.Delay(s.attempt, err)
		if !s.parent.Policy.withinBudget(s.spent, wait) {
			return nil, &ErrRetryBudgetExhausted{Waited: s.spent, Needed: wait, Cause: err}
		}
		if waitErr := s.sleep(ctx, wait); waitErr != nil {
			return nil, waitErr
		}
		s.spent += wait

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
