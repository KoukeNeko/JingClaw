package provider_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// lateFailing is a provider shaped like the Gemini adapter: Generate hands
// back a stream before the request has been made, so every failure — including
// the ones worth retrying — arrives on the first Recv instead.
type lateFailing struct {
	failures int
	calls    int
}

func (p *lateFailing) Name() string { return "late" }

func (p *lateFailing) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *lateFailing) Generate(context.Context, provider.Request) (provider.Stream, error) {
	p.calls++
	fail := p.calls <= p.failures
	return &lateStream{fail: fail}, nil
}

type lateStream struct {
	fail bool
	done bool
}

func (s *lateStream) Recv(context.Context) (provider.Event, error) {
	if s.fail {
		after := 10 * time.Millisecond
		return nil, &provider.Error{
			Kind:       provider.KindRateLimited,
			Provider:   "late",
			StatusCode: 429,
			RetryAfter: &after,
			Message:    "quota exceeded",
		}
	}
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return provider.TextDelta{Text: "the answer"}, nil
}

func (s *lateStream) Close() error { return nil }

// The failure this actually had in production: a rate limit that never got
// retried, because the retry wrapper only watches Generate and this provider
// reports nothing there.
//
// A stream that fails before emitting anything has produced no output to
// duplicate, so resending it is exactly as safe as resending a request that
// failed outright — and it is the case that every Gemini error takes.
func TestARetryableFailureOnTheFirstReadIsRetried(t *testing.T) {
	upstream := &lateFailing{failures: 2}
	retrying := provider.WithRetry(upstream, provider.RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Second,
	})

	stream, err := retrying.Generate(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("a rate limit on the first read was not retried: %v", err)
	}
	if text, ok := event.(provider.TextDelta); !ok || text.Text != "the answer" {
		t.Fatalf("got %#v", event)
	}
	if upstream.calls != 3 {
		t.Errorf("the request was made %d times, want 3", upstream.calls)
	}
}

// Once a stream has produced output, resending it would duplicate what the
// caller already has, and with tools in play the second attempt can decide to
// do something different from what the first one already started.
func TestAFailureAfterOutputIsNotRetried(t *testing.T) {
	upstream := &failsAfterOutput{}
	retrying := provider.WithRetry(upstream, provider.RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   time.Millisecond,
	})

	stream, err := retrying.Generate(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Recv(context.Background()); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := stream.Recv(context.Background()); err == nil {
		t.Fatal("a failure after output was silently replayed")
	}
	if upstream.calls != 1 {
		t.Errorf("the request was made %d times; a half-delivered answer must not be resent", upstream.calls)
	}
}

type failsAfterOutput struct{ calls int }

func (p *failsAfterOutput) Name() string { return "half" }

func (p *failsAfterOutput) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *failsAfterOutput) Generate(context.Context, provider.Request) (provider.Stream, error) {
	p.calls++
	return &halfStream{}, nil
}

type halfStream struct{ sent bool }

func (s *halfStream) Recv(context.Context) (provider.Event, error) {
	if !s.sent {
		s.sent = true
		return provider.TextDelta{Text: "half an answer"}, nil
	}
	return nil, &provider.Error{Kind: provider.KindTransient, Provider: "half", StatusCode: 503}
}

func (s *halfStream) Close() error { return nil }

var _ = errors.Is
