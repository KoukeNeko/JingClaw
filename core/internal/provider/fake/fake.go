// Package fake provides a deterministic Provider for the walking skeleton.
//
// It exists so the whole path — RPC, runtime, event log, streaming, resume,
// cancellation — can be exercised offline with no API key and no network. That
// makes the first streaming bug attributable to exactly one layer. This
// harness stays useful long after real providers land; it should not be
// deleted once M1 arrives.
package fake

import (
	"context"
	"io"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

const defaultPrefix = "收到："

// Provider echoes the user's text back in chunks, with an optional delay
// between them so streaming and mid-stream interruption are observable.
type Provider struct {
	// Prefix is emitted as the first chunk.
	Prefix string

	// ChunkDelay is applied before every chunk after the first.
	ChunkDelay time.Duration
}

func New(chunkDelay time.Duration) *Provider {
	return &Provider{Prefix: defaultPrefix, ChunkDelay: chunkDelay}
}

func (p *Provider) Generate(ctx context.Context, req runtime.ModelRequest) (runtime.ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prefix := p.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}

	chunks := []string{prefix}
	if req.LastUserText != "" {
		chunks = append(chunks, req.LastUserText)
	}

	return &stream{chunks: chunks, delay: p.ChunkDelay}, nil
}

type stream struct {
	chunks []string
	delay  time.Duration
	next   int
}

func (s *stream) Recv(ctx context.Context) (runtime.ModelEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.next >= len(s.chunks) {
		return nil, io.EOF
	}

	if s.next > 0 && s.delay > 0 {
		// Honour cancellation while waiting, so an interrupt takes effect
		// mid-generation instead of after the whole reply is produced.
		timer := time.NewTimer(s.delay)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	text := s.chunks[s.next]
	s.next++
	return runtime.TextDelta{Text: text}, nil
}

func (s *stream) Close() error { return nil }
