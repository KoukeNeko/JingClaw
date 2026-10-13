package control

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/KoukeNeko/JingClaw/core/internal/skill"
)

// The skill package says why it refused in the sentinel it tags an error with;
// the code a client sees is decided here, and is whose move it is: INVALID_ARGUMENT
// is a request to correct, FAILED_PRECONDITION is a state to change first.
func TestEachWayTheSkillPackageRefusesHasItsCode(t *testing.T) {
	for _, one := range []struct {
		refusal error
		want    connect.Code
	}{
		{skill.ErrBadName, connect.CodeInvalidArgument},
		{skill.ErrSource, connect.CodeFailedPrecondition},
		{skill.ErrBusy, connect.CodeFailedPrecondition},
		{skill.ErrNotStaged, connect.CodeFailedPrecondition},
		{skill.ErrStale, connect.CodeFailedPrecondition},
		{skill.ErrCollision, connect.CodeFailedPrecondition},
		{skill.ErrNotInstalled, connect.CodeFailedPrecondition},
	} {
		wrapped := fmt.Errorf("a refusal with context: %w", one.refusal)
		if got := connect.CodeOf(toConnectError(wrapped)); got != one.want {
			t.Errorf("%v is %v, want %v", one.refusal, got, one.want)
		}
	}

	if got := connect.CodeOf(toConnectError(errors.New("disk full"))); got != connect.CodeInternal {
		t.Errorf("an error that is none of them is %v", got)
	}

	// A staged skill whose name cannot be a directory is a source that did not
	// yield a skill, and not a request with a bad name in it. The name came out
	// of the repository.
	both := fmt.Errorf("%w: %w", skill.ErrSource, skill.ErrBadName)
	if got := connect.CodeOf(toConnectError(both)); got != connect.CodeFailedPrecondition {
		t.Errorf("a fetched skill with a bad name is %v", got)
	}
}

// git repeats the address it was given, and an address can carry a credential.
// Whatever the words, what a client is told has none.
func TestWhatTheSkillPackageSaidIsToldWithoutACredential(t *testing.T) {
	for _, refusal := range []error{skill.ErrSource, skill.ErrBadName, skill.ErrStale} {
		said := fmt.Errorf("fetching https://user:hunter2@example.com/x.git, then 'ssh://hunter2@example.com/y': %w", refusal)

		got := toConnectError(said).Error()
		if strings.Contains(got, "hunter2") {
			t.Errorf("%v: a credential was told to a client: %s", refusal, got)
		}
		if !strings.Contains(got, "example.com/x.git") {
			t.Errorf("%v: the rest of the message was lost with it: %s", refusal, got)
		}
	}
}
