package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// posted returns what the channel would have been sent.
func posted(t *testing.T, h *harness) []string {
	t.Helper()

	var texts []string
	for _, dispatch := range h.dispatches(t) {
		if dispatch.Kind != gateway.DispatchMessage {
			continue
		}
		var payload gateway.MessagePayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		texts = append(texts, payload.Text)
	}
	return texts
}

func said(texts []string, want string) bool {
	for _, text := range texts {
		if strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			return true
		}
	}
	return false
}

// The first thing a console channel is used for, somebody is told what it is.
// A boundary that lives only in a configuration file on a machine nobody in
// the room is sitting at is one everybody will assume the shape of instead.
func TestAConsoleChannelSaysWhatItIsTheFirstTimeItIsUsed(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "console", "user_1")

	if _, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	texts := posted(t, h)
	if !said(texts, "cannot run programs") {
		t.Errorf("the channel was never told what it cannot do: %v", texts)
	}
	if !said(texts, "approve") {
		t.Errorf("the channel was not told it can decide: %v", texts)
	}
}

// An ordinary channel gets no such notice, because none of it is true there.
func TestAnOrdinaryChannelIsNotToldItIsAConsole(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	if _, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if said(posted(t, h), "This channel is a console") {
		t.Error("an ordinary channel was told it is a console")
	}
}

