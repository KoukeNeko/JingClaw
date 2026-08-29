package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// expandInstruction asks for vocabulary and nothing else.
//
// Written as a closed task with a stated output shape because the answer is
// parsed, not read. It says "words" rather than "synonyms" deliberately: what
// is wanted is the vocabulary a note about the same subject would plausibly
// have been written in, which is broader than synonymy and is the gap that
// makes a word index miss.
const expandInstruction = `You are helping search a small collection of notes ` +
	`an assistant wrote about a project and the person it works for.

A search for the words below found nothing. List other words that a note about
the same subject might have been written in: broader terms, the general idea a
specific thing is an instance of, and the ordinary name for a technical one.

Answer with the words only, separated by spaces, on a single line. No more than
eight. No numbering, no punctuation, no explanation. If nothing sensible comes
to mind, answer with nothing at all.`

// expandMaxOutputTokens bounds the answer. Eight words needs a fraction of
// this; the room is for a model that cannot resist a preamble, whose prose is
// discarded when the answer is parsed.
const expandMaxOutputTokens = 64

// modelExpander asks the configured model for other words to search for.
//
// It is a bare completion: no tools, no conversation, no memories. The only
// thing it is given is the query the agent already wrote, so this exposes
// nothing to the provider that the run was not already sending it.
type modelExpander struct {
	provider provider.Provider
	model    string
}

func (e *modelExpander) Expand(ctx context.Context, query string) ([]string, error) {
	stream, err := e.provider.Generate(ctx, provider.Request{
		Model:  e.model,
		System: provider.Text(expandInstruction),
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: provider.Text(query),
		}},
		MaxOutputTokens: expandMaxOutputTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("ask for other words: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var answer strings.Builder
	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read other words: %w", err)
		}
		if delta, ok := event.(provider.TextDelta); ok {
			answer.WriteString(delta.Text)
		}
	}

	// Returned whole rather than split here. What counts as a usable term is
	// the searcher's business, and it applies the same rules to every
	// expander instead of trusting each one to have split its own answer
	// the way the index needs.
	return []string{answer.String()}, nil
}
