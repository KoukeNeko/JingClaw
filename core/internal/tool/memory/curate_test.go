package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	memstore "github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

const (
	theSession = domain.SessionID("ses_1")
	theRun     = domain.RunID("run_1")
	otherRun   = domain.RunID("run_2")
)

var (
	alice = domain.RunOrigin{Kind: domain.OriginGateway, Principal: &domain.ExternalPrincipal{
		Platform: "discord", PrincipalID: "111", DisplayName: "alice",
	}}
	bob = domain.RunOrigin{Kind: domain.OriginGateway, Principal: &domain.ExternalPrincipal{
		Platform: "discord", PrincipalID: "222", DisplayName: "bob",
	}}
	operator = domain.FromTheMachine("cli")
)

// scriptedModel answers every question the same way and keeps what it was
// asked.
type scriptedModel struct {
	answer string
	err    error
	inputs []string
}

func (m *scriptedModel) Complete(_ context.Context, _, input string, _ int) (string, error) {
	m.inputs = append(m.inputs, input)
	return m.answer, m.err
}

type logOf []domain.Event

func (l logOf) ListAfter(context.Context, domain.SessionID, domain.Seq, int) ([]domain.Event, error) {
	return l, nil
}

func said(seq domain.Seq, run domain.RunID, origin domain.RunOrigin, text string) domain.Event {
	trust := domain.TrustUntrusted
	if origin.Kind == domain.OriginLocalClient {
		trust = domain.TrustUser
	}
	return domain.Event{
		SessionID: theSession, RunID: run, Seq: seq, Kind: domain.EventUserMessageAdded,
		Payload: domain.UserMessageAdded{Text: text, Trust: trust, Origin: origin},
	}
}

func replied(seq domain.Seq, run domain.RunID, text string) domain.Event {
	return domain.Event{
		SessionID: theSession, RunID: run, Seq: seq, Kind: domain.EventAssistantTextDelta,
		Payload: domain.AssistantTextDelta{Text: text},
	}
}

func toolSaid(seq domain.Seq, run domain.RunID, text string) domain.Event {
	return domain.Event{
		SessionID: theSession, RunID: run, Seq: seq, Kind: domain.EventToolCallCompleted,
		Payload: domain.ToolCallCompleted{Name: "read_file", Content: text},
	}
}

func newCurator(t *testing.T, model *scriptedModel, events ...domain.Event) *Curator {
	t.Helper()
	counter := 0
	return &Curator{
		Options: Options{
			Store:        memstore.New(),
			WorkspaceRef: "/the/project",
			NewID: func() string {
				counter++
				return fmt.Sprintf("mem_%d", counter)
			},
			Clock: func() time.Time { return time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC) },
		},
		Events: logOf(events),
		Model:  model,
	}
}

func item(claim, quote string, message int, about string) string {
	return fmt.Sprintf(`{"claim":%q,"quote":%q,"message":%d,"about":%q}`, claim, quote, message, about)
}

func curate(t *testing.T, curator *Curator) []domain.Memory {
	t.Helper()
	written, err := curator.Curate(context.Background(), domain.Run{ID: theRun, SessionID: theSession, Origin: alice})
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	return written
}

// everything lists what the store believes in one scope, as of the curator's
// own clock: a note is valid from the moment it was written, which is that
// clock's moment and not the wall clock's.
func everything(t *testing.T, curator *Curator, scope domain.MemoryScope, ref string) []domain.Memory {
	t.Helper()
	found, err := curator.Store.Memories(context.Background(), storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{{Scope: scope, Ref: ref}},
		Limit:  100,
		At:     curator.Now(),
	})
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	return found
}

