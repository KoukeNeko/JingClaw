package runtime_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

func newPlanRuntime(t *testing.T) (*runtime.Runtime, *memory.Store) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := memory.New()

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	var steps atomic.Uint64
	return runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      fake.New(0),
		Model:         fake.ModelID,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: func() string { return fmt.Sprintf("todo_%d", steps.Add(1)) },
		Now:           time.Now,
		Logger:        slog.New(slog.DiscardHandler),
	}), store
}

func add(title string) domain.PlanOpRequest {
	return domain.PlanOpRequest{Op: domain.PlanOpAdd, Title: title}
}

func setStatus(id string, status domain.PlanStatus) domain.PlanOpRequest {
	return domain.PlanOpRequest{Op: domain.PlanOpSetStatus, ID: id, Status: status}
}

// Adding a step must not disturb the ones already there. A tool that took the
// whole list would let a model drop an id it did not think mattered, and
// nothing downstream could tell that from a step being finished.
func TestAddingAStepLeavesTheOthersAlone(t *testing.T) {
	rt, _ := newPlanRuntime(t)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "planning")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		add("read the failing test"), add("fix it"),
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		setStatus("todo_1", domain.PlanCompleted),
	}); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	items, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{add("run the suite")})
	if err != nil {
		t.Fatalf("add a third: %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("the plan has %d steps, want 3: %+v", len(items), items)
	}
	if items[0].Status != domain.PlanCompleted {
		t.Error("the finished step was revived by adding another")
	}
	if items[0].ID != "todo_1" || items[1].ID != "todo_2" || items[2].ID != "todo_3" {
		t.Errorf("ids moved: %+v", items)
	}
}

// A step that is not there is a model that has lost track of its own plan.
// Doing nothing quietly would let it go on believing the step was marked.
func TestNamingAStepThatIsNotThereIsAnError(t *testing.T) {
	rt, _ := newPlanRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "planning")
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{add("one")}); err != nil {
		t.Fatalf("add: %v", err)
	}

	_, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		setStatus("todo_99", domain.PlanCompleted),
	})
	if err == nil {
		t.Fatal("marking a step that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "todo_99") {
		t.Errorf("the error does not say which step: %v", err)
	}
}

// A status the model invented must be refused rather than stored. A step in a
// state nothing recognises is one every client draws differently.
func TestAnInventedStatusIsRefused(t *testing.T) {
	rt, _ := newPlanRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "planning")
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{add("one")}); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		setStatus("todo_1", "nearly"),
	}); err == nil {
		t.Error("a status nothing recognises was stored")
	}
}

// Every change announces the whole plan. A client that joined late reads one
// event and knows where things stand, rather than replaying every change.
func TestEachChangeAnnouncesTheWholePlan(t *testing.T) {
	rt, store := newPlanRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "planning")
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		add("one"), add("two"),
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		setStatus("todo_1", domain.PlanCompleted),
	}); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	var announcements []domain.PlanChanged
	for _, one := range events {
		if changed, ok := one.Payload.(domain.PlanChanged); ok {
			announcements = append(announcements, changed)
		}
	}

	if len(announcements) != 2 {
		t.Fatalf("%d plan events for two changes", len(announcements))
	}
	last := announcements[len(announcements)-1]
	if len(last.Items) != 2 {
		t.Fatalf("the last announcement carries %d steps, want the whole plan", len(last.Items))
	}
	if last.Items[0].Status != domain.PlanCompleted {
		t.Error("the announcement does not carry the change it is announcing")
	}
}

// The plan survives a restart. One that did not would be forgotten every time
// the daemon was updated, which is exactly when somebody is watching.
func TestThePlanSurvivesInTheStore(t *testing.T) {
	rt, store := newPlanRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "planning")
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		add("one"), setStatus("todo_1", domain.PlanInProgress),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Read from the store rather than from the runtime, which is what a new
	// process would do.
	items, err := store.Plan(ctx, session.ID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(items) != 1 || items[0].Status != domain.PlanInProgress {
		t.Errorf("the plan did not survive: %+v", items)
	}
}

// A plan is a plan, not the work. A model that writes forty steps has
// enumerated rather than planned, and the list is then too long to be read by
// anybody it was written for.
func TestAPlanIsBounded(t *testing.T) {
	rt, _ := newPlanRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "planning")

	ops := make([]domain.PlanOpRequest, 0, 40)
	for i := 0; i < 40; i++ {
		ops = append(ops, add(fmt.Sprintf("step %d", i)))
	}

	if _, err := rt.UpdatePlan(ctx, session.ID, ops); err == nil {
		t.Fatal("forty steps were accepted")
	}
}

// A plan belongs to its session. One that leaked would have the agent working
// from another conversation's list.
func TestAPlanBelongsToItsSession(t *testing.T) {
	rt, store := newPlanRuntime(t)
	ctx := context.Background()

	mine, _ := rt.CreateSession(ctx, "mine")
	theirs, _ := rt.CreateSession(ctx, "theirs")

	if _, err := rt.UpdatePlan(ctx, mine.ID, []domain.PlanOpRequest{add("mine")}); err != nil {
		t.Fatalf("update: %v", err)
	}

	other, err := store.Plan(ctx, theirs.ID)
	if err != nil {
		t.Fatalf("read the other plan: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("the plan leaked: %+v", other)
	}
}

// The plan has to reach the model, which is the whole point of it being state
// rather than prose. A plan the model cannot see is one it has to reconstruct
// from its own earlier output — which is the thing this exists to replace.
func TestThePlanIsPutInFrontOfTheModel(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "planning")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		add("read the failing test"),
		add("fix the timeout"),
	}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := rt.UpdatePlan(ctx, session.ID, []domain.PlanOpRequest{
		setStatus("todo_1", domain.PlanCompleted),
	}); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "carry on"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var prompt string
	for time.Now().Before(deadline) {
		if prompts := watching.prompts(); len(prompts) > 0 {
			prompt = prompts[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if prompt == "" {
		t.Fatal("the model was never asked anything")
	}

	for _, want := range []string{
		"read the failing test", "fix the timeout",
		"todo_1", "todo_2", "done", "todo_update",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the model was not shown %q:\n%s", want, prompt)
		}
	}
}

// A session that never planned must not be told about a plan. An empty
// "here is your plan" section is instructions to use a tool for nothing.
func TestASessionWithNoPlanIsToldNothingAboutOne(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "no plan")
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var prompt string
	var asked bool
	for time.Now().Before(deadline) {
		if prompts := watching.prompts(); len(prompts) > 0 {
			prompt, asked = prompts[0], true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !asked {
		t.Fatal("the model was never asked anything")
	}
	if strings.Contains(prompt, "Your plan for this session") {
		t.Errorf("a session with no plan was shown one:\n%s", prompt)
	}
}
