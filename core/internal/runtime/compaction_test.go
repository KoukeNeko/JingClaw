package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// theSummary is deliberately unlike anything else in these conversations, so
// finding it in a request proves where the history came from.
const theSummary = "SUMMARY-OF-WHAT-CAME-BEFORE"

// compactingProvider answers two different questions: the summarisation
// request the runtime makes when a session is too large, and the ordinary
// turns. Which one it is being asked is read from the system prompt rather
// than from a call counter, so the test does not depend on how many times
// compaction happens to fire.
type compactingProvider struct {
	mu sync.Mutex

	requests    []provider.Request
	summaries   int
	summaryFail error
	reply       string
}

func (p *compactingProvider) Name() string { return "compacting" }

func (p *compactingProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (p *compactingProvider) Generate(_ context.Context, req provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)

	if isSummarisation(req) {
		p.summaries++
		if p.summaryFail != nil {
			return nil, p.summaryFail
		}
		return &scriptedStream{events: []provider.Event{
			provider.TextDelta{Text: theSummary},
			provider.Completed{},
		}}, nil
	}

	return &scriptedStream{events: []provider.Event{
		provider.TextDelta{Text: p.reply},
		provider.Completed{},
	}}, nil
}

func isSummarisation(req provider.Request) bool {
	for _, block := range req.System {
		if text, ok := block.(provider.TextBlock); ok &&
			strings.Contains(text.Text, "compacting a working session") {
			return true
		}
	}
	return false
}

// turnRequests returns the ordinary requests, without the summarisation ones.
func (p *compactingProvider) turnRequests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()

	var turns []provider.Request
	for _, req := range p.requests {
		if !isSummarisation(req) {
			turns = append(turns, req)
		}
	}
	return turns
}

func (p *compactingProvider) summaryCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.summaries
}

// newCompactionHarness builds a runtime with no tools and a small window, so
// the only thing that grows the conversation is what the test sends.
func newCompactionHarness(
	t *testing.T,
	store *memory.Store,
	model *compactingProvider,
	window int64,
) *runtime.Runtime {
	t.Helper()

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return runtime.New(ctx, runtime.Options{
		Store:    store,
		Hub:      event.NewHub(),
		Provider: model,
		Model:    "compacting",
		ContextBudget: runtime.ContextBudget{
			Window:       window,
			CompactAt:    0.7,
			KeepFraction: 0.3,
		},
		SystemPrompt:  "you are an agent",
		MaxIterations: 5,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: next("todo"),
		NewQuestionID: next("qst"),
		NewScheduleID: next("sch"),
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:        slog.New(slog.DiscardHandler),
	})
}

func requestText(req provider.Request) string {
	var all strings.Builder
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if text, ok := block.(provider.TextBlock); ok {
				all.WriteString(text.Text)
			}
		}
	}
	return all.String()
}

func countEvents(t *testing.T, store *memory.Store, sessionID domain.SessionID, kind domain.EventKind) int {
	t.Helper()

	events, err := store.ListAfter(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	found := 0
	for _, ev := range events {
		if ev.Kind == kind {
			found++
		}
	}
	return found
}

// A session that goes on long enough must keep working. This is the whole
// point: the request stops growing, and what was dropped is represented rather
// than silently gone.
func TestLongSessionIsCompactedAndKeepsWorking(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}
	rt := newCompactionHarness(t, store, model, 2000)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const firstTurn = "FIRST-THING-EVER-ASKED"
	runTurn(t, rt, session.ID, firstTurn+" "+strings.Repeat("x", 3000))
	for turn := range 4 {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", 3000))
	}

	if compactions := countEvents(t, store, session.ID, "conversation.compacted"); compactions == 0 {
		t.Fatal("a session well past the window was never compacted")
	}

	turns := model.turnRequests()
	last := requestText(turns[len(turns)-1])

	if !strings.Contains(last, theSummary) {
		t.Error("the last request does not carry the summary")
	}
	if strings.Contains(last, firstTurn) {
		t.Error("the last request still replays the very first turn verbatim")
	}
}

