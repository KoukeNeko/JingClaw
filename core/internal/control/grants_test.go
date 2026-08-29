package control_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

func newGrants(ttl time.Duration) (*control.Grants, *time.Time) {
	clock := time.Unix(1_700_000_000, 0).UTC()

	var minted int
	return control.NewGrants(ttl, func() time.Time { return clock }, func() string {
		minted++
		return fmt.Sprintf("con_%d", minted)
	}), &clock
}

// Two browsers must not share a credential. Sharing one is what made revoking
// a page mean revoking every page, and what made "which browsers can reach
// this agent" a question with no answer.
func TestEachPairingGetsItsOwnCredential(t *testing.T) {
	grants, _ := newGrants(0)

	first, firstID, err := grants.Issue("one browser")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	second, secondID, err := grants.Issue("another browser")
	if err != nil {
		t.Fatalf("issue again: %v", err)
	}

	if first.Value == second.Value {
		t.Fatal("two browsers were handed the same credential")
	}
	if firstID == secondID {
		t.Fatal("two grants share an id, so neither can be revoked alone")
	}
	if first.Scope != control.ScopeConsole {
		t.Errorf("scope is %q, want the console's", first.Scope)
	}
}

// Revoking one must end that one and nothing else.
func TestRevokingOneLeavesTheOthers(t *testing.T) {
	grants, _ := newGrants(0)

	doomed, doomedID, _ := grants.Issue("the one being revoked")
	kept, _, _ := grants.Issue("the one still working")

	if !grants.Revoke(doomedID) {
		t.Fatal("revoking a grant that exists reported that it did not")
	}
	if grants.Verify(doomed.Value) {
		t.Error("the revoked credential still works")
	}
	if !grants.Verify(kept.Value) {
		t.Error("revoking one browser revoked another")
	}
}

// A mistyped id must not be answered as success. "It is gone" and "I did not
// find it" lead somewhere different when what somebody is doing is shutting
// off access in a hurry.
func TestRevokingSomethingThatIsNotThereSaysSo(t *testing.T) {
	grants, _ := newGrants(0)
	grants.Issue("a browser")

	if grants.Revoke("con_nothing") {
		t.Error("revoking an id that names nothing reported success")
	}
}

// Somebody who has decided that whatever is out there should not be needs one
// action, not one per row.
func TestEverythingCanBeRevokedAtOnce(t *testing.T) {
	grants, _ := newGrants(0)

	first, _, _ := grants.Issue("one")
	second, _, _ := grants.Issue("another")

	if ended := grants.RevokeAll(); ended != 2 {
		t.Errorf("revoked %d, want both", ended)
	}
	if grants.Verify(first.Value) || grants.Verify(second.Value) {
		t.Error("a credential survived revoking everything")
	}
	if len(grants.List()) != 0 {
		t.Error("the list is not empty after revoking everything")
	}
}

// The listing is what ends up in a screenshot, so it must not carry the thing
// worth stealing.
func TestTheListingCarriesNoCredentials(t *testing.T) {
	grants, _ := newGrants(0)
	token, id, _ := grants.Issue("Mozilla/5.0 (Macintosh)")

	listed := grants.List()
	if len(listed) != 1 {
		t.Fatalf("listed %d grants, want one", len(listed))
	}
	if listed[0].ID != id {
		t.Errorf("id is %q, want %q", listed[0].ID, id)
	}
	if strings.Contains(fmt.Sprintf("%+v", listed[0]), token.Value) {
		t.Error("the listing carries the credential itself")
	}
	if listed[0].Label != "Mozilla/5.0 (Macintosh)" {
		t.Errorf("label is %q", listed[0].Label)
	}
}

// A console somebody works in all day must not stop in the afternoon, and one
// nobody has touched since Tuesday must not still be open. Both follow from
// counting the idle time rather than the age.
func TestACredentialExpiresFromLastUseNotFromPairing(t *testing.T) {
	grants, clock := newGrants(time.Hour)
	token, _, _ := grants.Issue("a browser")

	// Used every half hour for a day: still working.
	for i := 0; i < 48; i++ {
		*clock = clock.Add(30 * time.Minute)
		if !grants.Verify(token.Value) {
			t.Fatalf("a credential in constant use stopped working after %d uses", i)
		}
	}

	// Then left alone.
	*clock = clock.Add(2 * time.Hour)
	if grants.Verify(token.Value) {
		t.Error("a credential nobody has used for two hours still works with a one-hour limit")
	}
}

// A label is a User-Agent header, which is whatever the other end felt like
// sending. It is shown to a person in a terminal.
func TestALabelCannotFillOrEscapeATerminal(t *testing.T) {
	grants, _ := newGrants(0)
	grants.Issue("\x1b[31mred\x07\n" + strings.Repeat("x", 500))

	label := grants.List()[0].Label
	if strings.ContainsAny(label, "\x1b\x07\n") {
		t.Errorf("the label carries control characters: %q", label)
	}
	if len([]rune(label)) > 80 {
		t.Errorf("the label is %d characters", len([]rune(label)))
	}
}
