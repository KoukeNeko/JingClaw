package gateway

import (
	"encoding/json"
	"sort"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// RunSummary is what a finished run drew on and what it cost.
//
// Deliberately absent: any claim that a source was "not used". Nothing here
// can observe a model declining to rely on something it was shown. What can be
// observed is whether the content was still in front of the model when it
// wrote the answer, and that is what Retained reports. Presenting the
// difference as used against unused would be inventing a causal fact out of a
// structural one.
type RunSummary struct {
	// Provider and Model are who answered. Worth saying because it changes:
	// a deployment moved from a hosted model to a local one gets different
	// answers for reasons that have nothing to do with the prompt, and a
	// summary that omits this leaves somebody comparing two runs with no way
	// to tell they were not the same agent.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	Tools   []ToolUse `json:"tools,omitempty"`
	Sources []Source  `json:"sources,omitempty"`

	// SourcesOmitted counts sources left out of the list because it would
	// otherwise be longer than anybody will read. Saying how many were
	// dropped is the difference between a bounded list and a wrong one.
	SourcesOmitted int `json:"sources_omitted,omitempty"`

	InputTokens       int64 `json:"input_tokens,omitempty"`
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64 `json:"output_tokens,omitempty"`

	// Silent says the run ended having produced no answer and asked for no
	// tool.
	//
	// Worth saying because it is otherwise indistinguishable from success: the
	// run completes, the tokens are spent, and the channel is told how long it
	// took. A model that thought and then said nothing is a thing that
	// happens, and somebody watching should not have to work out that nothing
	// arrived.
	Silent bool `json:"silent,omitempty"`

	// Partial says this run began before the process did, so the tool and
	// source lists are known to be incomplete. Token counts are cumulative and
	// stay correct regardless.
	Partial bool `json:"partial,omitempty"`
}

// ToolUse is one tool and how often a run reached for it.
type ToolUse struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`

	// Failed counts the calls that returned an error. A run that read six
	// files and failed on four did something different from one that read six.
	Failed int `json:"failed,omitempty"`

	// Milliseconds is the time spent inside this tool across the run.
	//
	// Reported because it is usually where the time went. A run that took two
	// minutes and spent one of them in a single command is a different story
	// from one that spent two minutes waiting on the model, and a summary
	// giving only tokens cannot tell them apart.
	Milliseconds int64 `json:"milliseconds,omitempty"`

	// SlowestMS is the longest single call, which is what somebody looking
	// for the cause actually wants: six quick reads and one slow one average
	// into something that describes neither.
	SlowestMS int64 `json:"slowest_ms,omitempty"`
}

// Source is something a run brought in from outside its own reasoning.
type Source struct {
	// Kind is "web" for a fetched page and "file" for something in the
	// workspace. They are worth telling apart: one crossed the network and
	// one did not.
	Kind string `json:"kind"`

	Ref string `json:"ref"`

	// Retained says the content was still in the model's context when it
	// produced the final answer, rather than having been folded into a
	// summary by compaction along the way.
	//
	// This is not the same as the model having relied on it, and it is not the
	// opposite either: something folded into a summary may well have shaped the
	// answer through the summary. It says what can be proved from the log and
	// no more.
	Retained bool `json:"retained"`
}

// maxListedSources bounds the list. A run that reads forty files produces a
// message nobody finishes, and the count of the rest carries what matters.
const maxListedSources = 12

// runRecord accumulates what a run has done so far.
//
// Held in memory rather than read back from the log at the end. A run that was
// live when the process stopped is resolved as an orphan and never completes,
// so the only run that can outlive this record is one parked on an approval
// across a restart — which is why the summary says when it is partial rather
// than quietly reporting a smaller number as though it were the whole.
type runRecord struct {
	// seen says this record covers the run from its start.
	seen bool

	// waited says the run was seen in line. With seen still false when it
	// ends, it ended before it began: taken back, not stopped.
	waited bool

	tools map[string]*ToolUse

	// pending holds calls that have been requested but not yet finished, so a
	// source is only counted once the call it came from actually succeeded.
	pending map[domain.ToolCallID]pendingCall

	sources []recordedSource

	// said records whether the run produced any answer text at all.
	said bool

	// compactedThrough is the last event folded into a summary. A source
	// recorded at or before it is no longer in front of the model as itself.
	compactedThrough domain.Seq

	usage domain.Usage
}

type pendingCall struct {
	kind string
	ref  string
}

// recordedSource is a source plus where in the log it came from.
//
// The seq stays here rather than on Source because it is bookkeeping for
// deciding what compaction folded away, and a reader of the summary has no use
// for it.
type recordedSource struct {
	kind string
	ref  string
	seq  domain.Seq
}

func newRunRecord() *runRecord {
	return &runRecord{
		tools:   make(map[string]*ToolUse),
		pending: make(map[domain.ToolCallID]pendingCall),
	}
}

// requested notes a call, and what it would draw on if it succeeds.
func (r *runRecord) requested(payload domain.ToolCallRequested) {
	if kind, ref, ok := sourceOf(payload.Name, payload.Arguments); ok {
		r.pending[payload.CallID] = pendingCall{kind: kind, ref: ref}
	}
}

// completed records the outcome, and promotes a source only now.
//
// A fetch that failed is not a source. Counting the address the agent tried
// would put pages it never read into a list of what it drew on.
func (r *runRecord) completed(payload domain.ToolCallCompleted, seq domain.Seq) {
	use, ok := r.tools[payload.Name]
	if !ok {
		use = &ToolUse{Name: payload.Name}
		r.tools[payload.Name] = use
	}
	use.Calls++
	if payload.IsError {
		use.Failed++
	}
	use.Milliseconds += payload.DurationMS
	if payload.DurationMS > use.SlowestMS {
		use.SlowestMS = payload.DurationMS
	}

	call, waiting := r.pending[payload.CallID]
	delete(r.pending, payload.CallID)
	if !waiting || payload.IsError {
		return
	}

	for _, existing := range r.sources {
		if existing.kind == call.kind && existing.ref == call.ref {
			// Reading the same page twice is one source, not two.
			return
		}
	}
	r.sources = append(r.sources, recordedSource{kind: call.kind, ref: call.ref, seq: seq})
}

// summarise renders what was accumulated.
func (r *runRecord) summarise(provider, model string) RunSummary {
	summary := RunSummary{
		Provider:          provider,
		Model:             model,
		InputTokens:       r.usage.InputTokens,
		CachedInputTokens: r.usage.CachedInputTokens,
		OutputTokens:      r.usage.OutputTokens,
		Partial:           !r.seen,
		Silent:            r.seen && !r.said && len(r.tools) == 0,
	}

	for _, use := range r.tools {
		summary.Tools = append(summary.Tools, *use)
	}
	// Slowest first, then most-used, then by name. Somebody reading this is
	// usually asking where the time went, and the answer should be at the top.
	sort.Slice(summary.Tools, func(i, j int) bool {
		left, right := summary.Tools[i], summary.Tools[j]
		if left.Milliseconds != right.Milliseconds {
			return left.Milliseconds > right.Milliseconds
		}
		if left.Calls != right.Calls {
			return left.Calls > right.Calls
		}
		return left.Name < right.Name
	})

	for _, source := range r.sources {
		if len(summary.Sources) >= maxListedSources {
			summary.SourcesOmitted++
			continue
		}
		summary.Sources = append(summary.Sources, Source{
			Kind: source.kind,
			Ref:  source.ref,
			// Whether the content itself is still in front of the model, as
			// opposed to whatever a summary said about it.
			Retained: source.seq > r.compactedThrough,
		})
	}

	return summary
}

// sourceOf reports what a tool call would draw on, if anything.
//
// Only tools that bring in outside content count. A grep is a search over
// material already accounted for, and recall answers with memories this agent
// wrote itself, neither of which is a source in the sense a reader means when
// they ask where something came from.
func sourceOf(name, arguments string) (kind, ref string, ok bool) {
	var field string
	switch name {
	case "web_read":
		kind, field = "web", "url"
	case "read_file":
		kind, field = "file", "path"
	default:
		return "", "", false
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		return "", "", false
	}

	value, _ := decoded[field].(string)
	if value == "" {
		return "", "", false
	}
	return kind, value, true
}
