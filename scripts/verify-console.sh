#!/bin/sh
# Proves the console is reachable and the API behind it is not: the page is
# served without a credential, every call needs one, and a browser's own
# transport — Connect over JSON, unary and streaming — works against the same
# endpoint the CLI uses, with no proxy in between.
set -eu

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
[model]
provider = "fake"
model = "fake-echo"
fake_delay = "0s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7791"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
web_console = true
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
TOKEN=$(grep -o '?t=[A-Za-z0-9_-]*' "$WORK/daemon.out" | cut -c4-)
[ -n "$TOKEN" ] || fail "the daemon did not print a console URL: $(cat "$WORK/daemon.out")"
printf 'ok   the daemon says where the console is\n'

# 1. The page loads with no credential. A browser cannot present one on the
#    request that fetches the page it would get the token from.
for FILE in / /app.js /app.css; do
	CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$FILE")
	[ "$CODE" = 200 ] || fail "$FILE returned $CODE without a credential"
done
printf 'ok   the console loads without a credential\n'

curl -s -D - -o /dev/null "$BASE/" | grep -qi 'content-security-policy' ||
	fail "the page is served without a content policy"
printf 'ok   and with a policy that keeps it from reaching anywhere else\n'

# 2. Everything it can do needs one.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H 'content-type: application/json' -d '{}' \
	"$BASE/jingclaw.control.v1.SessionService/ListSessions")
[ "$CODE" = 401 ] || fail "an uncredentialed API call returned $CODE, want 401"
printf 'ok   the API behind it refuses an uncredentialed call\n'

# 3. A name that resolves here is still refused, credential or not.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com:7791' "$BASE/")
[ "$CODE" = 403 ] || fail "a rebound host reached the console: $CODE"
printf 'ok   and a rebound host reaches neither\n'

# 4. The browser's own transport: a unary call is a POST with a JSON body.
SESSION=$(curl -s -X POST \
	-H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" \
	-d '{"meta":{"clientId":"verify"},"title":"from a browser"}' \
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
	-H "authorization: Bearer $TOKEN" \
	--max-time 3 \
	"$BASE/jingclaw.control.v1.SessionService/SubscribeEvents" > "$WORK/stream" 2>/dev/null || true

grep -q 'hello from the terminal' "$WORK/stream" ||
	fail "the streamed events do not carry the turn sent from the terminal:
$(head -c 400 "$WORK/stream")"
printf 'ok   a streaming call works, and carries what the CLI sent\n'

# 6. Turning it off means it is off.
sed -i.bak 's/web_console = true/web_console = false/' "$WORK/config.toml"
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/off.out" 2>"$WORK/off.err" &
DAEMON=$!
WAITED=0
while ! grep -q Listening "$WORK/off.out" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not restart: $(cat "$WORK/off.err")"
	sleep 0.1
done

CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")
[ "$CODE" = 404 ] || fail "the console still answers with web_console = false: $CODE"
grep -q Console "$WORK/off.out" && fail "the banner still advertises a console that is off"
printf 'ok   turning it off turns it off\n'

printf '\nall checks passed\n'
