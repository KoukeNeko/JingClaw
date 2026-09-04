package daemon

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// modelCompleter asks the configured model one closed question.
//
// A bare completion: no tools, no conversation, no memories. The only thing
// the provider sees is what the caller hands over, so a caller that hands over
// a person's own words and nothing else has exposed nothing the run was not
// already sending.
type modelCompleter struct {
	provider provider.Provider
	model    string
}

func (c *modelCompleter) Complete(
	ctx context.Context, instruction, input string, maxOutputTokens int,
) (string, error) {
	stream, err := c.provider.Generate(ctx, provider.Request{
		Model:  c.model,
		System: provider.Text(instruction),
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: provider.Text(input),
		}},
		MaxOutputTokens: maxOutputTokens,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var answer strings.Builder
	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if delta, ok := event.(provider.TextDelta); ok {
			answer.WriteString(delta.Text)
		}
	}
	return answer.String(), nil
}
