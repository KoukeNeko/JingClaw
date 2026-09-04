package runtime

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

func foldLog(folds ...domain.ConversationCompacted) []domain.Event {
	events := make([]domain.Event, 0, len(folds))
	for i, fold := range folds {
		events = append(events, domain.Event{
			Seq:     domain.Seq(100 + i),
			Kind:    domain.EventConversationCompacted,
			Payload: fold,
		})
	}
	return events
}

func throughs(folds []domain.ConversationCompacted) []domain.Seq {
	out := make([]domain.Seq, 0, len(folds))
	for _, fold := range folds {
		out = append(out, fold.ThroughSeq)
	}
	return out
}

func sameSeqs(a, b []domain.Seq) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// One rule decides which folds still stand: a fold replaces every earlier fold
// that starts at or after it does. The three shapes a log can hold all fall
// out of it.
func TestWhichFoldsStand(t *testing.T) {
	cases := []struct {
		name  string
		folds []domain.ConversationCompacted
		want  []domain.Seq
	}{
		{
			name: "ordinary folds stand side by side",
			folds: []domain.ConversationCompacted{
				{FromSeq: 1, ThroughSeq: 10},
				{FromSeq: 11, ThroughSeq: 20},
				{FromSeq: 21, ThroughSeq: 30},
			},
			want: []domain.Seq{10, 20, 30},
		},
		{
			name: "a fold from before ranges were recorded replaces everything before it",
			folds: []domain.ConversationCompacted{
				{FromSeq: 1, ThroughSeq: 10},
				{FromSeq: 11, ThroughSeq: 20},
				{FromSeq: 0, ThroughSeq: 30},
			},
			want: []domain.Seq{30},
		},
		{
			name: "an ordinary fold after a range-less one leaves it standing",
			folds: []domain.ConversationCompacted{
				{FromSeq: 0, ThroughSeq: 10},
				{FromSeq: 11, ThroughSeq: 20},
			},
			want: []domain.Seq{10, 20},
		},
		{
			name: "a condensing fold replaces the folds it condensed",
			folds: []domain.ConversationCompacted{
				{FromSeq: 1, ThroughSeq: 10},
				{FromSeq: 11, ThroughSeq: 20},
				{FromSeq: 21, ThroughSeq: 30},
				{FromSeq: 11, ThroughSeq: 40},
			},
			want: []domain.Seq{10, 40},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := throughs(foldsIn(foldLog(tc.folds...)))
			if !sameSeqs(got, tc.want) {
				t.Errorf("standing folds reach %v, want %v", got, tc.want)
			}
		})
	}
}

func rawMessage(seq domain.Seq, text string) boundedMessage {
	return boundedMessage{
		Message: provider.Message{Role: provider.RoleUser, Content: provider.Text(text)},
		LastSeq: seq,
	}
}

func foldMessage(from, through domain.Seq) boundedMessage {
	return boundedMessage{
		Message: provider.Message{Role: provider.RoleUser, Content: provider.Text("summary")},
		LastSeq: through,
		Fold:    true,
		FromSeq: from,
	}
}

// The plan never summarises the folds in front of the conversation on their
// own, and starts the new fold right after the last one.
func TestAPlanStartsAfterTheFolds(t *testing.T) {
	messages := []boundedMessage{
		foldMessage(1, 10),
		foldMessage(11, 20),
		rawMessage(21, "old"),
		rawMessage(22, "older still"),
		rawMessage(23, "the latest turn"),
	}

	plan, ok := planCompaction(messages, 20, 1)
	if !ok {
		t.Fatal("nothing planned")
	}
	if plan.start != 2 {
		t.Errorf("the plan summarises from index %d; the two folds are at 0 and 1", plan.start)
	}
	if plan.from != 21 {
		t.Errorf("the new fold starts at %d, not right after the last one at 21", plan.from)
	}
	if plan.cut != 4 || plan.through != 22 {
		t.Errorf("the plan cuts at %d through %d; the latest turn must stay", plan.cut, plan.through)
	}
}

// When only folds would be folded there is nothing to do, and saying so is
// what keeps a compaction that cannot advance from looping.
func TestAPlanThatWouldOnlyFoldFoldsIsNoPlan(t *testing.T) {
	messages := []boundedMessage{
		foldMessage(1, 10),
		rawMessage(11, "the latest turn"),
	}
	if _, ok := planCompaction(messages, 10, 1); ok {
		t.Error("planned a compaction whose only foldable message is a fold")
	}
}

// The fold event sits after the turn it was made for, so the events between
// what it reaches and where it is are the turn being answered. Retention must
// stop at what the fold reaches; cutting at the event before the fold threw
// away the question the model was about to be asked.
func TestRetentionNeverReachesPastTheFold(t *testing.T) {
	events := []domain.Event{
		{Seq: 1, Kind: domain.EventUserMessageAdded},
		{Seq: 2, Kind: domain.EventAssistantMessageCompleted},
		{Seq: 3, Kind: domain.EventUserMessageAdded},
		{Seq: 4, Kind: domain.EventConversationCompacted, Payload: domain.ConversationCompacted{FromSeq: 1, ThroughSeq: 2}},
	}

	if got := safeCut(events, 0); got != 2 {
		t.Errorf("safe cut is %d; the fold reaches 2 and the turn at 3 is still being answered", got)
	}
}

// Once enough folds stand, the plan takes all of them together with the turns
// being folded, and the new fold starts where the first of them did.
func TestAPlanCondensesAPileOfFolds(t *testing.T) {
	var messages []boundedMessage
	for i := range maxFoldsInHead {
		from := domain.Seq(i*10 + 1)
		messages = append(messages, foldMessage(from, from+9))
	}
	last := messages[len(messages)-1].LastSeq
	messages = append(messages,
		rawMessage(last+1, "old"),
		rawMessage(last+2, "the latest turn"),
	)

	plan, ok := planCompaction(messages, last, 1)
	if !ok {
		t.Fatal("nothing planned")
	}
	if plan.start != 0 || plan.from != 1 {
		t.Errorf("the plan starts at index %d from seq %d; condensing must take every fold from the first",
			plan.start, plan.from)
	}
}
