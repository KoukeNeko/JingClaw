package runtime

import "context"

// ModelRequest is the normalized input to a provider. M0 carries only what the
// fake provider needs; context assembly, tools and system prompts arrive in M1.
type ModelRequest struct {
	LastUserText string
}

// ModelEvent is one item from a provider stream, already normalized. Provider
// SDK types must not appear here: a UI should never need to know whether the
// bytes came from Anthropic's SSE shape or OpenAI's Responses events.
type ModelEvent interface {
	isModelEvent()
}

// TextDelta is a chunk of assistant text.
type TextDelta struct {
	Text string
}

// Completed marks the end of a successful generation.
type Completed struct{}

func (TextDelta) isModelEvent() {}
func (Completed) isModelEvent() {}

// ModelStream yields ModelEvents until the generation ends or ctx is cancelled.
type ModelStream interface {
	// Recv returns the next event, or io.EOF once the stream is exhausted.
	Recv(ctx context.Context) (ModelEvent, error)

	// Close releases the stream. Callers must always call it.
	Close() error
}

// Provider is the single seam between the runtime and any language model.
//
// Everything downstream of it — persistence, permissions, tools, checkpoints,
// the UI protocol — is written against this interface, so adding a provider is
// an adapter, not an architectural change.
type Provider interface {
	Generate(ctx context.Context, req ModelRequest) (ModelStream, error)
}

// ProviderFunc adapts a plain function to Provider, for tests.
type ProviderFunc func(ctx context.Context, req ModelRequest) (ModelStream, error)

func (f ProviderFunc) Generate(ctx context.Context, req ModelRequest) (ModelStream, error) {
	return f(ctx, req)
}