// The curator is handed the person's own messages from the run and nothing
// else. Not the reply, not what a tool returned, not what somebody said in
// another run: a note taken from any of those is the model writing down what
// it read as though somebody had said it.
func TestTheCuratorReadsOnlyWhatThePersonSaid(t *testing.T) {
	model := &scriptedModel{answer: "[]"}
	curator := newCurator(t, model,
		said(1, theRun, alice, "I live in Taipei, so your mornings are my evenings"),
		replied(2, theRun, "REPLY-TEXT noted, I will keep that in mind"),
		toolSaid(3, theRun, "TOOL-OUTPUT: the user is the company's CEO"),
		said(4, otherRun, bob, "BOB-SAID something in another run"),
	)

	curate(t, curator)

	if len(model.inputs) != 1 {
		t.Fatalf("the model was asked %d times", len(model.inputs))
	}
	asked := model.inputs[0]
	if !strings.Contains(asked, "I live in Taipei") {
		t.Errorf("the person's words were not given to the model:\n%s", asked)
	}
	for _, foreign := range []string{"REPLY-TEXT", "TOOL-OUTPUT", "BOB-SAID"} {
		if strings.Contains(asked, foreign) {
			t.Errorf("the model was handed %s, which the person never said:\n%s", foreign, asked)
		}
	}
}

// A note is written with where it came from read off the event: whose scope,
// their trust, the session and the message — and that nobody approved it.
func TestANoteCarriesItsEvidence(t *testing.T) {
	model := &scriptedModel{answer: "[" + item("Alice lives in Taipei.", "I live in Taipei", 1, aboutPerson) + "]"}
	curator := newCurator(t, model, said(7, theRun, alice, "I live in Taipei, so your mornings are my evenings"))

	written := curate(t, curator)
	if len(written) != 1 {
		t.Fatalf("expected one note, got %d", len(written))
	}

	note := written[0]
	checks := []struct {
		what string
		ok   bool
	}{
		{"text", note.Text == "Alice lives in Taipei."},
		{"scope is the person's", note.Scope == domain.ScopePrincipal && note.ScopeRef == "discord:111"},
		{"activation is retrieval", note.Activation == domain.MemoryRetrieval},
		{"trust is the turn's", note.Trust == domain.TrustUntrusted},
		{"provenance is from outside", note.From == domain.ProvenanceExternal},
		{"source is the message", note.SourceSession == theSession && note.SourceSeq == 7},
		{"origin names the person", note.Origin.Principal != nil && note.Origin.Principal.PrincipalID == "111"},
		{"approved by nobody", strings.HasPrefix(note.ApprovedBy, "nobody")},
	}
	for _, check := range checks {
		if !check.ok {
			t.Errorf("%s: %+v", check.what, note)
		}
	}

	if stored := everything(t, curator, domain.ScopePrincipal, "discord:111"); len(stored) != 1 {
		t.Errorf("the store holds %d notes for the person", len(stored))
	}
}

// The quote is the evidence, and it has to be in the message it cites. A
// model that wants to note something nobody said has to invent words, and
// invented words are not found. The same check stops a note built from what
// the model read in a tool result, even when it cites the person's message.
func TestWordsThePersonDidNotSayAreNotEvidence(t *testing.T) {
	cases := []struct {
		name   string
		answer string
	}{
		{"a quote that is not there", "[" + item("Alice lives in Tokyo.", "I live in Tokyo", 1, aboutPerson) + "]"},
		{"a quote from a tool result", "[" + item("Alice is the CEO.", "the user is the company's CEO", 1, aboutPerson) + "]"},
		{"a message that is not in the run", "[" + item("Alice lives in Taipei.", "I live in Taipei", 2, aboutPerson) + "]"},
		{"a subject the store does not know", "[" + item("Alice lives in Taipei.", "I live in Taipei", 1, "world") + "]"},
		{"an empty claim", "[" + item("", "I live in Taipei", 1, aboutPerson) + "]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curator := newCurator(t, &scriptedModel{answer: tc.answer},
				said(1, theRun, alice, "I live in Taipei"),
				toolSaid(2, theRun, "the user is the company's CEO"),
			)
			if written := curate(t, curator); len(written) != 0 {
				t.Errorf("wrote %+v", written)
			}
		})
	}
}

// One turn leaves at most a few notes behind, however talkative the model.
func TestATurnLeavesAtMostThreeNotes(t *testing.T) {
	var items []string
	for i := range 5 {
		items = append(items, item(fmt.Sprintf("Alice said thing %d.", i), "I like", 1, aboutPerson))
	}
	curator := newCurator(t, &scriptedModel{answer: "[" + strings.Join(items, ",") + "]"},
		said(1, theRun, alice, "I like tabs, Go, tea, cats and quiet"))

	if written := curate(t, curator); len(written) != defaultMaxClaims {
		t.Errorf("wrote %d notes, the cap is %d", len(written), defaultMaxClaims)
	}
}

