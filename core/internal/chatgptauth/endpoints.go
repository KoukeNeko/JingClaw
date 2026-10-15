// Package chatgptauth signs this deployment in with ChatGPT, so that a model
// can be served from the person's own ChatGPT plan.
//
// Implemented from OpenAI's documentation of the flow for open-source and
// locally hosted apps (developers.openai.com/siwc/token-sharing-open-source)
// and tested against fake servers built from it. It has not been run against
// the live service. Where the documentation is silent a comment says so.
//
// Everything this package holds that is worth stealing - the issued client id,
// the host id, the tokens - lives in one owner-only file and nowhere else. It
// is never logged, never part of an error, and never in a reply.
package chatgptauth

import (
	"net/url"
	"os"
	"strings"
)

const (
	// FileName is the one file everything is kept in, directly inside the
	// deployment directory. config reserves the name so that no credential
	// slot can ever be pointed at it.
	FileName = "chatgpt-auth.json"

	// DefaultAuthBase is the authorization server, and also the issuer an ID
	// token must name.
	DefaultAuthBase = "https://auth.openai.com"

	// DefaultAPIBase is where models and responses are served.
	DefaultAPIBase = "https://api.openai.com/v1"

	// Resource is the audience of the access token. A constant rather than the
	// API base, so a test double on another address still asks for what the
	// documentation says to ask for.
	Resource = "https://api.openai.com/v1"

	// RegistrationClientID is the client id of the first authorization, which
	// registers this host. The issued id replaces it; it is never used for a
	// token exchange or a refresh.
	RegistrationClientID = "dynamic_agent_client"

	// AgentName is shown on the consent screen of a registration.
	AgentName = "JingClaw"

	// ScopePlanUsage is the scope without which inference on the person's plan
	// is not allowed.
	ScopePlanUsage = "chatgpt.tokens.use.direct"

	requestedScope = "openid profile email offline_access resource.invoke " + ScopePlanUsage

	// AuthURLEnv and APIURLEnv point the two services at test doubles, so that
	// end-to-end scripts can run without the real ones. Honoured only for
	// http on a loopback host.
	AuthURLEnv = "JINGCLAW_CHATGPT_AUTH_URL"
	APIURLEnv  = "JINGCLAW_CHATGPT_API_URL"
)

// Endpoints are the two services this talks to.
type Endpoints struct {
	// Auth is the authorization server's base address, without a trailing
	// slash. Its paths are the documented ones.
	Auth string

	// API is the base the models and responses paths hang off.
	API string
}

// DefaultEndpoints are OpenAI's.
func DefaultEndpoints() Endpoints {
	return Endpoints{Auth: DefaultAuthBase, API: DefaultAPIBase}
}

// EndpointsFromEnv is DefaultEndpoints, except where the environment names a
// test double. A variable naming anything but http on 127.0.0.1 or localhost
// is ignored, so that setting one can never send a token somewhere else.
func EndpointsFromEnv() Endpoints {
	ep := DefaultEndpoints()
	if base, ok := loopbackHTTP(os.Getenv(AuthURLEnv)); ok {
		ep.Auth = base
	}
	if base, ok := loopbackHTTP(os.Getenv(APIURLEnv)); ok {
		ep.API = base
	}
	return ep
}

func loopbackHTTP(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil {
		return "", false
	}
	if host := parsed.Hostname(); host != "127.0.0.1" && host != "localhost" {
		return "", false
	}
	return strings.TrimRight(parsed.String(), "/"), true
}