// Asking what is waiting is answered by this program, and must not become a
// turn in the conversation: the agent reading its own console as something
// somebody said to it is how a command becomes a prompt.
func TestAConsoleCommandIsNotATurn(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "console", "user_1")

	// One real turn first, so the conversation exists.
	first, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), first.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	accepted, err := h.ingress.Accept(context.Background(),
		message("m2", "pending", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	if accepted.RunID != "" {
		t.Errorf("a console command started run %s", accepted.RunID)
	}
	if !said(posted(t, h), "Nothing is waiting") {
		t.Errorf("the command was not answered: %v", posted(t, h))
	}
}

// The same words in an ordinary channel are just words, and reach the agent.
func TestTheSameWordsInAnOrdinaryChannelReachTheAgent(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "pending", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.RunID == "" {
		t.Error("a message in an ordinary channel was swallowed as a command")
	}
}

// A console decides for the work it can see. Reaching past its own
// conversation would make every bound channel a way to approve anything,
// including a run somebody started at the machine.
func TestAConsoleCannotDecideAnotherConversationsApproval(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "console", "user_1")

	// A session belonging to nobody in this channel, with something waiting.
	other, err := h.runtime.CreateSession(context.Background(), "elsewhere")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), first.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// An id that is not this channel's is refused whether or not it exists.
	if _, err := h.ingress.Accept(context.Background(),
		message("m2", "approve apr_from_elsewhere", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if !said(posted(t, h), "Nothing here is waiting") {
		t.Errorf("an approval from another conversation was not refused: %v", posted(t, h))
	}
	_ = other
}

// The point of the whole thing: a run stops for a decision, somebody answers
// in the channel, and the run continues.
func TestAnApprovalIsGivenInTheChannelAndTheRunContinues(t *testing.T) {
	arguments, err := json.Marshal(map[string]any{"path": "notes.md", "content": "written"})
	if err != nil {
		t.Fatal(err)
	}

	h := newSummaryHarness(t, [][]provider.Event{
		{
			provider.ToolCallRequested{ID: "call_1", Name: "write_file", Args: arguments},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "Written."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})
	h.bind(t, "console", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "write notes.md", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Writing stops in a console, and the channel is told it may answer.
	approval := waitForDispatch(t, h, gateway.DispatchApproval)
	var payload gateway.ApprovalPayload
	if err := json.Unmarshal([]byte(approval.Payload), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.DecidableHere {
		t.Error("a console channel was not told it can decide its own approval")
	}

	// Answered in the channel.
	if _, err := h.ingress.Accept(context.Background(),
		message("m2", "approve "+payload.ApprovalID, discordPrincipal("user_1"))); err != nil {
		t.Fatalf("approve: %v", err)
	}

	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("the run did not continue after being approved: %v", err)
	}
	if !said(posted(t, h), "Approved") {
		t.Errorf("the channel was not told what happened: %v", posted(t, h))
	}
}

// The line the whole design rests on. Running a program is not something a
// channel can authorise, however private the channel is: a stolen account
// holds the request and the approval both, and only being at the machine
// proves anybody is.
func TestAConsoleCannotRunPrograms(t *testing.T) {
	h := newSummaryHarness(t, [][]provider.Event{
		{
			provider.ToolCallRequested{
				ID:   "call_1",
				Name: "exec_command",
				Args: json.RawMessage(`{"program":"rm","args":["-rf","/"]}`),
			},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "I could not."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})
	h.bind(t, "console", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "delete everything", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Refused outright, not offered for approval: there is nothing here that
	// could grant it.
	for _, dispatch := range h.dispatches(t) {
		if dispatch.Kind == gateway.DispatchApproval {
			t.Errorf("running a program was offered for approval in a channel: %s", dispatch.Payload)
		}
	}
}

// Handed over because somebody asked for it by name, and not before. A run
// that produces a large result says it exists; the bytes cross into a channel
// only when a person names the one they want.
func TestAnArtifactIsHandedOverWhenAskedForByName(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "console", "user_1")

	stored, err := h.artifacts.PutBytes(context.Background(),
		[]byte("the whole build log"), "text/plain")
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	if _, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := h.ingress.Accept(context.Background(),
		message("m2", "artifact "+stored.ID, discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	var file *gateway.MessageFile
	for _, dispatch := range h.dispatches(t) {
		var payload gateway.MessagePayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			continue
		}
		if payload.File != nil {
			file = payload.File
		}
	}

	if file == nil {
		t.Fatalf("nothing was handed over: %v", posted(t, h))
	}
	if string(file.Content) != "the whole build log" {
		t.Errorf("the content is %q", file.Content)
	}
	// A name somebody can open, rather than a digest.
	if !strings.HasSuffix(file.Name, ".txt") {
		t.Errorf("the attachment is named %q", file.Name)
	}
}

// Asking for something that is not there is answered plainly rather than
// failing, because the usual cause is a mistyped id.
func TestAskingForSomethingThatIsNotStoredIsSaidPlainly(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "console", "user_1")

	if _, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := h.ingress.Accept(context.Background(),
		message("m2", "artifact sha256-nothing", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if !said(posted(t, h), "nothing stored") {
		t.Errorf("the channel was not told: %v", posted(t, h))
	}
}

// An ordinary channel cannot ask. The command set belongs to a console, and
// the same words elsewhere are a message for the agent.
func TestAnOrdinaryChannelCannotAskForArtifacts(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "artifact sha256-anything", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.RunID == "" {
		t.Error("the words were treated as a command in an ordinary channel")
	}
}

// A console is a log; an ordinary channel is a conversation. The same run
// produces different amounts of detail depending on who is reading.
func TestOnlyAConsoleGetsTheLog(t *testing.T) {
	arguments, err := json.Marshal(map[string]any{"path": "notes.md"})
	if err != nil {
		t.Fatal(err)
	}

	turns := [][]provider.Event{
		{
			provider.ToolCallRequested{ID: "call_1", Name: "read_file", Args: arguments},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "Read it."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	}

	for _, profile := range []string{"console", "gateway"} {
		t.Run(profile, func(t *testing.T) {
			h := newSummaryHarness(t, turns)
			h.bind(t, profile, "user_1")

			accepted, err := h.ingress.Accept(context.Background(),
				message("m1", "read notes.md", discordPrincipal("user_1")))
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
				t.Fatalf("wait: %v", err)
			}

			var logs []gateway.LogPayload
			for _, dispatch := range h.dispatches(t) {
				if dispatch.Kind != gateway.DispatchLog {
					continue
				}
				var payload gateway.LogPayload
				if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
					t.Fatalf("decode: %v", err)
				}
				logs = append(logs, payload)
			}

			if profile == "gateway" {
				if len(logs) != 0 {
					t.Errorf("a room other people can type in was sent %d log lines", len(logs))
				}
				return
			}

			if len(logs) == 0 {
				t.Fatal("a console was sent no log")
			}
			if logs[0].Tool != "read_file" {
				t.Errorf("the log names %q", logs[0].Tool)
			}
		})
	}
}

// The redaction protects a room other people read. Where the only reader is
// the operator, hiding the reason from them is not protecting anybody.
func TestAConsoleIsToldWhyARunFailed(t *testing.T) {
	upstream := "provider gemini: rate_limited: quota exceeded for metric x, limit 16000"

	for _, profile := range []string{"console", "gateway"} {
		t.Run(profile, func(t *testing.T) {
			h := newSummaryHarness(t, nil) // no turns: the model errors at once
			h.bind(t, profile, "user_1")

			accepted, err := h.ingress.Accept(context.Background(),
				message("m1", "do something", discordPrincipal("user_1")))
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			_ = h.runtime.Wait(context.Background(), accepted.RunID)

			var detail string
			for _, dispatch := range h.dispatches(t) {
				if dispatch.Kind != gateway.DispatchStatus {
					continue
				}
				var payload gateway.StatusPayload
				if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
					continue
				}
				if payload.State == "failed" {
					detail = payload.Detail
				}
			}

			if detail == "" {
				t.Fatal("the channel was not told the run failed")
			}

			if profile == "gateway" {
				// Still a sentence, still nothing about the account.
				if strings.Contains(strings.ToLower(detail), "quota") {
					t.Errorf("a public channel was told about the operator's account: %q", detail)
				}
				return
			}
			// A console gets the reason, which is what makes it fixable.
			if detail == explainFailureFor(t) {
				t.Errorf("a console was given the redacted sentence: %q", detail)
			}
		})
	}
	_ = upstream
}

// explainFailureFor is the sentence a public channel would get, for comparison.
func explainFailureFor(t *testing.T) string {
	t.Helper()
	return "something went wrong at the model"
}
