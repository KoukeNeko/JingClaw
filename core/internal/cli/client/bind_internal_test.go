package client

import (
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

// The list must not say "nobody" for a binding that accepts the whole room.
//
// A binding naming nobody defers to the channel's own membership, so printing
// "nobody" there hid the one configuration an operator most needs to see. A
// binding naming only roles is the case that went wrong twice: the list looked
// at accounts alone.
func TestTheListSaysWhoMayTrigger(t *testing.T) {
	cases := []struct {
		name    string
		binding *controlv1.Binding
		want    string
	}{
		{
			name:    "naming nobody is the whole room",
			binding: &controlv1.Binding{},
			want:    "anyone the platform lets in",
		},
		{
			name:    "accounts",
			binding: &controlv1.Binding{AllowedPrincipals: []string{"111", "222"}},
			want:    "111, 222",
		},
		{
			name: "roles alone are not nobody",
			binding: &controlv1.Binding{AllowedClaims: []*controlv1.PrincipalClaim{
				{Namespace: "discord.role", Value: "333"},
			}},
			want: "discord.role:333",
		},
		{
			name: "accounts and roles together",
			binding: &controlv1.Binding{
				AllowedPrincipals: []string{"111"},
				AllowedClaims: []*controlv1.PrincipalClaim{
					{Namespace: "discord.role", Value: "333"},
				},
			},
			want: "111, discord.role:333",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := whoMayTrigger(tc.binding); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
