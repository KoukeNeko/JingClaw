package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// workerIterations is how far a delegated search may go.
//
// Lower than a conversation's, because the thing being asked for is bounded
// by construction: a question with an answer in the workspace. One that needs
// more than this is one the run should be doing itself, where a person can
// see it happening.
const workerIterations = 8

// ErrNotDelegatable says a run may not delegate.
var ErrNotDelegatable = errors.New("runtime: a delegated search may not delegate again")

// Investigate answers a bounded question in a context of its own.
//
// Deliberately not a general subagent. What delegation actually buys is
// context isolation — keeping a search's hundred tool results out of what the
// parent has to read again — and the rest of what a subagent framework offers
// costs more than it returns: measured against the same token budget, one
// agent matches or beats several.
//
// So this is narrow on purpose:
//
//   - a fresh conversation, and no sight of the parent's
//   - read-only tools, and none that could need an approval
//   - it cannot delegate in turn
//   - a hard ceiling on how far it may go
//   - it answers, and the answer is a tool result
//
// The child's whole trace is in the log under its own run. What it is not is
// part of the conversation: see delegatedRuns.
func (r *Runtime) Investigate(
	ctx context.Context, parent domain.RunID, question string,
) (string, error) {
	if strings.TrimSpace(question) == "" {
		return "", errors.New("runtime: a delegated search needs a question")
	}

	asking, err := r.opts.Store.Run(ctx, parent)
	if err != nil {
		return "", err
	}

	// One level. A worker that could delegate would turn a bounded search
	// into a tree nobody sized, and the containment is worth more than the
	// recursion: if depth two ever proves itself, this is the line to move.
	if asking.Kind == domain.RunWorker {
		return "", ErrNotDelegatable
	}

	child := domain.Run{
		ID:        domain.RunID(r.opts.NewRunID()),
		SessionID: asking.SessionID,
		Status:    domain.RunQueued,

		// The parent's origin, so what the child may do is bounded by what
		// the conversation may do. A worker that ran as something more
		// trusted than the turn that asked for it would be a way to launder
		// authority through delegation.
		Origin: asking.Origin,

		Kind:        domain.RunWorker,
		ParentRunID: parent,
		CreatedAt:   r.opts.Now(),
	}
	if err := r.opts.Store.CreateRun(ctx, child); err != nil {
		return "", err
	}

	// The question, as the child's whole conversation. Everything the parent
	// knows and did not say is not available to it, which is the test of
	// whether this was worth delegating: a question that cannot be asked in a
	// paragraph is one the parent should answer itself.
	if err := r.append(ctx, child.SessionID, child.ID, domain.EventUserMessageAdded,
		domain.UserMessageAdded{
			MessageID: domain.MessageID(r.opts.NewMessageID()),
			Text:      question,
			Trust:     trustForOrigin(asking.Origin),
			Origin:    asking.Origin,
		}); err != nil {
		return "", err
	}

	// Where the child's own events start. Read from here rather than from the
	// beginning: the session may hold a long conversation, and none of it is
	// the answer to this.
	before, err := r.opts.Store.Head(ctx, child.SessionID)
	if err != nil {
		return "", err
	}

	r.executeWorker(ctx, child)

	return r.workerAnswer(ctx, child, before)
}

// executeWorker runs the child to completion, in the calling goroutine.
//
// Synchronous because the parent is waiting on a tool result: there is no
// point returning to a model that has nothing to do until this finishes. It
// is registered as active so an interrupt of the session reaches it.
func (r *Runtime) executeWorker(ctx context.Context, child domain.Run) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	tracked := &activeRun{session: child.SessionID, cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.active[child.ID] = tracked
	r.mu.Unlock()
	defer r.releaseRun(child.ID, tracked)

	r.execute(runCtx, child)
}

// workerAnswer is what the child said, as the parent will see it.
//
// Read back out of the log rather than collected while the run went, because
// the log is what happened and a second copy of it in memory would be a
// second thing that could be wrong.
//
// The last message it finished, and only that. Everything before it is
// narration between tool calls — "let me look at that" — and joining it all
// together turns a run that never reached a conclusion into one that appears
// to have reported several.
func (r *Runtime) workerAnswer(
	ctx context.Context, child domain.Run, after domain.Seq,
) (string, error) {
	events, err := r.opts.Store.ListAfter(ctx, child.SessionID, after, 0)
	if err != nil {
		return "", err
	}

	said := map[domain.MessageID]*strings.Builder{}
	var answer string
	var ended domain.RunStateChanged

	for _, event := range events {
		if event.RunID != child.ID {
			continue
		}
		switch payload := event.Payload.(type) {
		case domain.AssistantTextDelta:
			text, ok := said[payload.MessageID]
			if !ok {
				text = &strings.Builder{}
				said[payload.MessageID] = text
			}
			text.WriteString(payload.Text)

		case domain.AssistantMessageCompleted:
			// Only a message it stopped on. One that ended in a tool call is
			// the middle of the work, whatever it happens to say.
			if payload.StopReason != domain.StopEndTurn {
				continue
			}
			if text, ok := said[payload.MessageID]; ok {
				answer = strings.TrimSpace(text.String())
			}

		case domain.RunStateChanged:
			if payload.Status.IsTerminal() {
				ended = payload
			}
		}
	}

	// A run that did not complete has no answer, whatever it wrote on the way.
	// The alternative is handing the parent a half-finished thought with
	// nothing to say it was one.
	if ended.Status == domain.RunCompleted && answer != "" {
		return answer, nil
	}

	// Why matters to whoever asked — a search that hit its ceiling is worth
	// narrowing and asking again, and one that failed on the provider is
	// worth waiting out — so pass the reason on rather than flattening every
	// ending into the same sentence.
	switch {
	case ended.Reason != "":
		return "", errors.New(ended.Reason)
	case ended.Status == domain.RunFailed:
		return "", errors.New("it failed without saying why")
	case ended.Status == "":
		return "", errors.New("it stopped before finishing")
	case ended.Status == domain.RunCompleted:
		return "", errors.New("it finished without answering")
	default:
		return "", fmt.Errorf("it %s without answering", ended.Status)
	}
}

// declarationsFor is the tools a run may call.
//
// A worker gets the ones that only read. Not as a policy that could be
// widened but as an absence: a tool it is never told about is one it cannot
// ask for, and there is then no approval to route to a person who is waiting
// on a tool call, and no model approving something for another model.
func (r *Runtime) declarationsFor(run domain.Run) []provider.ToolDeclaration {
	all := r.toolDeclarations()
	if run.Kind != domain.RunWorker {
		return all
	}

	kept := make([]provider.ToolDeclaration, 0, len(all))
	for _, declared := range all {
		if readOnlyForWorkers[declared.Name] {
			kept = append(kept, declared)
		}
	}
	return kept
}

// readOnlyForWorkers is what a delegated search may use.
//
// Named rather than derived from a tool's declared level. A level says what a
// tool is for; this says what somebody was willing to let a worker do, and
// deriving one from the other would silently widen this list every time a
// tool's level was reconsidered.
var readOnlyForWorkers = map[string]bool{
	"read_file":     true,
	"glob_files":    true,
	"grep":          true,
	"git_status":    true,
	"git_diff":      true,
	"read_artifact": true,
	"skill_load":    true,
}
