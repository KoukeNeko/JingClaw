#!/bin/sh
# Proves the gateway is not Discord-shaped: the same daemon, the same bindings
# and the same outbox carry a run end to end over a second platform, with a
# stub standing in for Telegram so the check needs no bot and no network.
#
# What this cannot prove is that the requests are the ones Telegram accepts.
# The stub answers whatever it is asked, so a wrong method name or a missing
# field passes here. It is checked against the documented API and has never
# run against the real one.
set -eu

# A .JingClaw directory above this checkout must not decide anything here: a
# check that reaches the operator's own deployment would read its settings and,
# worse, write to its database.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/gatewayd" ./cmd/gatewayd

DAEMON=""
GATEWAY=""
STUB=""
cleanup() {
	[ -n "$GATEWAY" ] && kill "$GATEWAY" 2>/dev/null
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$STUB" ] && kill "$STUB" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHAT_ID=-1001234567890
USER_ID=77

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

# The stub records every call it is given, so the assertions are about what
# would have gone over the wire rather than about what the adapter believes it
# sent.
cat > "$WORK/stub.py" <<'PY'
import json, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, calls_path, chat_id, user_id = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), int(sys.argv[4])

lock = threading.Lock()
delivered = False
next_id = 5000


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        global delivered, next_id

        method = self.path.rsplit("/", 1)[-1]
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw) if raw else {}
        except ValueError:
            body = {"_raw": raw.decode("utf-8", "replace")}

        with lock:
            with open(calls_path, "a") as log:
                log.write(json.dumps({"method": method, "body": body}) + "\n")

            if method == "getMe":
                result = {"id": 1, "username": "jingclaw_bot"}
            elif method == "getUpdates":
                if delivered:
                    result = []
                else:
                    delivered = True
                    result = [{
                        "update_id": 1,
                        "message": {
                            "message_id": 11,
                            "from": {"id": user_id, "is_bot": False, "username": "someone",
                                     "first_name": "Someone"},
                            "chat": {"id": chat_id, "type": "private"},
                            "date": 0,
                            "text": "say something back",
                        },
                    }]
            else:
                next_id += 1
                result = {"message_id": next_id, "chat": {"id": chat_id, "type": "private"}}

        payload = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

python3 "$WORK/stub.py" 7793 "$WORK/calls.jsonl" "$CHAT_ID" "$USER_ID" 2>/dev/null &
STUB=$!

WAITED=0
while ! curl -s -o /dev/null -X POST "http://127.0.0.1:7793/botx/getMe"; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub never came up"
	sleep 0.1
done
: > "$WORK/calls.jsonl"

cat > "$WORK/config.toml" <<EOF
[model]
provider = "fake"
model = "fake-echo"
fake_delay = "0s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7792"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "telegram"
[gateway.telegram]
account_id = "main"
api_base = "http://127.0.0.1:7793"
[[gateway.telegram.channels]]
channel_ids = ["$CHAT_ID"]
tenant_id = "$CHAT_ID"
workspace_id = "default"
users = ["$USER_ID"]
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   the daemon accepts a telegram binding\n'

TELEGRAM_BOT_TOKEN=stub-token \
	"$WORK/gatewayd" --config "$WORK/config.toml" >"$WORK/gw.out" 2>"$WORK/gw.err" &
GATEWAY=$!

WAITED=0
while ! grep -q '"msg":"connected to telegram"' "$WORK/gw.err" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the gateway never connected: $(cat "$WORK/gw.err")"
	sleep 0.1
done
printf 'ok   gatewayd serves telegram when the file says so\n'

# The bot has to know its own name before a mention can match one. A gateway
# that started without it would sit in a group ignoring everybody.
grep -q '"bot_user":"jingclaw_bot"' "$WORK/gw.err" ||
	fail "the gateway did not learn its own username"
printf 'ok   and learns its own name before answering\n'

WAITED=0
while ! grep -q '"msg":"started work from a message"' "$WORK/gw.err" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the message never became a run: $(cat "$WORK/gw.err")"
	sleep 0.1
done
printf 'ok   a message from the platform becomes a run\n'

WAITED=0
while ! grep -q '"method": "sendMessage"' "$WORK/calls.jsonl" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "nothing was ever posted back: $(cat "$WORK/gw.err")"
	sleep 0.1
done
printf 'ok   and the answer is posted back to the platform\n'

python3 - "$WORK/calls.jsonl" "$CHAT_ID" <<'CHECK'
import json, sys

calls = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
chat_id = int(sys.argv[2])
sends = [c for c in calls if c["method"] == "sendMessage"]


def fail(why):
    print("FAIL: " + why, file=sys.stderr)
    raise SystemExit(1)


if not sends:
    fail("nothing was sent")

for send in sends:
    body = send["body"]
    if body.get("chat_id") != chat_id:
        fail("a message went to %r rather than the chat it came from" % body.get("chat_id"))
    if "parse_mode" in body:
        # Telegram refuses a message whose entities do not balance, and a model
        # writes an unmatched asterisk often enough that formatting would mean
        # occasionally posting nothing at all.
        fail("a message was sent with a parse mode Telegram may refuse")
    if not isinstance(body.get("text"), str) or not body["text"].strip():
        fail("an empty message was sent")

if not any("say something back" in s["body"]["text"] for s in sends):
    fail("the answer does not carry what the run produced: %r" % [s["body"]["text"] for s in sends])

# The offset has to move past what was handled, or the same message is offered
# forever and one question becomes an endless loop of runs.
polls = [c for c in calls if c["method"] == "getUpdates"]
if len(polls) < 2:
    fail("only %d polls; the offset was never exercised" % len(polls))
if polls[-1]["body"].get("offset") != 2:
    fail("the last poll resumed at %r, want past the message it handled"
         % polls[-1]["body"].get("offset"))
CHECK
printf 'ok   addressed to the chat it came from, and acknowledged once\n'

printf '\nPASS: a second platform carries a run end to end\n'
