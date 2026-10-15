#!/bin/sh
# Proves Sign in with ChatGPT works end to end against fake servers, and that
# no token reaches a log, the database or a reply on the way.
#
# The unit tests cover the flow, the provider and the error mapping. What they
# cannot see is the daemon: whether the chatgpt backend with nobody signed in
# starts a repair daemon that says where to sign in, whether the sign-in
# service is served there, whether the loopback listener a real process opens
# receives the redirect, whether the agent that starts after a restart is
# served from the stored sign-in, and whether a token appears anywhere it should
# not.
#
# Built from OpenAI's documentation and run against stand-ins for the
# authorization server and the Responses API (scripts/support/fake-chatgpt.go),
# wired in through the two environment variables the daemon honours only for
# http on a loopback host. Nothing here contacts OpenAI, and it says nothing
# about how the live service behaves.
set -eu

# Nothing inherited may name a deployment, supply a document, or hold a
# credential: the settings below are the only ones this check's daemon is given.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done
unset GEMINI_API_KEY GOOGLE_API_KEY ANTHROPIC_API_KEY BRAVE_SEARCH_TOKEN DISCORD_BOT_TOKEN TELEGRAM_BOT_TOKEN

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
CLI="$WORK/jingclaw"
go build -o "$CLI" ./cmd/jingclaw
go build -o "$WORK/fake-chatgpt" ../scripts/support/fake-chatgpt.go

PORT=7860
BASE="http://127.0.0.1:$PORT"

# Values nothing else would say.
ACCESS="e2e-access-canary-Zq7Tn2Wb9Hc4"
REFRESH="e2e-refresh-canary-Xs8Kd1Pm3Lv5"
CLIENT="oaiapp_e2e-fake-client"

DAEMON=""
FAKE=""
cleanup() {
	# Best effort from here: see verify-credentials.sh.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$FAKE" ] && kill "$FAKE" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

"$WORK/fake-chatgpt" "$WORK/fake.url" "$ACCESS" "$REFRESH" >"$WORK/fake.log" 2>&1 &
FAKE=$!
WAITED=0
while [ ! -s "$WORK/fake.url" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stand-in servers did not start: $(cat "$WORK/fake.log")"
	sleep 0.1
done
FAKEURL=$(cat "$WORK/fake.url")

# A deployment of this check's own, and nothing that opens a browser.
export JINGCLAW_HOME="$WORK/home"
mkdir -p "$JINGCLAW_HOME"
chmod 700 "$JINGCLAW_HOME"
export JINGCLAW_CHATGPT_AUTH_URL="$FAKEURL"
export JINGCLAW_CHATGPT_API_URL="$FAKEURL/v1"
export JINGCLAW_NO_BROWSER=1

export JINGCLAW_CONFIG="[provider]
backend = \"chatgpt\"

[provider.chatgpt]
model = \"fake-gpt\"

[server]
addr = \"127.0.0.1:$PORT\"
"

discovered() {
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$JINGCLAW_HOME/run/daemon.json" "$1"
}

start() {
	rm -f "$JINGCLAW_HOME/run/daemon.json"
	"$CLI" daemon >"$WORK/$1.out" 2>"$WORK/$1.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$JINGCLAW_HOME/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/$1.err")"
		sleep 0.1
	done
	TOKEN=$(discovered token)
}
stop() {
	kill "$DAEMON"
	wait "$DAEMON" 2>/dev/null || true
	DAEMON=""
}

call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer ${3:-$TOKEN}" -d "$2" \
		"$BASE/jingclaw.control.v1.$1"
}
field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }
fake() { curl -s "$FAKEURL/_state" | field "$1"; }

