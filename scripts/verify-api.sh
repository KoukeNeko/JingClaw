#!/bin/sh
# Proves the control API is reachable only by a credential that may reach it.
#
# This replaces verify-console.sh, which checked the browser pairing flow as
# well. The console is gone; what it was protecting is not. Three of its
# assertions survive because they were never about the browser:
#
#   an uncredentialed call is refused
#   a name that resolves here is refused even with one
#   a gateway credential cannot reach what an operator's can
#
# And two because a second client finds transport bugs one client never does:
# a unary Connect call as plain JSON, and a streaming one in length-prefixed
# frames. The TUI will use the same transport.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7791"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE="http://127.0.0.1:7791"
LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

# The scopes a credential can have. Named here so that removing one is a
# change to this line rather than something that quietly widens what the
# remaining ones reach.
[ -z "$("$WORK/agentd" --print-config | grep -E '^\s*#?\s*(web_console|pairing_ttl|console_ttl)')" ] ||
	fail "the configuration still offers a browser console"
printf 'ok   no browser console is configurable\n'

grep -q 'console' "$WORK/daemon.out" &&
	fail "the daemon still advertises a console: $(cat "$WORK/daemon.out")"
printf 'ok   and the daemon does not advertise one\n'

# 1. Everything needs a credential.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H 'content-type: application/json' -d '{}' \
	"$BASE/jingclaw.control.v1.SessionService/ListSessions")
[ "$STATUS" = 401 ] || fail "an uncredentialed API call returned $STATUS, want 401"
printf 'ok   the API refuses an uncredentialed call\n'

# 2. A name that resolves here is refused, credential or not. Without this a
#    page on any website can drive this daemon through the browser.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com:7791' \
	-H "authorization: Bearer $LOCAL" -X POST -H 'content-type: application/json' -d '{}' \
	"$BASE/jingclaw.control.v1.SessionService/ListSessions")
[ "$STATUS" = 403 ] || fail "a rebound host reached the API: $STATUS"
printf 'ok   and a rebound host, even with one\n'

# 3. The gateway's credential reaches the ingress and nothing else. A process
#    holding a bot token must not be able to execute tools.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H 'content-type: application/json' -H "authorization: Bearer $GATEWAY" -d '{}' \
	"$BASE/jingclaw.control.v1.SessionService/ListSessions")
[ "$STATUS" = 403 ] || fail "the gateway credential reached SessionService: $STATUS"
printf 'ok   a gateway credential reaches only the ingress\n'

# 4. A unary Connect call is a POST with a JSON body.
SESSION=$(curl -s -X POST \
	-H 'content-type: application/json' \
	-H "authorization: Bearer $LOCAL" \
	-d '{"meta":{"clientId":"verify"},"title":"over json"}' \
	"$BASE/jingclaw.control.v1.SessionService/CreateSession" |
	sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "creating a session over JSON produced no id"
printf 'ok   a unary call works as plain JSON over POST\n'

"$WORK/agent" --config "$WORK/config.toml" send "$SESSION" "hello from the terminal" >/dev/null
sleep 1

# 5. A streaming call: the request is enveloped too, and the answer comes back
#    in length-prefixed frames. Getting this wrong is what a second client
#    finds and one client never does.
python3 - "$SESSION" "$WORK/framed" <<'FRAME'
import json, struct, sys

# The five-byte prefix is one flag byte then a big-endian length. Written here
# rather than in shell because getting it wrong by one byte is exactly the
# mistake this check exists to catch.
body = json.dumps({
    "meta": {"clientId": "verify"},
    "sessionId": sys.argv[1],
    "afterSeq": "0",
}).encode()

with open(sys.argv[2], "wb") as out:
    out.write(struct.pack(">BI", 0, len(body)))
    out.write(body)
FRAME

curl -s -X POST --data-binary "@$WORK/framed" \
	-H 'content-type: application/connect+json' \
	-H 'connect-protocol-version: 1' \
	-H "authorization: Bearer $LOCAL" \
	--max-time 3 \
	"$BASE/jingclaw.control.v1.SessionService/SubscribeEvents" > "$WORK/stream" 2>/dev/null || true

grep -q 'hello from the terminal' "$WORK/stream" ||
	fail "the streamed events do not carry the turn sent from the terminal:
$(head -c 400 "$WORK/stream")"
printf 'ok   a streaming call works, and carries what the CLI sent\n'

printf '\nall checks passed\n'
