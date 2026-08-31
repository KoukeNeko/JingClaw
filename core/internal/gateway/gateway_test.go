package gateway_test

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// somebody is a principal the platform authenticated.
func somebody(id string) gateway.Principal {
	return gateway.Principal{ID: id, DisplayName: id}
}

// TestABindingNamingNobodyDefersToTheRoom is the rule for an empty list.
//
// Naming people in two places — here and in the channel's own membership —
// means keeping two lists in step, and the way that fails is somebody being
// added to the room and silently ignored. So an empty list is not the absence
// of a rule; it is the platform's rule.
func TestABindingNamingNobodyDefersToTheRoom(t *testing.T) {
	open := gateway.Binding{}

	if !open.OpenToTheRoom() {
		t.Fatal("a binding naming nobody does not report itself as open")
	}
	if !open.Permits(somebody("anyone-at-all")) {
		t.Error("a binding naming nobody refused somebody the room let in")
	}

	// And a binding that does name somebody still means only them, which is
	// the precondition: without it the check above would pass against a
	// binding that permitted everybody however it was written.
	named := gateway.Binding{AllowedPrincipals: []string{"the-operator"}}

	if named.OpenToTheRoom() {
		t.Error("a binding that names somebody reports itself as open")
	}
	if !named.Permits(somebody("the-operator")) {
		t.Error("the named person was refused")
	}
	if named.Permits(somebody("somebody-else")) {
		t.Error("a binding that names one person let in another")
	}
}

// TestAnEmptyListNeverMeansAnyoneMayApprove is the half that must not widen.
//
// "Anyone in this room may talk to it" and "anyone in this room may authorise
// what it does" are different sentences, and leaving a field out says only
// the first.
func TestAnEmptyListNeverMeansAnyoneMayApprove(t *testing.T) {
	open := gateway.Binding{}

	if open.MayApprove(somebody("anyone-at-all")) {
		t.Fatal("a binding naming no approvers let somebody approve")
	}

	// Even when the same binding is open to the room for speaking, which is
	// exactly the configuration this is about.
	if !open.Permits(somebody("anyone-at-all")) {
		t.Fatal("the binding under test is not the open one")
	}
	if open.MayApprove(somebody("anyone-at-all")) {
		t.Error("being allowed to ask became being allowed to permit")
	}
}

// TestABotIsRefusedHoweverTheBindingIsWritten keeps the loop closed.
//
// Two automations talking each other into an unbounded conversation is the
// failure this guards, and an open binding must not be the way in.
func TestABotIsRefusedHoweverTheBindingIsWritten(t *testing.T) {
	bot := gateway.Principal{ID: "some-bot", IsBot: true}

	if (gateway.Binding{}).Permits(bot) {
		t.Error("an open binding let a bot in")
	}
	if (gateway.Binding{AllowedPrincipals: []string{"some-bot"}}).Permits(bot) {
		t.Error("a bot named in the list was let in")
	}
}