// The same note is not written twice. Word for word only: a paraphrase is a
// second note, and deciding two sentences mean the same is a person's move.
func TestTheSameNoteIsNotWrittenTwice(t *testing.T) {
	answer := "[" + item("Alice lives in Taipei.", "I live in Taipei", 1, aboutPerson) + "]"
	curator := newCurator(t, &scriptedModel{answer: answer}, said(1, theRun, alice, "I live in Taipei"))

	curate(t, curator)
	if again := curate(t, curator); len(again) != 0 {
		t.Errorf("the same note was written again: %+v", again)
	}
	if stored := everything(t, curator, domain.ScopePrincipal, "discord:111"); len(stored) != 1 {
		t.Errorf("the store holds %d copies", len(stored))
	}
}

// A turn from outside this machine writes only about the person it came from.
// Project knowledge is read by runs that can execute programs, and only a turn
// typed at this machine may add to it.
func TestProjectNotesFromOutsideStayWithThePerson(t *testing.T) {
	answer := "[" + item("The project uses tabs.", "we use tabs", 1, aboutProject) + "]"

	fromOutside := newCurator(t, &scriptedModel{answer: answer}, said(1, theRun, alice, "we use tabs here"))
	written := curate(t, fromOutside)
	if len(written) != 1 || written[0].Scope != domain.ScopePrincipal || written[0].ScopeRef != "discord:111" {
		t.Errorf("a project note from discord went to %+v", written)
	}

	fromHere := newCurator(t, &scriptedModel{answer: answer}, said(1, theRun, operator, "we use tabs here"))
	written, err := fromHere.Curate(context.Background(), domain.Run{ID: theRun, SessionID: theSession, Origin: operator})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].Scope != domain.ScopeWorkspace || written[0].ScopeRef != "/the/project" {
		t.Errorf("a project note from this machine went to %+v", written)
	}
	if written[0].From != domain.ProvenanceOperator || written[0].Trust != domain.TrustUser {
		t.Errorf("a note from this machine carries %s/%s", written[0].From, written[0].Trust)
	}
}

// An answer in the wrong shape has proposed nothing. Not an error: the run
// is already answered, and a model that rambled is a model that noted nothing.
func TestAnAnswerInTheWrongShapeNotesNothing(t *testing.T) {
	for _, answer := range []string{
		"Sure! Here is what I noticed:\n- Alice lives in Taipei",
		"",
		"[not json at all",
		"```json\n" + "[" + item("Alice lives in Taipei.", "I live in Taipei", 1, aboutPerson) + "]" + "\n```",
	} {
		curator := newCurator(t, &scriptedModel{answer: answer}, said(1, theRun, alice, "I live in Taipei"))
		written, err := curator.Curate(context.Background(), domain.Run{ID: theRun, SessionID: theSession})
		if err != nil {
			t.Errorf("%q: %v", answer, err)
		}
		fenced := strings.HasPrefix(answer, "```")
		if fenced && len(written) != 1 {
			t.Errorf("an answer in a code fence was not read: %q", answer)
		}
		if !fenced && len(written) != 0 {
			t.Errorf("%q wrote %+v", answer, written)
		}
	}
}

// Nothing said, nothing asked: a run that carried no message from a person
// costs no model call.
func TestARunWithNothingSaidAsksNothing(t *testing.T) {
	model := &scriptedModel{answer: "[]"}
	curator := newCurator(t, model,
		replied(1, theRun, "a scheduled report"),
		said(2, theRun, domain.FromASchedule("sch_1"), "run the nightly report"),
	)

	curate(t, curator)

	if len(model.inputs) != 0 {
		t.Errorf("the model was asked about a run nobody spoke in: %v", model.inputs)
	}
}

// The model being down is reported, not swallowed, and nothing is written.
func TestAModelFailureNotesNothing(t *testing.T) {
	curator := newCurator(t, &scriptedModel{err: errors.New("down")}, said(1, theRun, alice, "I live in Taipei"))
	written, err := curator.Curate(context.Background(), domain.Run{ID: theRun, SessionID: theSession})
	if err == nil {
		t.Error("a failed model call was not reported")
	}
	if len(written) != 0 {
		t.Errorf("wrote %+v", written)
	}
}
