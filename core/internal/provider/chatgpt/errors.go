package chatgpt

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

const providerName = "chatgpt"

// Codes the documentation lists (errors-and-recovery). The list is not
// exhaustive, so anything else falls through to the HTTP status.
const (
	codeUsageLimit     = "subscription_sharing_usage_limit_exceeded"
	codeUsageUnavail   = "subscription_sharing_usage_unavailable"
	codeUserUnavail    = "subscription_sharing_user_unavailable"
	codeNotEligible    = "subscription_sharing_user_not_eligible"
	codeUnsupported    = "subscription_sharing_unsupported_capability"
	codeRouteNotSupp   = "subscription_sharing_route_not_supported"
	codeInvalidUser    = "subscription_sharing_invalid_user"
	codeScopeNotAuth   = "chatpass_v2_scope_not_authorized"
	codeInvalidContext = "chatpass_v2_invalid_authorization_context"
)

// failure maps a documented error to what the runtime should do about it.
//
// Nothing here moves a request to another way of paying: a usage limit is a
// stop, not a reason to try something else. The service's own message is kept,
// short, because it is diagnostic and is the only place a reason is given.
func failure(code, param, message string, status int) *provider.Error {
	kind := kindForStatus(status)
	said := ""

	switch code {
	case codeUsageLimit:
		kind = provider.KindQuotaExhausted
		said = "your ChatGPT plan's usage limit has been reached; see ChatGPT Settings, Usage. " +
			"JingClaw does not know when it resets"
	case codeUsageUnavail, codeUserUnavail:
		kind = provider.KindOverloaded
		said = "ChatGPT could not check plan usage right now; retrying with a pause"
	case codeNotEligible:
		kind = provider.KindAuth
		said = "ChatGPT plan usage is not available for this account, workspace or policy"
	case codeUnsupported:
		kind = provider.KindInvalidRequest
		said = "ChatGPT rejected part of the request as unsupported"
		if param != "" {
			said += " (" + param + ")"
		}
		if strings.Contains(param, "tool") {
			// The wire shape of function tools is the least certain part of this
			// provider (see wire.go), so say plainly that it may be the cause.
			said += "; the way JingClaw declares tools may be what OpenAI refuses (see docs/using-it.md, Sign in with ChatGPT)"
		}
	case codeRouteNotSupp:
		kind = provider.KindInvalidRequest
		said = "ChatGPT does not support this route or method"
	case codeInvalidUser:
		kind = provider.KindAuth
		said = "ChatGPT could not validate the signed-in account"
	case codeScopeNotAuth, codeInvalidContext:
		kind = provider.KindAuth
		said = "ChatGPT did not authorize this sign-in for plan usage; sign in again and allow it"
	}

	if said == "" {
		said = firstLine(message)
	} else if code != "" {
		said += " [" + code + "]"
	}
	return &provider.Error{Provider: providerName, Kind: kind, StatusCode: status, Message: said}
}

func kindForStatus(status int) provider.ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.KindRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.KindAuth
	case status == http.StatusServiceUnavailable:
		return provider.KindOverloaded
	case status >= 500:
		return provider.KindTransient
	case status == http.StatusNotFound:
		return provider.KindNotFound
	case status == http.StatusBadRequest:
		return provider.KindInvalidRequest
	}
	return provider.KindUnknown
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// code reads the error out of a failed response body: {"error":{code,message,
// param}}, or the {"detail": "..."} the documentation says direct-route
// admission may return, whose text is diagnostic only.
func errorOf(body []byte) (code, param, message string) {
	var reported struct {
		Error struct {
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &reported) != nil {
		return "", "", ""
	}
	message = reported.Error.Message
	if message == "" {
		var detail string
		if json.Unmarshal(reported.Detail, &detail) == nil {
			message = detail
		}
	}
	return reported.Error.Code, reported.Error.Param, message
}

// statusError reads a failed HTTP response.
func statusError(response *http.Response, body []byte) *provider.Error {
	code, param, message := errorOf(body)
	perr := failure(code, param, message, response.StatusCode)
	perr.RequestID = response.Header.Get("x-request-id")

	// Only a stated delay is used. A usage limit never gets one: nothing in the
	// documentation says when it resets, and a guess would be retried against.
	if code != codeUsageLimit {
		if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds >= 0 {
			after := time.Duration(seconds) * time.Second
			perr.RetryAfter = &after
		}
	}
	return perr
}
