package openaicompat

import (
	"encoding/json"
	"sort"
	"strings"
)

// toolAssembly collects one tool call from the fragments it arrives in.
//
// A call is delivered across frames: the first carries an id and a name, and
// every frame after it carries only an index and a slice of the arguments
// string. Those slices are not individually valid JSON — one ends mid-key —
// so nothing can be decided about a call until the stream says it is done.
type toolAssembly struct {
	index int

	id        string
	name      string
	arguments strings.Builder
}

// toolAccumulator assembles the calls of one response.
//
// Keyed by choice and then by the index within it, because calls are
// interleaved when a model asks for several at once: fragments arrive for
// index 0, then 1, then 0 again. Assuming one call finishes before the next
// begins produces arguments belonging to two different calls concatenated into
// one.
type toolAccumulator struct {
	byChoice map[int]map[int]*toolAssembly
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byChoice: make(map[int]map[int]*toolAssembly)}
}

// add folds one fragment in.
func (a *toolAccumulator) add(choice int, fragment wireToolCall) {
	calls, ok := a.byChoice[choice]
	if !ok {
		calls = make(map[int]*toolAssembly)
		a.byChoice[choice] = calls
	}

	// An absent index means a server that sends one call at a time without
	// numbering it. Treating that as index zero is right for the single-call
	// case and no worse than any alternative for the rest.
	index := 0
	if fragment.Index != nil {
		index = *fragment.Index
	}

	assembly, ok := calls[index]
	if !ok {
		assembly = &toolAssembly{index: index}
		calls[index] = assembly
	}

	// The id and name arrive once. Later fragments carry them as empty, and
	// overwriting would erase what was already learned.
	if fragment.ID != "" {
		assembly.id = fragment.ID
	}
	if fragment.Function.Name != "" {
		assembly.name = fragment.Function.Name
	}
	assembly.arguments.WriteString(fragment.Function.Arguments)
}

// take returns the finished calls for a choice and forgets them.
//
// Called when the stream says the response is done, never earlier. Emitting on
// the first fragment would hand the runtime a call whose arguments are half a
// key.
func (a *toolAccumulator) take(choice int) []assembledCall {
	calls, ok := a.byChoice[choice]
	if !ok {
		return nil
	}
	delete(a.byChoice, choice)

	assembled := make([]assembledCall, 0, len(calls))
	for _, call := range calls {
		assembled = append(assembled, assembledCall{
			Index:     call.index,
			ID:        call.id,
			Name:      call.name,
			Arguments: normalizeArguments(call.arguments.String()),
		})
	}

	// In the order the model asked for them, which is the order they should
	// run in.
	sort.Slice(assembled, func(i, j int) bool { return assembled[i].Index < assembled[j].Index })
	return assembled
}

// takeAll returns everything still held, for a stream that ended without
// saying so.
func (a *toolAccumulator) takeAll() []assembledCall {
	choices := make([]int, 0, len(a.byChoice))
	for choice := range a.byChoice {
		choices = append(choices, choice)
	}
	sort.Ints(choices)

	var assembled []assembledCall
	for _, choice := range choices {
		assembled = append(assembled, a.take(choice)...)
	}
	return assembled
}

func (a *toolAccumulator) empty() bool { return len(a.byChoice) == 0 }

type assembledCall struct {
	Index     int
	ID        string
	Name      string
	Arguments json.RawMessage
}

// normalizeArguments turns the assembled string into something the runtime can
// hand to a tool.
//
// A model that takes no arguments sends nothing at all rather than "{}", and a
// tool receiving empty bytes fails to unmarshal for a reason that has nothing
// to do with what it was asked. Anything that does not parse is passed through
// untouched: the tool's own validation produces a better message than a guess
// made here, and rewriting invalid arguments would hide that the model
// produced them.
func normalizeArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}
