// Package fake provides a deterministic Provider.
//
// It exists so the whole path — RPC, runtime, event log, streaming, resume,
// cancellation — can be exercised offline with no API key and no network. That
// makes a streaming bug attributable to exactly one layer. This harness stays
// useful long after real providers land; it should not be deleted.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Script is what to do turn by turn instead of echoing.
	//
	// It exists because tool calls are a path only a model takes, and until
	// now checking that path end to end needed a real model — which makes a
	// check slow, needs a key or a local server, and fails for reasons that
	// have nothing to do with what is being checked.
	//
	// The turn is worked out from the conversation rather than kept here: one
	// provider serves every session, and a counter would make two sessions
	// running at once read each other's place in the script.
	Script []Turn

	// Reasoning is emitted as working-out before the answer, where set.
	//
	// Off by default: most checks are about the answer, and a fake that
	// always thought would make every one of them assert around it. It exists
	// so the path a real thinking model takes — recorded under its own kind,
	// refused by the projector, shown only on the control plane — can be
	// driven without one.
	Reasoning string
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

// Turn is one scripted answer.
type Turn struct {
	// Text is what it says. May be empty when it only calls a tool.
	Text string

	// Tool and Args are a call it makes. Empty Tool means it calls nothing,
	// which is how a script ends the turn.
	Tool string
	Args string
}

func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(p.Script) > 0 {
		return p.scripted(req), nil
	}

	prefix := p.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}

	chunks := []string{prefix}
	if text := req.LastUserText(); text != "" {
		chunks = append(chunks, text)
	}

	return &stream{chunks: chunks, reasoning: p.Reasoning, delay: p.ChunkDelay}, nil
}

// scripted answers according to the script, by counting how far through it
// this conversation already is.
func (p *Provider) scripted(req provider.Request) provider.Stream {
	// Each scripted turn that made a call left a tool observation behind, so
	// counting those is counting turns already taken. Counting assistant
	// messages instead would also count the one being written.
	taken := 0
	for _, message := range req.Messages {
		if message.Role == provider.RoleTool {
			taken++
		}
	}

	if taken >= len(p.Script) {
		// Past the end of the script, so it answers and stops. A script that
		// ran out mid-conversation would otherwise loop.
		return &stream{chunks: []string{p.Script[len(p.Script)-1].Text}, delay: p.ChunkDelay}
	}

	turn := p.Script[taken]
	chunks := []string{}
	if turn.Text != "" {
		chunks = append(chunks, turn.Text)
	}

	return &stream{
		chunks:    chunks,
		reasoning: p.Reasoning,
		delay:     p.ChunkDelay,
		call:      turn,
		callIndex: taken,
	}
}

type stream struct {
	chunks    []string
	reasoning string
	delay     time.Duration

	call      Turn
	callIndex int

	next          int
	reasoningSent bool
	callSent      bool
	usageSent     bool
	done          bool
}

func (s *stream) Recv(ctx context.Context) (provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Before the answer, as a real model thinks before it replies.
	if s.reasoning != "" && !s.reasoningSent {
		s.reasoningSent = true
		return provider.ReasoningDelta{Text: s.reasoning}, nil
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

	// The call comes after whatever the turn said, as a real model's does.
	if s.call.Tool != "" && !s.callSent {
		s.callSent = true
		args := s.call.Args
		if args == "" {
			args = "{}"
		}
		return provider.ToolCallRequested{
			ID:   fmt.Sprintf("call_%d", s.callIndex+1),
			Name: s.call.Tool,
			Args: json.RawMessage(args),
		}, nil
	}

	// A real provider reports usage, so the fake does too; otherwise the
	// accounting path would only ever be exercised against the network.
	if !s.usageSent {
		s.usageSent = true

		var output int64
		for _, chunk := range s.chunks {
			output += int64(len([]rune(chunk)))
		}
		input := int64(0)
		if len(s.chunks) > 0 {
			input = int64(len([]rune(s.chunks[len(s.chunks)-1])))
		}
		return provider.UsageDelta{Usage: domain.Usage{
			InputTokens:  input,
			OutputTokens: output,
		}}, nil
	}

	if !s.done {
		s.done = true
		if s.call.Tool != "" {
			// A turn that asked for a tool did not end; saying it did is how
			// a runtime stops before running the call.
			return provider.Completed{StopReason: domain.StopToolUse}, nil
		}
		return provider.Completed{StopReason: domain.StopEndTurn}, nil
	}

	return nil, io.EOF
}

func (s *stream) Close() error { return nil }
