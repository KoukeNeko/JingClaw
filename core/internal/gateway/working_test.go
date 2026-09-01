package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// What it is reaching for is named, not just that it is reaching.
//
// A run that reads the web reports "network_started" and nothing about which
// page, so the one status a reader most wants — where is it going — is the
// one that says least. The emoji beside a message is the same emoji whichever
// address it went to.
func TestReachingForAPageSaysWhichPage(t *testing.T) {
	said := statusFor(t, domain.ToolCallRequested{
		CallID: "call_1", Name: "web_read",
		Arguments: `{"url":"https://example.com/a"}`,
	})

	if !strings.Contains(said, "network_started") {
		t.Fatalf("a page read is no longer reported as network: %s", said)
	}
	if !strings.Contains(said, "https://example.com/a") {
		t.Errorf("it does not say which page: %s", said)
	}
}

// And so does reading memory.
func TestReadingMemorySaysWhatItLookedFor(t *testing.T) {
	said := statusFor(t, domain.ToolCallRequested{
		CallID: "call_1", Name: "recall",
		Arguments: `{"query":"the deploy runbook"}`,
	})

	if !strings.Contains(said, "the deploy runbook") {
		t.Errorf("it does not say what it looked for: %s", said)
	}
}

// A run starting says it is thinking, in words as well as in an emoji.
//
// The first thing anybody sees after sending a message, and on a platform
// that draws a line rather than a reaction it was blank: the state carried
// nothing to draw.
func TestARunStartingSaysItIsThinking(t *testing.T) {
	projector, store, _ := newProjectorFixture(t)

	if err := projector.Observe(context.Background(), gatewayRun(time.Now()), domain.Event{
		Kind:    domain.EventRunStateChanged,
		Payload: domain.RunStateChanged{Status: domain.RunRunning},
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	dispatches := enqueued(t, store)
	if len(dispatches) == 0 {
		t.Fatal("a run that started said nothing")
	}
	said := dispatches[len(dispatches)-1].Payload
	if !strings.Contains(said, "provider_started") {
		t.Fatalf("a run starting is no longer reported: %s", said)
	}
	if !strings.Contains(said, "thinking") {
		t.Errorf("there is nothing to draw for a platform that draws words: %s", said)
	}
}

// statusFor is what the projector says about one tool call.
func statusFor(t *testing.T, call domain.ToolCallRequested) string {
	t.Helper()

	projector, store, _ := newProjectorFixture(t)
	if err := projector.Observe(context.Background(), gatewayRun(time.Now()), domain.Event{
		Kind: domain.EventToolCallRequested, Payload: call,
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	dispatches := enqueued(t, store)
	if len(dispatches) == 0 {
		t.Fatalf("%s produced no status at all", call.Name)
	}
	return dispatches[len(dispatches)-1].Payload
}

// The line names the thing, whatever the tool calls its argument.
//
// A reader watching a run wants to know which skill, which question, which
// file — and every tool spells that differently. A tool whose argument is not
// recognised draws its own name and nothing else, which is the emoji problem
// again in words.
func TestTheLineNamesWhatEachToolIsWorkingOn(t *testing.T) {
	for _, call := range []struct {
		name      string
		arguments string
		wanted    string
	}{
		{"skill_load", `{"name":"deploy-runbook"}`, "deploy-runbook"},
		{"investigate", `{"question":"which functions parse the config"}`, "which functions parse"},
		{"read_file", `{"path":"notes.md"}`, "notes.md"},
		{"web_read", `{"url":"https://example.com/a"}`, "https://example.com/a"},
		{"exec_command", `{"program":"go","args":["test"]}`, "go"},
	} {
		said := statusFor(t, domain.ToolCallRequested{
			CallID: "call_1", Name: call.name, Arguments: call.arguments,
		})
		if !strings.Contains(said, call.wanted) {
			t.Errorf("%s does not say what it is working on: %s", call.name, said)
		}
	}
}

// And a long one does not become the whole message.
//
// A line that is rewritten every couple of seconds must stay a line. A
// question of two hundred characters would push the answer off the screen
// while saying nothing the first eighty did not.
func TestTheLineStaysALine(t *testing.T) {
	said := statusFor(t, domain.ToolCallRequested{
		CallID: "call_1", Name: "investigate",
		Arguments: `{"question":"` + strings.Repeat("why ", 60) + `"}`,
	})

	var payload struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(said), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len([]rune(payload.Detail)) > 100 {
		t.Errorf("the line is %d characters long: %q",
			len([]rune(payload.Detail)), payload.Detail)
	}
}
