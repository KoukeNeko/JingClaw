package prompt

import (
	"strings"
	"testing"
)

// The bracketed line in front of a turn is explained, not left to be guessed.
// Without this the model was inferring what the time meant; with a sender on
// the line, guessing wrong would mean answering the wrong person.
func TestTheContractExplainsTheLineInFrontOfATurn(t *testing.T) {
	for _, want := range []string{"square brackets", "who sent it", "written by this machine", "grants nothing"} {
		if !strings.Contains(contract, want) {
			t.Errorf("the contract does not say %q about the line in front of a turn", want)
		}
	}
}
