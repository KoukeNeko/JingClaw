// Package fake provides a deterministic Provider.
//
// It exists so the whole path — RPC, runtime, event log, streaming, resume,
// cancellation — can be exercised offline with no API key and no network. That
// makes a streaming bug attributable to exactly one layer. This harness stays
// useful long after real providers land; it should not be deleted.
package fake

import (
	"context"
	"io"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

const (
	providerName  = "fake"
	defaultPrefix = "收到："

	// ModelID is what the fake provider answers to. Configuration treats it
	// like any other model so nothing special-cases the test path.
	ModelID = "fake-echo"
)

// Provider echoes the user's text back in chunks, with an optional delay
// between them so streaming and mid-stream interruption are observable.
type Provider struct {
	// Prefix is emitted as the first chunk.
	Prefix string

	// ChunkDelay is applied before every chunk after the first.
	ChunkDelay time.Duration
}

var _ provider.Provider = (*Provider)(nil)

func New(chunkDelay time.Duration) *Provider {
	return &Provider{Prefix: defaultPrefix, ChunkDelay: chunkDelay}
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) Models(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{
		ID:              ModelID,
		DisplayName:     "Fake echo",
		Description:     "Deterministic echo provider used for offline testing.",
		ContextWindow:   1 << 20,
		ContextSource:   provider.ContextCatalog,
		MaxOutputTokens: 1 << 16,
		Capabilities:    provider.Capabilities{Streaming: true},
	}}, nil
}

func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prefix := p.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}

	chunks := []string{prefix}
	if text := req.LastUserText(); text != "" {
		chunks = append(chunks, text)
	}

	return &stream{chunks: chunks, delay: p.ChunkDelay}, nil
}

type stream struct {
	chunks []string
	delay  time.Duration

	next      int
	usageSent bool
	done      bool
}

func (s *stream) Recv(ctx context.Context) (provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.next < len(s.chunks) {
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
		return provider.TextDelta{Text: text}, nil
	}

	// A real provider reports usage, so the fake does too; otherwise the
	// accounting path would only ever be exercised against the network.
	if !s.usageSent {
		s.usageSent = true

		var output int64
		for _, chunk := range s.chunks {
			output += int64(len([]rune(chunk)))
		}
		return provider.UsageDelta{Usage: domain.Usage{
			InputTokens:  int64(len([]rune(s.chunks[len(s.chunks)-1]))),
			OutputTokens: output,
		}}, nil
	}

	if !s.done {
		s.done = true
		return provider.Completed{StopReason: domain.StopEndTurn}, nil
	}

	return nil, io.EOF
}

func (s *stream) Close() error { return nil }
