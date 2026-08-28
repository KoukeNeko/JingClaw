package openaicompat

import (
	"net/http"
	"sort"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Profile carries what one server does differently from the protocol it says
// it implements.
//
// Named in configuration rather than inferred from the address. A reverse
// proxy, a tunnel or a gateway makes the URL say nothing about what is behind
// it, and guessing wrong here means guessing wrong about whether a failure is
// worth retrying.
type Profile struct {
	Name string

	// AsksForUsage sends stream_options.include_usage. Harmless where it is
	// not understood, and the only way to be told on the servers that need
	// asking.
	AsksForUsage bool

	// ClassifyStatus overrides the default reading of an HTTP status. It
	// exists because status codes are where compatibility breaks worst: one
	// server answers 403 for a prompt longer than the context, which under
	// the ordinary reading is a permissions failure nobody can fix.
	//
	// Returning KindUnknown falls through to the default.
	ClassifyStatus func(status int, body *wireError) provider.ErrorKind
}

// Profiles are the servers whose differences are known. Generic is the default
// and does nothing: it exists so that adding a quirk later does not mean
// changing every caller.
var profiles = map[string]Profile{
	"generic": {
		Name:         "generic",
		AsksForUsage: true,
	},

	"vllm": {
		Name:         "vllm",
		AsksForUsage: true,
	},

	"lmstudio": {
		// Older builds do not know stream_options, and it costs nothing to
		// ask: a server that does not recognise the field ignores it.
		Name:         "lmstudio",
		AsksForUsage: true,
	},

	"llamacpp": {
		Name:         "llamacpp",
		AsksForUsage: true,
		ClassifyStatus: func(status int, body *wireError) provider.ErrorKind {
			if status != http.StatusServiceUnavailable || body == nil {
				return provider.KindUnknown
			}
			// The same 503 covers a model still loading and a server with no
			// free slot. Both clear by waiting, but only one of them is
			// worth telling somebody about.
			if strings.Contains(strings.ToLower(body.Message), "loading") {
				return provider.KindTransient
			}
			return provider.KindOverloaded
		},
	},

	"openrouter": {
		// Usage always arrives in the final frame here without being asked.
		Name:         "openrouter",
		AsksForUsage: true,
		ClassifyStatus: func(_ int, body *wireError) provider.ErrorKind {
			// This gateway normalizes whatever the upstream provider said
			// into a type of its own, and its documentation says to read that
			// rather than the status.
			if body == nil || body.Metadata == nil {
				return provider.KindUnknown
			}
			switch body.Metadata.ErrorType {
			case "rate_limit_exceeded":
				return provider.KindRateLimited
			case "insufficient_credits", "quota_exceeded":
				return provider.KindQuotaExhausted
			case "context_length_exceeded":
				return provider.KindContextOverflow
			case "moderation":
				return provider.KindContentFiltered
			}
			return provider.KindUnknown
		},
	},

	"groq": {
		Name:         "groq",
		AsksForUsage: true,
	},

	"together": {
		Name:         "together",
		AsksForUsage: true,
		ClassifyStatus: func(status int, _ *wireError) provider.ErrorKind {
			switch status {
			case http.StatusPaymentRequired:
				return provider.KindQuotaExhausted
			case http.StatusForbidden:
				// Not a permissions failure here. This server answers 403
				// when the prompt plus max_tokens exceeds the context window,
				// which the ordinary reading would report as something no
				// amount of shortening could fix.
				return provider.KindContextOverflow
			case http.StatusServiceUnavailable:
				return provider.KindOverloaded
			}
			return provider.KindUnknown
		},
	},
}

// ProfileByName resolves a configured profile.
//
// An unknown name is refused rather than falling back to generic. Quietly
// substituting the profile that knows nothing for the one that knows how this
// server reports being out of credit is the worst way to handle a typo.
func ProfileByName(name string) (Profile, bool) {
	if name == "" {
		return profiles["generic"], true
	}
	found, ok := profiles[strings.ToLower(name)]
	return found, ok
}

// ProfileNames lists what may be configured, for an error message that helps.
func ProfileNames() []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