// The property that matters is boundedness, not that any one request is
// smaller than the last: a session of any length has to keep producing
// requests that fit, however much has been said.
func TestRequestsStayBoundedHoweverLongTheSession(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}

	const (
		window   = 2000
		turnSize = 3000
		turns    = 8
	)
	rt := newCompactionHarness(t, store, model, window)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	for turn := range turns {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", turnSize))
	}

	// Four characters to a token is the estimate the runtime itself uses, so
	// the whole window is the ceiling a request may not pass.
	ceiling := window * 4

	sent := model.turnRequests()
	if len(sent) < turns {
		t.Fatalf("only %d of %d turns reached the model", len(sent), turns)
	}
	for i, req := range sent {
		if size := len(requestText(req)); size > ceiling {
			t.Errorf("request %d is %d characters, past the %d the window allows", i, size, ceiling)
		}
	}

	// And without compaction it would have been far past it, which is what
	// makes the bound above worth asserting.
	if turns*turnSize <= ceiling {
		t.Fatal("the test does not send enough to exceed the window")
	}
}

// Compaction is durable because it is an event. A process that restarted and
// replayed the history the summary replaced would defeat the whole mechanism.
func TestCompactionSurvivesRestart(t *testing.T) {
	store := memory.New()

	const firstTurn = "FIRST-THING-EVER-ASKED"

	first := newCompactionHarness(t, store, &compactingProvider{reply: "ok"}, 2000)
	session, err := first.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runTurn(t, first, session.ID, firstTurn+" "+strings.Repeat("x", 3000))
	for turn := range 4 {
		runTurn(t, first, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", 3000))
	}
	if countEvents(t, store, session.ID, "conversation.compacted") == 0 {
		t.Fatal("nothing was compacted before the restart")
	}

	// A new runtime over the same log, as a restarted daemon would be.
	secondModel := &compactingProvider{reply: "ok"}
	second := newCompactionHarness(t, store, secondModel, 2000)
	runTurn(t, second, session.ID, "after the restart")

	turns := secondModel.turnRequests()
	if len(turns) == 0 {
		t.Fatal("the restarted runtime sent nothing")
	}
	sent := requestText(turns[0])

	if !strings.Contains(sent, theSummary) {
		t.Error("the restarted runtime did not start from the summary")
	}
	if strings.Contains(sent, firstTurn) {
		t.Error("the restarted runtime replayed history the summary had already replaced")
	}
}

// Summarising costs a model call, so a session with room to grow must not be
// made to pay for one.
func TestNothingIsCompactedWhileThereIsRoom(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}
	rt := newCompactionHarness(t, store, model, 200_000)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	for turn := range 3 {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d", turn))
	}

	if model.summaryCount() != 0 {
		t.Errorf("the model was asked for %d summaries with the window barely touched",
			model.summaryCount())
	}
	if compactions := countEvents(t, store, session.ID, "conversation.compacted"); compactions != 0 {
		t.Errorf("%d compactions were recorded with the window barely touched", compactions)
	}
}

// A window nobody could determine leaves history alone. Summarising against a
// guessed one would either throw work away early or fail to save the session
// that needed saving.
func TestNothingIsCompactedWhenTheWindowIsUnknown(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}
	rt := newCompactionHarness(t, store, model, 0)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	for turn := range 4 {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", 3000))
	}

	if model.summaryCount() != 0 {
		t.Errorf("compaction ran against an unknown window %d times", model.summaryCount())
	}
}

