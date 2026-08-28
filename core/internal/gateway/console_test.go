package gateway

import (
	"strings"
	"testing"
)

// A console channel is still a place people talk. Only a short, closed list is
// treated as an instruction, matched on the whole message, because a channel
// where an ordinary sentence might be swallowed as a command is one nobody can
// use.
func TestOnlyDeliberateCommandsAreCommands(t *testing.T) {
	commands := map[string]consoleCommand{
		"approve apr_123": {verb: "approve", arg: "apr_123"},
		"allow apr_123":   {verb: "approve", arg: "apr_123"},
		"deny apr_123":    {verb: "deny", arg: "apr_123"},
		"reject apr_123":  {verb: "deny", arg: "apr_123"},
		"  pending  ":     {verb: "pending"},
		"approvals":       {verb: "pending"},
		"help":            {verb: "help"},
		"APPROVE apr_9":   {verb: "approve", arg: "apr_9"},
	}

	for text, want := range commands {
		got, ok := parseConsoleCommand(text)
		if !ok {
			t.Errorf("%q was not recognised", text)
			continue
		}
		if got != want {
			t.Errorf("%q parsed as %+v, want %+v", text, got, want)
		}
	}

	// Everything a person might actually say has to reach the agent.
	for _, text := range []string{
		"approve the design doc please",
		"can you deny that request",
		"what is pending on the release",
		"help me write a test",
		"approve",
		"deny",
		"",
		"read notes.md and tell me what it says",
	} {
		if command, ok := parseConsoleCommand(text); ok {
			t.Errorf("%q was swallowed as the command %+v instead of reaching the agent", text, command)
		}
	}
}

// The notice has to say the thing that is not obvious: that this channel
// cannot run programs, and why the rest of it is nonetheless safe.
func TestTheNoticeStatesTheBoundaryAndItsReason(t *testing.T) {
	notice := consoleMOTD(Binding{})

	for _, want := range []string{
		"console",
		"cannot run programs",
		"somebody at the machine",
		"approve",
		"deny",
		"pending",
	} {
		if !strings.Contains(strings.ToLower(notice), strings.ToLower(want)) {
			t.Errorf("the notice does not mention %q:\n%s", want, notice)
		}
	}

	// The reason, not only the rule. A boundary without one is a boundary
	// somebody will route around.
	if !strings.Contains(strings.ToLower(notice), "stolen") {
		t.Errorf("the notice states the limit without saying what it is for:\n%s", notice)
	}

	// A restricted binding says so, since who may type here is what makes the
	// rest of it reasonable.
	restricted := consoleMOTD(Binding{AllowedPrincipals: []string{"user_1"}})
	if !strings.Contains(restricted, "Only listed accounts") {
		t.Errorf("a restricted channel does not say so:\n%s", restricted)
	}
}
