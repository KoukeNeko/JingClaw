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

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse.
# Where the agent may read and write is this directory's workspace, which is
# why the check has to have one rather than simply having none.
export JINGCLAW_HOME="$WORK"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
GATEWAY=""
STUB=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
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

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

# The stub records every call it is given, so the assertions are about what
# would have gone over the wire rather than about what the adapter believes it
# sent.

python3 ../scripts/support/telegram-stub.py 7793 "$WORK/calls.jsonl" "$CHAT_ID" "$USER_ID" 2>/dev/null &
STUB=$!

WAITED=0
while ! curl -s -o /dev/null -X POST "http://127.0.0.1:7793/botx/getMe"; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub never came up"
	sleep 0.1
done
: > "$WORK/calls.jsonl"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
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

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   the daemon accepts a telegram binding\n'

TELEGRAM_BOT_TOKEN=stub-token \
	"$WORK/jingclaw" gateway --config "$WORK/config.toml" >"$WORK/gw.out" 2>"$WORK/gw.err" &
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