// A provider hiccup while summarising must not end the run and must not lose
// anything: the history is still in the log, and the request is simply the one
// that would have been sent anyway.
func TestAFailedSummaryLosesNothing(t *testing.T) {
	store := memory.New()

	const firstTurn = "FIRST-THING-EVER-ASKED"

	model := &compactingProvider{reply: "ok", summaryFail: errors.New("provider unavailable")}
	rt := newCompactionHarness(t, store, model, 2000)

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runTurn(t, rt, session.ID, firstTurn+" "+strings.Repeat("x", 3000))
	for turn := range 3 {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", 3000))
	}

	if model.summaryCount() == 0 {
		t.Fatal("the test never reached a summarisation attempt")
	}
	if compactions := countEvents(t, store, session.ID, "conversation.compacted"); compactions != 0 {
		t.Errorf("%d compactions were recorded despite every summary failing", compactions)
	}

	runs, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range runs {
		if state, ok := ev.Payload.(domain.RunStateChanged); ok && state.Status == domain.RunFailed {
			t.Fatalf("a run failed because a summary could not be produced: %s", state.Reason)
		}
	}

	turns := model.turnRequests()
	if !strings.Contains(requestText(turns[len(turns)-1]), firstTurn) {
		t.Error("history went missing even though nothing was compacted")
	}
}

// A run resumed later must be given the prompt it started with.
//
// Standing directions come from memory, and memory changes. Recomputing them
// on a resume — possibly in another process, hours after the approval — would
// mean the same run was given two different prompts and neither the log nor
// anybody reading it could say which.
func TestARunKeepsTheDirectionsItStartedWith(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}

	directions := "- the first answer"
	rt := newDirectedHarness(t, store, model, func() string { return directions })

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "hello")

	first := model.turnRequests()
	if len(first) == 0 {
		t.Fatal("nothing was sent")
	}
	if !strings.Contains(systemOf(first[0]), "the first answer") {
		t.Fatalf("the directions never reached the prompt: %q", systemOf(first[0]))
	}

	// Memory changes, and a new run gets the new answer.
	directions = "- the second answer"
	runTurn(t, rt, session.ID, "again")

	sent := model.turnRequests()
	if !strings.Contains(systemOf(sent[len(sent)-1]), "the second answer") {
		t.Error("a later run did not pick up the changed directions")
	}

	// But replaying the first run reads what it was actually given, rather
	// than assembling it again.
	events, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	recorded := 0
	for _, event := range events {
		if payload, ok := event.Payload.(domain.RunDirections); ok {
			recorded++
			if payload.Text == "" {
				t.Error("a run recorded empty directions")
			}
		}
	}
	if recorded != 2 {
		t.Errorf("%d runs recorded their directions, want 2", recorded)
	}
}

// With nothing to say, nothing is recorded: an event per run carrying an empty
// string is noise in the one place that has to stay readable.
func TestARunWithNoDirectionsRecordsNothing(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}
	rt := newDirectedHarness(t, store, model, func() string { return "" })

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "hello")

	events, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		if _, ok := event.Payload.(domain.RunDirections); ok {
			t.Error("a run with nothing to say recorded directions anyway")
		}
	}
}

func systemOf(req provider.Request) string {
	var text strings.Builder
	for _, block := range req.System {
		if content, ok := block.(provider.TextBlock); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String()
}

func newDirectedHarness(
	t *testing.T,
	store *memory.Store,
	model *compactingProvider,
	directions func() string,
) *runtime.Runtime {
	t.Helper()

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return runtime.New(ctx, runtime.Options{
		Store:           store,
		Hub:             event.NewHub(),
		Provider:        model,
		Model:           "directed",
		SystemPrompt:    "you are an agent",
		SystemPromptFor: func(context.Context, domain.Run) string { return directions() },
		MaxIterations:   5,
		NewSessionID:    next("ses"),
		NewRunID:        next("run"),
		NewMessageID:    next("msg"),
		NewEventID:      next("evt"),
		NewApprovalID:   next("apr"),
		NewPlanItemID:   next("todo"),
		NewQuestionID:   next("qst"),
		NewScheduleID:   next("sch"),
		Now:             func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:          slog.New(slog.DiscardHandler),
	})
}
