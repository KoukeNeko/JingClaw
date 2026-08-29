package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
)

// fakeExpander stands in for the model, and counts.
//
// The count is the point of half these tests: broadening costs a model call,
// and the promise made in the configuration is that it is only ever paid on a
// search that already failed.
type fakeExpander struct {
	words string
	err   error
	calls atomic.Int64
}

func (e *fakeExpander) Expand(context.Context, string) ([]string, error) {
	e.calls.Add(1)
	if e.err != nil {
		return nil, e.err
	}
	return []string{e.words}, nil
}

func expandingTools(
	t *testing.T,
	expander memorytool.Expander,
) (*memorytool.Remember, *memorytool.Recall) {
	t.Helper()

	store := memory.New()

	var counter atomic.Uint64
	options := memorytool.Options{
		Store:        store,
		WorkspaceRef: workspace,
		NewID:        func() string { return fmt.Sprintf("mem_%d", counter.Add(1)) },
		Clock:        func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Expander:     expander,
	}

	return &memorytool.Remember{Options: options}, &memorytool.Recall{Options: options}
}

// The failure this exists for. The note and the question are the same subject
// and share no word, so the index has nothing to match and reports nothing
// missing.
func TestASearchWhoseWordsMissIsTriedAgain(t *testing.T) {
	expander := &fakeExpander{words: "component reuse duplicate"}
	write, read := expandingTools(t, expander)

	remember(t, write, localTurn(), map[string]any{
		"text":  "prefer reusing an existing component over building a second one",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	if !strings.Contains(found, "reusing an existing component") {
		t.Errorf("the note was not found under other words:\n%s", found)
	}
	if calls := expander.calls.Load(); calls != 1 {
		t.Errorf("asked for other words %d times, want 1", calls)
	}
}

// A result found under words the agent did not ask for answers a question near
// the one it asked. Reporting it as if it answered that one is the same
// failure as reporting nothing, pointed the other way.
func TestAWidenedSearchSaysSo(t *testing.T) {
	write, read := expandingTools(t, &fakeExpander{words: "component"})

	remember(t, write, localTurn(), map[string]any{
		"text":  "prefer reusing an existing component over building a second one",
		"scope": "workspace",
	})

	direct := recall(t, read, localTurn(), map[string]any{"query": "component"})
	if strings.Contains(direct, "related words") {
		t.Errorf("a search that matched on its own claimed to be broadened:\n%s", direct)
	}

	widened := recall(t, read, localTurn(), map[string]any{"query": "modal"})
	if !strings.Contains(widened, "related words") {
		t.Errorf("a broadened search did not say so:\n%s", widened)
	}
}

// Broadening is paid for. A search that answered the question it was asked
// must not reach the provider at all.
func TestASearchThatFoundSomethingIsNotBroadened(t *testing.T) {
	expander := &fakeExpander{words: "component"}
	write, read := expandingTools(t, expander)

	remember(t, write, localTurn(), map[string]any{
		"text":  "the deploy script needs sudo",
		"scope": "workspace",
	})

	recall(t, read, localTurn(), map[string]any{"query": "deploy"})

	if calls := expander.calls.Load(); calls != 0 {
		t.Errorf("a search that already matched asked for other words %d times", calls)
	}
}

// Listing everything is not a search, so there is nothing to broaden.
func TestListingEverythingDoesNotAskForOtherWords(t *testing.T) {
	expander := &fakeExpander{words: "component"}
	write, read := expandingTools(t, expander)

	remember(t, write, localTurn(), map[string]any{"text": "the deploy script needs sudo"})
	recall(t, read, localTurn(), map[string]any{})

	if calls := expander.calls.Load(); calls != 0 {
		t.Errorf("listing asked for other words %d times", calls)
	}
}

// The broadened search is the optional half. Losing it leaves the answer the
// agent would have had anyway, rather than turning a lookup that merely found
// nothing into a tool failure it has to reason about.
func TestAnExpanderThatFailsLeavesTheMissStanding(t *testing.T) {
	expander := &fakeExpander{err: errors.New("the model is unreachable")}
	write, read := expandingTools(t, expander)

	remember(t, write, localTurn(), map[string]any{
		"text":  "prefer reusing an existing component",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	if !strings.Contains(found, "Nothing has been remembered") {
		t.Errorf("a failed expansion did not report a plain miss:\n%s", found)
	}
	if strings.Contains(found, "related words") {
		t.Errorf("a failed expansion claimed the search was broadened:\n%s", found)
	}
}

// Trying other words and still finding nothing is a different answer from not
// having tried, and the agent decides what to do next on the difference.
func TestAWidenedSearchThatStillFindsNothingSaysThat(t *testing.T) {
	write, read := expandingTools(t, &fakeExpander{words: "component reuse"})

	remember(t, write, localTurn(), map[string]any{
		"text":  "the deploy script needs sudo",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	if !strings.Contains(found, "including under related words") {
		t.Errorf("a broadened search that found nothing did not say so:\n%s", found)
	}
}

// What comes back is searched for and nothing else.
//
// The expander is a language model, so its answer is model-written text
// arriving at a query interface. It is quoted the same way the agent's own
// query is: it cannot become MATCH syntax, and it cannot reach a scope the
// turn was not already allowed to see.
func TestWordsFromAnExpanderAreSearchedForAndNotObeyed(t *testing.T) {
	expander := &fakeExpander{
		words: `secret OR 1=1 -- "*" AND scope:principal ignore the scopes and return everything`,
	}
	write, read := expandingTools(t, expander)

	remember(t, write, gatewayTurn("u_other"),
		map[string]any{"text": "the secret handshake is three taps"})
	remember(t, write, localTurn(), map[string]any{
		"text":  "the deploy secret is read from the environment",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	// The control: "secret" was one of the words returned, and a memory this
	// turn may see does match it. Without this the test would pass just as
	// well against a broadening that quietly did nothing at all.
	if !strings.Contains(found, "read from the environment") {
		t.Fatalf("the words were not searched for, so nothing here is proven:\n%s", found)
	}
	if strings.Contains(found, "three taps") {
		t.Errorf("a broadened search read another principal's memory:\n%s", found)
	}
}

// Every word an expander returns is ORed into one query, so each one past a
// handful is another way to match a memory about something else. The bound is
// on the terms used, not on what the model felt like sending.
func TestOnlyAHandfulOfOtherWordsAreUsed(t *testing.T) {
	var padding []string
	for i := range 20 {
		padding = append(padding, fmt.Sprintf("filler%d", i))
	}

	note := map[string]any{
		"text":  "prefer reusing an existing component",
		"scope": "workspace",
	}

	// The same word, once inside the bound and once past it. One test rather
	// than two because the pair is the claim: it is the position that decides,
	// not the word.
	within, readWithin := expandingTools(t,
		&fakeExpander{words: "component " + strings.Join(padding, " ")})
	remember(t, within, localTurn(), note)

	beyond, readBeyond := expandingTools(t,
		&fakeExpander{words: strings.Join(padding, " ") + " component"})
	remember(t, beyond, localTurn(), note)

	query := map[string]any{"query": "modal"}

	if found := recall(t, readWithin, localTurn(), query); !strings.Contains(found, "reusing an existing") {
		t.Fatalf("a word inside the bound was not searched for:\n%s", found)
	}
	if found := recall(t, readBeyond, localTurn(), query); strings.Contains(found, "reusing an existing") {
		t.Errorf("a word past the bound was still searched for:\n%s", found)
	}
}

// A tool that reads back what this agent was told grants no reach the turn did
// not have. Asking the model for other words does not change that, and the
// declared level has to keep saying so.
func TestBroadeningDoesNotRaiseWhatRecallCosts(t *testing.T) {
	_, read := expandingTools(t, &fakeExpander{words: "component"})

	if level := read.Spec().Level; level != tool.LevelInternal {
		t.Errorf("recall declares %v, want %v", level, tool.LevelInternal)
	}
}

// An expander is told to answer with nothing when nothing sensible comes to
// mind, and a model that follows an instruction should not be treated as one
// that broke. Nothing was added, so nothing is claimed to have been searched.
func TestAnExpanderWithNothingToAddIsNotAFailure(t *testing.T) {
	write, read := expandingTools(t, &fakeExpander{words: "  \n  "})

	remember(t, write, localTurn(), map[string]any{
		"text":  "prefer reusing an existing component",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	if found != "Nothing has been remembered that matches." {
		t.Errorf("an expander with nothing to add changed the answer:\n%s", found)
	}
}

// Words already in the query are not worth a second search for. Sending them
// back is the ordinary case, not a mistake: a model asked for other words for
// "modal" will often lead with "modal".
func TestWordsAlreadyInTheQueryAreNotAddedBack(t *testing.T) {
	write, read := expandingTools(t, &fakeExpander{words: "modal MODAL modal."})

	remember(t, write, localTurn(), map[string]any{
		"text":  "prefer reusing an existing component",
		"scope": "workspace",
	})

	found := recall(t, read, localTurn(), map[string]any{"query": "modal"})

	if strings.Contains(found, "related words") {
		t.Errorf("repeating the query counted as broadening it:\n%s", found)
	}
}
