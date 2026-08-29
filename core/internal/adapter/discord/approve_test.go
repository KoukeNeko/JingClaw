package discord

import "testing"

// The identifier on a button is not a secret and not a claim: Discord hands it
// back exactly as it was sent, from whoever pressed it. It therefore has to be
// a locator and nothing more, and this is the test that says so.
func TestAButtonCarriesALocatorAndNothingElse(t *testing.T) {
	id := buttonID("apr_123", buttonAllow)

	approvalID, action, ok := parseButtonID(id)
	if !ok {
		t.Fatalf("%q did not parse", id)
	}
	if approvalID != "apr_123" || action != buttonAllow {
		t.Errorf("read back %q/%q", approvalID, action)
	}

	for _, leaked := range []string{"user", "role", "admin", "allow_", "true"} {
		if contains(id, leaked) {
			t.Errorf("the button id %q carries %q, which is a claim rather than a locator",
				id, leaked)
		}
	}
}

// Discord's own limit. A longer one is rejected by the platform at post time,
// which would mean an approval that silently arrives without its controls.
func TestAButtonIdFitsDiscordsLimit(t *testing.T) {
	// Longer than any id this program mints, so the check has room to spare.
	id := buttonID("apr_01K4FQNFZXV41YF3YPK2A6C98RXXXXXXXXXX", buttonDeny)

	const discordLimit = 100
	if len(id) > discordLimit {
		t.Errorf("a button id is %d characters, over Discord's %d", len(id), discordLimit)
	}
}

// A bot may carry other buttons. Swallowing their presses would break them
// while looking like nothing happened.
func TestSomebodyElsesButtonIsLeftAlone(t *testing.T) {
	for _, customID := range []string{
		"",
		"other.feature:apr_1:allow",
		"jc.approval:apr_1",
		"jc.approval:apr_1:allow:extra",
		"jc.approval::allow",
		"jc.approval:apr_1:delete",
	} {
		if _, _, ok := parseButtonID(customID); ok {
			t.Errorf("%q was treated as ours", customID)
		}
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
