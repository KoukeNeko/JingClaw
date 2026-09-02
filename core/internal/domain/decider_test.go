package domain_test

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// The whole point of an approval is that somebody authorised it. A record
// naming a program has recorded nobody while looking like it recorded
// somebody, which is worse than recording nothing.
func TestNothingLocalEverBecomesAPerson(t *testing.T) {
	for _, client := range []string{"jingclaw-cli", "jingclaw-console", "", "somebody"} {
		origin := domain.FromTheMachine(client)

		if origin.Principal != nil {
			t.Errorf("a decision at the machine by %q named a person: %+v",
				client, origin.Principal)
		}
		if origin.Kind != domain.OriginLocalClient {
			t.Errorf("%q is %q, want %q", client, origin.Kind, domain.OriginLocalClient)
		}
		if origin.ClientID != client {
			t.Errorf("the client is %q, want %q", origin.ClientID, client)
		}
	}
}

// A platform names whoever pressed the button, and that is the one case where
// there is a person to record.
func TestAPlatformAccountIsAPerson(t *testing.T) {
	origin := domain.FromAPlatformAccount("discord", "900000000000000042", "Alice")

	if origin.Principal == nil {
		t.Fatal("a platform account recorded no person")
	}
	if origin.Principal.PrincipalID != "900000000000000042" {
		t.Errorf("principal is %q", origin.Principal.PrincipalID)
	}
	if origin.Kind != domain.OriginGateway {
		t.Errorf("kind is %q, want %q", origin.Kind, domain.OriginGateway)
	}
}

// A console binding is a room that is the credential. The room is what was
// authorised, so the room is what is recorded — and it is still not a person.
func TestAChannelIsARoomAndNotAPerson(t *testing.T) {
	origin := domain.FromAChannel("discord", "900000000000000041")

	if origin.Principal != nil {
		t.Errorf("a room became a person: %+v", origin.Principal)
	}
	if origin.ClientID != "discord:900000000000000041" {
		t.Errorf("the room is %q", origin.ClientID)
	}
}

// Every origin has to render to something. A blank in a listing of who
// decided what reads as a decision nobody made.
func TestEveryOriginSaysSomething(t *testing.T) {
	for name, origin := range map[string]domain.RunOrigin{
		"the machine":       domain.FromTheMachine("jingclaw-cli"),
		"a named account":   domain.FromAPlatformAccount("discord", "77", "Alice"),
		"an unnamed accont": domain.FromAPlatformAccount("discord", "77", ""),
		"a room":            domain.FromAChannel("discord", "1477"),
		"nothing at all":    {},
	} {
		if described := origin.Describe(); described == "" {
			t.Errorf("%s described itself as nothing", name)
		}
	}
}

// The name is what a person reading a log recognises; the id is what
// authorisation is checked against. Shown, the name wins.
func TestANameIsPreferredToAnIdentifier(t *testing.T) {
	named := domain.FromAPlatformAccount("discord", "900000000000000042", "Alice")
	if described := named.Describe(); described != "Alice" {
		t.Errorf("described as %q, want Alice", described)
	}

	unnamed := domain.FromAPlatformAccount("discord", "900000000000000042", "")
	if described := unnamed.Describe(); described != "discord:900000000000000042" {
		t.Errorf("described as %q", described)
	}
}
