package runtime_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// A stamp, as it reaches the model.
var stamped = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:[+-]\d{2}:\d{2}|Z)`)

// Every turn a person took says when they took it.
//
// Without this the conversation reaching the model is a sequence with no
// times in it at all. `current_time` says what the clock reads now and
// nothing about when any of this was said, so "how long ago did we talk about
// that", "is this from yesterday" and "you told me that last week" are
// unanswerable — and a model asked them answers anyway.
func TestEveryTurnSaysWhenItWasTaken(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)

	say(t, rt, session, "first thing")
	say(t, rt, session, "second thing")

	spoken := userTurnsSentTo(t, model)
	if len(spoken) != 2 {
		t.Fatalf("%d user turns reached the model, want two", len(spoken))
	}
	for index, turn := range spoken {
		if !stamped.MatchString(turn) {
			t.Errorf("turn %d reached the model with no time on it:\n%s", index+1, turn)
		}
	}
}

// The stamp is the log's, not the clock's when the request was assembled.
//
// The conversation is rebuilt from the event log on every turn. A time read
// from the clock during that rebuild would differ each time, which is both
// wrong — it would date an old turn to now — and expensive: the prefix a
// provider is paid to remember would change on every request.
func TestAStampIsTheSameEveryTimeTheTurnIsRebuilt(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)

	say(t, rt, session, "first thing")
	time.Sleep(1100 * time.Millisecond)
	say(t, rt, session, "second thing")

	requests := model.turnRequests()
	if len(requests) < 2 {
		t.Fatalf("%d requests reached the model, want at least two", len(requests))
	}

	first := stamped.FindString(firstUserTurn(t, requests[0]))
	again := stamped.FindString(firstUserTurn(t, requests[1]))
	if first == "" || again == "" {
		t.Fatalf("the first turn was not stamped on both passes: %q then %q", first, again)
	}
	if first != again {
		t.Errorf("the same turn was dated %s and then %s; the prefix changes every request",
			first, again)
	}
}

// What the person said is not rewritten to carry it.
//
// The stamp is written by this machine and the turn is written by somebody
// else. Running them together would put words in a person's mouth in the one
// place the model is told to treat as theirs, and a turn quoted back would
// come out with a timestamp inside it.
func TestTheStampIsNotMixedIntoWhatWasSaid(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)

	say(t, rt, session, "what is in this file")

	for _, block := range firstUserBlocks(t, model) {
		text, ok := block.(provider.TextBlock)
		if !ok {
			continue
		}
		if strings.Contains(text.Text, "what is in this file") && stamped.MatchString(text.Text) {
			t.Errorf("the stamp was written into what they said: %q", text.Text)
		}
	}
}

// helpers

func aConversation(t *testing.T) (*compactingProvider, *runtime.Runtime) {
	t.Helper()

	artifacts, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}
	model := &compactingProvider{reply: "Noted."}
	return model, newImageHarness(t, memory.New(), artifacts, model)
}

func aSession(t *testing.T, rt *runtime.Runtime) domain.SessionID {
	t.Helper()

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session.ID
}

func say(t *testing.T, rt *runtime.Runtime, session domain.SessionID, text string) {
	t.Helper()

	runID, _, err := rt.SendTurnTo(context.Background(), session, domain.Turn{
		Text:    text,
		Origin:  domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "cli"},
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient}},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForRun(t, rt, runID)
}

// userTurnsSentTo is every user turn in the last request, flattened.
func userTurnsSentTo(t *testing.T, model *compactingProvider) []string {
	t.Helper()

	requests := model.turnRequests()
	if len(requests) == 0 {
		t.Fatal("nothing reached the model")
	}

	var turns []string
	for _, message := range requests[len(requests)-1].Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		said := &strings.Builder{}
		for _, block := range message.Content {
			if text, ok := block.(provider.TextBlock); ok {
				said.WriteString(text.Text)
				said.WriteString("\n")
			}
		}
		turns = append(turns, said.String())
	}
	return turns
}

func firstUserTurn(t *testing.T, request provider.Request) string {
	t.Helper()

	for _, message := range request.Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		said := &strings.Builder{}
		for _, block := range message.Content {
			if text, ok := block.(provider.TextBlock); ok {
				said.WriteString(text.Text)
				said.WriteString("\n")
			}
		}
		return said.String()
	}
	t.Fatal("the request has no user turn in it")
	return ""
}

func firstUserBlocks(t *testing.T, model *compactingProvider) []provider.ContentBlock {
	t.Helper()

	requests := model.turnRequests()
	if len(requests) == 0 {
		t.Fatal("nothing reached the model")
	}
	for _, message := range requests[0].Messages {
		if message.Role == provider.RoleUser {
			return message.Content
		}
	}
	t.Fatal("the request has no user turn in it")
	return nil
}

// sayAs is say, from somebody in particular.
func sayAs(t *testing.T, rt *runtime.Runtime, session domain.SessionID, text string, origin domain.RunOrigin) {
	t.Helper()
	runID, _, err := rt.SendTurnTo(context.Background(), session, domain.Turn{
		Text:    text,
		Origin:  origin,
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient}},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForRun(t, rt, runID)
}

// A turn says who sent it and from where, not only when.
//
// In a room several people type in, the model was answering everybody as one
// person, because nothing it was shown told it otherwise. The line this
// machine writes in front of a turn now names the sender and the way in —
// from the event, so it is the same on every rebuild, like the time.
func TestATurnSaysWhoSentItAndFromWhere(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)

	sayAs(t, rt, session, "真的嗎", domain.FromAPlatformAccount("discord", "u_roc", "roc_general"))
	sayAs(t, rt, session, "共匪抓到", domain.FromAPlatformAccount("discord", "u_doe", "doeshing"))

	spoken := userTurnsSentTo(t, model)
	if len(spoken) != 2 {
		t.Fatalf("%d user turns reached the model, want two", len(spoken))
	}
	// On Discord the sender is written as the platform addresses them — the
	// id, which a reply can ping and a display name cannot spoof — with the
	// name they go by beside it.
	for _, want := range []string{"<@u_roc>", "roc_general", "discord", "gateway"} {
		if !strings.Contains(spoken[0], want) {
			t.Errorf("the first turn does not say %q:\n%s", want, spoken[0])
		}
	}
	if !strings.Contains(spoken[1], "<@u_doe>") || strings.Contains(spoken[1], "u_roc") {
		t.Errorf("the second turn does not name its own sender alone:\n%s", spoken[1])
	}
	if !stamped.MatchString(spoken[0]) {
		t.Errorf("naming the sender lost the time:\n%s", spoken[0])
	}
}

// A turn typed at this machine says so, and does not invent a person: the
// loopback credential identifies the machine, not who is at it.
func TestATurnFromThisMachineSaysSo(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)
	say(t, rt, session, "hello")

	turn := userTurnsSentTo(t, model)[0]
	if !strings.Contains(turn, "this machine") || !strings.Contains(turn, "cli") {
		t.Errorf("a local turn does not say it came from this machine:\n%s", turn)
	}
}

// Who sent it goes in the annotation, never into what they said.
func TestTheSenderIsNotMixedIntoWhatWasSaid(t *testing.T) {
	model, rt := aConversation(t)
	session := aSession(t, rt)
	sayAs(t, rt, session, "what is in this file", domain.FromAPlatformAccount("discord", "u1", "doeshing"))

	for _, block := range firstUserBlocks(t, model) {
		text, ok := block.(provider.TextBlock)
		if !ok {
			continue
		}
		if strings.Contains(text.Text, "what is in this file") && strings.Contains(text.Text, "doeshing") {
			t.Errorf("the sender was written into what they said: %q", text.Text)
		}
		if strings.Contains(text.Text, "doeshing") && !text.Annotation {
			t.Errorf("the sender line is not marked as this machine's: %q", text.Text)
		}
	}
}