mode() { python3 -c 'import os,sys;print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$1"; }
AUTHFILE="$JINGCLAW_HOME/chatgpt-auth.json"

# nowhere says no token is in any daemon's output, the database that holds the
# settings and the log, or anything this check collected.
nowhere() {
	for WHERE in "$WORK"/*.out "$WORK"/*.err "$WORK"/*.txt "$JINGCLAW_HOME"/data/*; do
		[ -f "$WHERE" ] || continue
		for VALUE in "$ACCESS" "$REFRESH" "$CLIENT"; do
			if grep -qF -- "$VALUE" "$WHERE"; then
				fail "a token or the issued client id is in $WHERE"
			fi
		done
	done
}

# signin METHOD signs in, by the loopback redirect when METHOD is loopback and
# by pasting the address the browser ended on when it is paste, and waits for
# the flow to say so.
signin() {
	STARTED=$(call ChatGPTAuthService/StartChatGPTSignIn '{}')
	FLOW=$(printf '%s' "$STARTED" | field 'd["flowId"]')
	URL=$(printf '%s' "$STARTED" | field 'd["authorizationUrl"]')
	REDIRECT=$(printf '%s' "$STARTED" | field 'd["redirectUri"]')
	case "$REDIRECT" in
		http://127.0.0.1:*/auth/callback) ;;
		*) fail "the redirect is not a loopback address: $REDIRECT" ;;
	esac

	if [ "$1" = loopback ]; then
		# The browser: the authorization server sends it to the listener, which
		# the daemon opened before it handed out the address.
		FINAL=$(curl -s -L "$URL")
		case "$FINAL" in
			*"Signed in"*) ;;
			*) fail "the listener did not accept the redirect: $FINAL" ;;
		esac
	else
		# A browser on another machine: its redirect goes nowhere, and the
		# address it ends on is pasted back.
		ADDRESS=$(curl -s -o /dev/null -D - "$URL" | sed -n 's/^[Ll]ocation: *//p' | tr -d '\r')
		[ -n "$ADDRESS" ] || fail "the authorization server did not redirect"
		REPLY=$(call ChatGPTAuthService/CompleteChatGPTSignIn "$(python3 -c 'import json,sys;print(json.dumps({"flowId": sys.argv[1], "redirectedAddress": sys.argv[2]}))' "$FLOW" "$ADDRESS")")
		case "$REPLY" in
			*SUCCEEDED*) ;;
			*) fail "pasting the address did not sign in: $REPLY" ;;
		esac
	fi

	WAITED=0
	until [ "$(call ChatGPTAuthService/GetChatGPTSignIn "{\"flowId\":\"$FLOW\"}" | field 'd.get("state")')" = CHAT_GPT_SIGN_IN_STATE_SUCCEEDED ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 50 ] && fail "the sign-in never succeeded"
		sleep 0.1
	done
}

# ── 1. nobody is signed in: repair mode, with the sign-in to hand ──────

start first

STATE=$(call ConfigService/GetAgentState '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] || fail "a chatgpt backend with nobody signed in is not in repair mode: $STATE"
printf '%s' "$STATE" | field 'd["problems"][0]["why"]' | grep -q 'not signed in' || fail "the reason does not say nobody is signed in: $STATE"
printf '%s' "$STATE" | field 'd["problems"][0]["fix"]' | grep -qi 'dashboard' || fail "the fix does not name the dashboard: $STATE"
printf 'ok   with nobody signed in the daemon is in repair mode and says to sign in from the dashboard\n'

ACCOUNT=$(call ChatGPTAuthService/GetChatGPTAccount '{}')
[ "$(printf '%s' "$ACCOUNT" | field 'd.get("signedIn", False)')" = False ] || fail "somebody is signed in already: $ACCOUNT"
WRONG=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' -H "authorization: Bearer not-a-credential" -d '{}' "$BASE/jingclaw.control.v1.ChatGPTAuthService/GetChatGPTAccount")
[ "$WRONG" = 401 ] || fail "the sign-in service answered a made-up credential with $WRONG"
printf 'ok   the sign-in service is served in repair mode, behind the credential check\n'

# ── 2. signing in through the loopback listener ────────────────────────

signin loopback

ACCOUNT=$(call ChatGPTAuthService/GetChatGPTAccount '{}')
printf '%s' "$ACCOUNT" >"$WORK/account.txt"
[ "$(printf '%s' "$ACCOUNT" | field 'd["signedIn"]')" = True ] || fail "not signed in: $ACCOUNT"
[ "$(printf '%s' "$ACCOUNT" | field 'd["planUsageGranted"]')" = True ] || fail "plan usage is not granted: $ACCOUNT"
[ "$(printf '%s' "$ACCOUNT" | field 'd["label"]')" = e2e@example.test ] || fail "the account is not labelled by its ID token: $ACCOUNT"
[ "$(printf '%s' "$ACCOUNT" | field 'd["restartNeeded"]')" = True ] || fail "a restart is not asked for: $ACCOUNT"
printf '%s' "$ACCOUNT" | field '"accessTokenExpiresAt" in d' | grep -q True || fail "no expiry is reported: $ACCOUNT"
case "$ACCOUNT" in
	*"$ACCESS"*|*"$REFRESH"*|*"$CLIENT"*|*urn:uuid*) fail "the account carries a secret or an identifier: $ACCOUNT" ;;
esac
[ "$(fake 'd["registrations"]')" = 1 ] || fail "the host was not registered through the first authorization"
[ "$(fake 'd["exchanges"]')" = 1 ] || fail "the code was not exchanged once"
printf 'ok   the first sign-in registers, the redirect reaches the listener, and the account says who and what, with no secret\n'

[ "$(mode "$AUTHFILE")" = 600 ] || fail "the sign-in file is mode $(mode "$AUTHFILE")"
grep -qF "$CLIENT" "$AUTHFILE" || fail "the issued client id was not saved"
grep -q 'urn:uuid:' "$AUTHFILE" || fail "no host id was saved"
LEFT=$(ls -A "$JINGCLAW_HOME" | grep -E '^\.chatgpt-auth' || true)
[ -z "$LEFT" ] || fail "a save left a temporary file: $LEFT"
printf 'ok   everything is in one mode-600 file\n'

