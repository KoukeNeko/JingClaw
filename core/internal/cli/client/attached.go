package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

// Credential says which of the daemon's tokens a client carries.
//
// Two, because they are not the same authority: the local one settles
// approvals and the gateway one may only hand messages inward.
type Credential int

const (
	// AsTheOperator is the local credential, which can decide.
	AsTheOperator Credential = iota

	// AsTheGateway is the one a chat adapter carries.
	AsTheGateway
)

// AtTheDaemon sends every request wherever the discovery file currently
// points, with the credential it currently holds.
//
// Resolved per request rather than once at dial, because the daemon publishes
// a fresh address and credential every time it starts. A client holding the
// first one it saw survives a restart as something worse than a dead process:
// it stays up, retries forever against a port nobody answers, and looks from
// the outside exactly like something that has decided not to work. That
// happened to the gateway for nine hours and to the console in a loop fast
// enough to fill a terminal.
//
// The file is read on every request rather than cached and invalidated. It is
// a few hundred bytes on local disk, the request rate is a handful per
// message, and a cache here would need to know when it was wrong — which is
// the thing that was missing in the first place.
type AtTheDaemon struct {
	Path string
	As   Credential
	Base http.RoundTripper
}

func (t *AtTheDaemon) RoundTrip(req *http.Request) (*http.Response, error) {
	found, err := discovery.Read(t.Path)
	if err != nil {
		return nil, fmt.Errorf("client: where the daemon is: %w", err)
	}

	token := found.Token
	if t.As == AsTheGateway {
		token = found.GatewayToken
	}
	if token == "" {
		return nil, errors.New("client: the daemon published no credential for this")
	}

	where, err := url.Parse(found.BaseURL)
	if err != nil {
		return nil, fmt.Errorf(
			"client: the daemon published an address that will not parse: %w", err)
	}

	// Rewritten rather than checked. Refusing a request whose host does not
	// match would mean rebuilding the client on every restart, which is the
	// arrangement this replaces.
	clone := req.Clone(req.Context())
	clone.URL.Scheme = where.Scheme
	clone.URL.Host = where.Host
	clone.Host = where.Host
	clone.Header.Set("Authorization", "Bearer "+token)

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
