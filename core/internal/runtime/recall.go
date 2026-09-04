package runtime

import (
	"context"
	"slices"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// withRecalled puts what this machine noted in earlier conversations in front
// of the turn being answered.
//
// Only that turn. The conversation is rebuilt from the log on every request
// and the earlier turns in it are history; a label put on one of those would
// edit history on the way out and change the prefix a provider is paid to
// remember. The latest turn is where the label can go, because everything a
// provider has cached ends before it.
func (r *Runtime) withRecalled(
	ctx context.Context, run domain.Run, messages []provider.Message,
) []provider.Message {
	if r.opts.Recall == nil || run.Kind == domain.RunWorker {
		return messages
	}

	latest := lastPersonTurn(messages)
	if latest < 0 {
		return messages
	}

	label := r.recalledFor(ctx, run, spokenText(messages[latest]))
	if label == "" {
		return messages
	}

	labelled := slices.Clone(messages)
	labelled[latest] = withLabel(messages[latest], label)
	return labelled
}

// recalledFor asks once per run and keeps the answer.
//
// Every request the run makes — one per model turn in the tool loop — has to
// carry the same label, or the turn changes under the calls and results that
// follow it and the provider's cache of them is lost for nothing. A run
// resumed by a restarted daemon asks again, which changes the label once;
// that is the price of not holding the answer anywhere but here.
func (r *Runtime) recalledFor(ctx context.Context, run domain.Run, said string) string {
	r.mu.Lock()
	tracked := r.active[run.ID]
	if tracked != nil && tracked.recalled != nil {
		defer r.mu.Unlock()
		return *tracked.recalled
	}
	r.mu.Unlock()

	label := r.opts.Recall(ctx, run, said)

	r.mu.Lock()
	defer r.mu.Unlock()
	if tracked != nil {
		tracked.recalled = &label
	}
	return label
}

// lastPersonTurn is the index of the last message somebody sent, or -1.
func lastPersonTurn(messages []provider.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser && spokenText(messages[i]) != "" {
			return i
		}
	}
	return -1
}

// spokenText is what a turn says, without what this machine wrote around it.
func spokenText(message provider.Message) string {
	var said strings.Builder
	for _, block := range message.Content {
		if text, ok := block.(provider.TextBlock); ok && !text.Annotation {
			said.WriteString(text.Text)
		}
	}
	return strings.TrimSpace(said.String())
}

// withLabel puts a label in front of the person's words, after whatever this
// machine already wrote there: the line saying when and by whom stays first,
// and the words themselves stay last, as their own block.
func withLabel(message provider.Message, label string) provider.Message {
	content := make([]provider.ContentBlock, 0, len(message.Content)+1)
	placed := false
	for _, block := range message.Content {
		if text, ok := block.(provider.TextBlock); !placed && (!ok || !text.Annotation) {
			content = append(content, provider.TextBlock{Text: label, Annotation: true})
			placed = true
		}
		content = append(content, block)
	}
	if !placed {
		content = append(content, provider.TextBlock{Text: label, Annotation: true})
	}
	return provider.Message{Role: message.Role, Content: content}
}
