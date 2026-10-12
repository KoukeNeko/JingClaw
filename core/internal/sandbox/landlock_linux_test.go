//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

// These need no landlock: with the network allowed the kernel is never asked,
// so what is left is whether the policy can be held at all — the part of the
// refusal that does not depend on the machine.

// TestAPolicyThatHidesADirectoryIsRefused is the reason the daemon cannot turn
// this on: it always hides the deployment's own directory, and landlock has
// no way to say "everything readable except this".
func TestAPolicyThatHidesADirectoryIsRefused(t *testing.T) {
	hidden := "/home/somebody/.config/JingClaw"

	err := keepable(Policy{Network: true, Unreadable: []string{hidden}})
	if err == nil {
		t.Fatal("a policy hiding a directory was accepted, and landlock cannot keep it")
	}
	if !strings.Contains(err.Error(), hidden) {
		t.Errorf("the refusal does not name what it could not hide: %v", err)
	}
}

// TestAPolicyThatHidesNothingIsKept is the precondition. Without it the check
// above would pass against a build that refused every policy.
func TestAPolicyThatHidesNothingIsKept(t *testing.T) {
	if err := keepable(Policy{Network: true, Writable: []string{"/work"}}); err != nil {
		t.Fatalf("a policy that asks for nothing landlock lacks was refused: %v", err)
	}
}

// TestCheckRefusesWhatWrapWould is the same refusal asked for at startup,
// where an operator is still looking, rather than at the first command.
func TestCheckRefusesWhatWrapWould(t *testing.T) {
	if err := Check(Policy{Network: true, Unreadable: []string{"/somewhere"}}); err == nil {
		t.Fatal("Check accepted a policy that Wrap would refuse")
	}
}
