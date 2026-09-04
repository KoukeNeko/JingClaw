package domain_test

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// A Discord principal is written as <@id>, which is what the platform
// renders as a mention and what a reply can use to reach them. Elsewhere the
// display name; nowhere a name nobody gave.
func TestAPrincipalIsWrittenTheWayItsPlatformAddressesThem(t *testing.T) {
	for _, tc := range []struct {
		principal domain.ExternalPrincipal
		want      string
	}{
		{domain.ExternalPrincipal{Platform: "discord", PrincipalID: "123", DisplayName: "doeshing"}, "<@123>"},
		{domain.ExternalPrincipal{Platform: "discord", DisplayName: "doeshing"}, "doeshing"},
		{domain.ExternalPrincipal{Platform: "telegram", PrincipalID: "456", DisplayName: "roc"}, "roc"},
		{domain.ExternalPrincipal{Platform: "telegram", PrincipalID: "456"}, "456"},
		{domain.ExternalPrincipal{}, ""},
	} {
		if got := tc.principal.Mention(); got != tc.want {
			t.Errorf("%+v written as %q, want %q", tc.principal, got, tc.want)
		}
	}
}
