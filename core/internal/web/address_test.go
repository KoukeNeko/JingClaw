package web_test

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

// resolveTo builds a resolver that answers with fixed addresses, so the checks
// do not depend on what DNS says today.
func resolveTo(addresses ...string) func(string) ([]net.IP, error) {
	return func(string) ([]net.IP, error) {
		ips := make([]net.IP, 0, len(addresses))
		for _, address := range addresses {
			ips = append(ips, net.ParseIP(address))
		}
		return ips, nil
	}
}

// The addresses that must never be reachable, whatever the hostname says.
//
// A name is somebody else's record. Refusing the string "localhost" catches
// none of this, which is why the check is on what the name resolves to.
func TestRefusesAddressesInsideThisNetwork(t *testing.T) {
	tests := []struct {
		name    string
		address string
		mention string
	}{
		{"this machine", "127.0.0.1", "this machine"},
		{"this machine over v6", "::1", "this machine"},
		{"a home network", "192.168.1.10", "private"},
		{"a corporate network", "10.0.0.5", "private"},
		{"the other private range", "172.16.3.9", "private"},
		{"cloud metadata", "169.254.169.254", "metadata"},
		{"carrier-grade NAT", "100.64.1.1", "NAT"},
		{"unspecified", "0.0.0.0", "unspecified"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A perfectly ordinary public hostname, pointed inward. This is
			// how the metadata service is actually reached.
			_, err := web.CheckURL("https://docs.example.com/page", resolveTo(test.address))

			var refused *web.ErrRefused
			if !errors.As(err, &refused) {
				t.Fatalf("%s was accepted", test.address)
			}
			if !strings.Contains(refused.Reason, test.mention) {
				t.Errorf("the refusal does not say why: %q", refused.Reason)
			}
		})
	}
}

// One public address among private ones is the whole trick: a resolver that
// answers with both, and a check that looks only at the first, reaches the
// private one whenever the order comes out the other way.
func TestRefusesWhenAnyAddressIsPrivate(t *testing.T) {
	_, err := web.CheckURL("https://docs.example.com", resolveTo("93.184.216.34", "127.0.0.1"))

	var refused *web.ErrRefused
	if !errors.As(err, &refused) {
		t.Fatal("a name resolving to both public and private addresses was accepted")
	}
}

func TestRefusesSchemesThatAreNotTheWeb(t *testing.T) {
	for _, address := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x",
		"jingclaw://do-something",
		"data:text/html,<script>",
	} {
		if _, err := web.CheckURL(address, resolveTo("93.184.216.34")); err == nil {
			t.Errorf("%s was accepted", address)
		}
	}
}

// Credentials in a URL hand a site somebody's password, and make a host look
// like one thing while being another.
func TestRefusesCredentialsInTheAddress(t *testing.T) {
	_, err := web.CheckURL("https://user:secret@evil.example.com/", resolveTo("93.184.216.34"))
	if err == nil {
		t.Fatal("an address carrying credentials was accepted")
	}
	// The refusal is logged, put in the model's context, and sometimes posted
	// back to a chat channel. It must not carry the password with it.
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the refusal repeats the password: %v", err)
	}
	if !strings.Contains(err.Error(), "evil.example.com") {
		t.Errorf("the refusal no longer says which host: %v", err)
	}
}

func TestRedactAddress(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://user:pw@host.example/path?q=1", "https://[redacted]@host.example/path?q=1"},
		{"https://user@host.example", "https://[redacted]@host.example"},
		{"https://host.example/path", "https://host.example/path"},
		{"not a url at all", "not a url at all"},
		// The @ belongs to the path here, not to any credentials.
		{"https://host.example/mail@example", "https://host.example/mail@example"},
	}

	for _, test := range tests {
		if got := web.RedactAddress(test.in); got != test.want {
			t.Errorf("RedactAddress(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestAcceptsAnOrdinaryPublicAddress(t *testing.T) {
	parsed, err := web.CheckURL("https://example.com/docs?q=1", resolveTo("93.184.216.34"))
	if err != nil {
		t.Fatalf("an ordinary address was refused: %v", err)
	}
	if parsed.Host != "example.com" {
		t.Errorf("host is %q", parsed.Host)
	}
}

func TestRefusesAHostThatDoesNotResolve(t *testing.T) {
	failing := func(string) ([]net.IP, error) { return nil, errors.New("no such host") }

	if _, err := web.CheckURL("https://nowhere.invalid", failing); err == nil {
		t.Fatal("an unresolvable host was accepted")
	}
}

func TestCollapseWhitespaceKeepsParagraphs(t *testing.T) {
	got := web.CollapseWhitespace("Title   \n\n\n\n\nFirst line\nSecond line\n\n\n  \n\nEnd\n\n")
	want := "Title\n\nFirst line\nSecond line\n\nEnd"

	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}