MODELS=$(call ChatGPTAuthService/ListChatGPTModels '{}')
printf '%s' "$MODELS" | grep -q 'fake-gpt' || fail "the model is not listed: $MODELS"
printf '%s' "$MODELS" | grep -q 'fake-hidden' && fail "a model not meant for display is listed: $MODELS"
printf 'ok   the models the account can use are listed, without the ones not meant for display\n'
nowhere
stop

# ── 3. after a restart the agent runs on the stored sign-in ────────────

start second
[ -z "$(discovered mode)" ] || fail "the daemon is still in repair mode: $(cat "$WORK/second.err")"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] || fail "the agent is not running"
[ "$(call ChatGPTAuthService/GetChatGPTAccount '{}' | field 'd.get("restartNeeded", False)')" = False ] || fail "a restart is still asked for"

SESSION=$(call SessionService/CreateSession '{"meta":{"clientId":"verify"},"title":"chatgpt"}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "no session was created"
"$CLI" send "$SESSION" "hello over chatgpt" >/dev/null
sleep 2
"$CLI" attach "$SESSION" >"$WORK/events.txt" 2>&1 &
ATTACH=$!
sleep 1
kill "$ATTACH" 2>/dev/null || true
grep 'assistant.delta' "$WORK/events.txt" | grep -q 'fake chatgpt heard: hello over chatgpt' ||
	fail "the reply was not streamed from the stand-in: $(cat "$WORK/events.txt")"
grep -q 'run.completed' "$WORK/events.txt" || fail "the run did not complete: $(cat "$WORK/events.txt")"
[ "$(fake 'd["responses"]')" -ge 1 ] || fail "the stand-in was never asked for a response"
[ "$(fake 'd["store_false_stream_true"]')" = True ] || fail "a request was sent without store:false and stream:true"
fake 'd["last_response_body"]' | python3 -c '
import json, sys
sent = json.loads(sys.stdin.read())
bad = [k for k in ("temperature", "top_p", "max_output_tokens", "metadata", "user", "truncation", "previous_response_id") if k in sent]
assert not bad, "the request carries " + ", ".join(bad)
assert isinstance(sent["input"], list) and sent.get("instructions"), "history or instructions are missing"
assert all(i.get("role") != "system" for i in sent["input"]), "a system item was sent"
' || fail "the request is not the documented shape"
printf 'ok   after a restart a message gets an answer streamed from the stand-in, in the documented request shape\n'
nowhere

# ── 4. sign-out revokes and clears; signing in again reuses the registration

OUT=$(call ChatGPTAuthService/SignOutChatGPT '{}')
[ "$(printf '%s' "$OUT" | field 'd.get("revoked", False)')" = True ] || fail "the revocation was not confirmed: $OUT"
[ "$(fake 'd["revocations"]')" = 1 ] || fail "nothing was revoked"
[ "$(fake 'd["revoked_token_is_refresh"]')" = True ] || fail "what was revoked is not the refresh token"
grep -qF "$ACCESS" "$AUTHFILE" && fail "an access token is still in the file"
grep -qF "$REFRESH" "$AUTHFILE" && fail "the refresh token is still in the file"
grep -qF "$CLIENT" "$AUTHFILE" || fail "the registration was thrown away"
[ "$(call ChatGPTAuthService/GetChatGPTAccount '{}' | field 'd.get("signedIn", False)')" = False ] || fail "still signed in"
printf 'ok   signing out revokes the refresh token, clears the tokens, and keeps the registration\n'

signin paste
[ "$(fake 'd["registrations"]')" = 1 ] || fail "signing in again registered a second time"
[ "$(call ChatGPTAuthService/GetChatGPTAccount '{}' | field 'd["signedIn"]')" = True ] || fail "pasting the address did not sign in"
BAD=$(call ChatGPTAuthService/StartChatGPTSignIn '{}' | field 'd["flowId"]')
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"flowId\":\"$BAD\",\"redirectedAddress\":\"http://127.0.0.1:1/auth/callback?code=x&state=wrong\"}" \
	"$BASE/jingclaw.control.v1.ChatGPTAuthService/CompleteChatGPTSignIn")
[ "$CODE" = 400 ] || fail "an address with another state was answered $CODE"
[ "$(fake 'd["exchanges"]')" = 2 ] || fail "a code was exchanged for an address whose state did not match"
printf 'ok   signing in again pastes the address back, reuses the registration, and a wrong state exchanges nothing\n'

call ChatGPTAuthService/SignOutChatGPT '{}' >/dev/null
nowhere
stop
printf 'ok   no token reached a log, the database or a reply\n'
