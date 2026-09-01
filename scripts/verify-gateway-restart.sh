#!/bin/sh
# Proves the gateway finds the daemon again after it restarts.
#
# The failure this exists for ran for nine hours in a real deployment and
# looked like nothing at all. The daemon publishes a fresh address every time
# it starts; the gateway read that once, kept it, and went on dialling a port
# nobody answers. It stayed connected to the platform the whole time, marked
# every message as seen, and delivered none of them — which from the room is
# indistinguishable from an agent that has decided not to reply.
#
# So the check is not "does it reconnect". It is: restart the daemon somewhere
# else, say something, and see whether an answer comes back.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
GATEWAY=""
STUB=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands,
	# leaving the rest running and holding their ports.
	set +e
	[ -n "$GATEWAY" ] && kill "$GATEWAY" 2>/dev/null
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$STUB" ] && kill "$STUB" 2>/dev/null
	# Reaped, so the shell does not print its own notice about the job it was
	# just told to end.
	wait "$GATEWAY" "$DAEMON" "$STUB" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHAT_ID=900000000000000021
USER_ID=900000000000000022

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
: > "$WORK/pending"

python3 ../scripts/support/telegram-stub.py 7802 "$WORK/calls.jsonl" \
	"$CHAT_ID" "$USER_ID" "$WORK/pending" 2>/dev/null &
STUB=$!

WAITED=0
while ! curl -s -o /dev/null -X POST "http://127.0.0.1:7802/botx/getMe"; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub never came up"
	sleep 0.1
done
: > "$WORK/calls.jsonl"

# No address for the daemon, so it takes whatever port it is given and
# publishes it. That is the whole situation: a fixed port would let a gateway
# holding a stale address work by luck.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "telegram"
[gateway.telegram]
account_id = "main"
api_base = "http://127.0.0.1:7802"
[[gateway.telegram.channels]]
channel_ids = ["$CHAT_ID"]
tenant_id = "$CHAT_ID"
workspace_id = "default"
users = ["$USER_ID"]
EOF

startDaemon() {
	"$WORK/jingclaw" daemon --config "$WORK/config.toml" \
		>>"$WORK/daemon.out" 2>>"$WORK/daemon.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(tail -5 "$WORK/daemon.err")"
		sleep 0.1
	done
}

whereItIs() {
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' \
		"$WORK/run/daemon.json"
}

# What was said back, not how much. Counting would pass on a reply to the
# message before: one turn can produce more than one post, so "there are two
# now" is not "the second one was answered".
waitForAnswerQuoting() {
	WAITED=0
	while ! grep '"method": "sendMessage"' "$WORK/calls.jsonl" 2>/dev/null |
		grep -q -- "$1"; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 400 ] && return 1
		sleep 0.1
	done
	return 0
}

startDaemon
FIRST=$(whereItIs)

TELEGRAM_BOT_TOKEN=stub-token \
	"$WORK/jingclaw" gateway --config "$WORK/config.toml" \
	>"$WORK/gw.out" 2>"$WORK/gw.err" &
GATEWAY=$!

waitForAnswerQuoting "say something back" ||
	fail "the first message was never answered: $(tail -5 "$WORK/gw.err")"
printf 'ok   a message is answered before anything restarts\n'

# The daemon goes away and comes back somewhere else. The gateway is left
# running on purpose: restarting it too would check nothing.
kill "$DAEMON" 2>/dev/null || true
wait "$DAEMON" 2>/dev/null || true
rm -f "$WORK/run/daemon.json"

startDaemon
SECOND=$(whereItIs)

[ "$FIRST" != "$SECOND" ] ||
	fail "the daemon came back on the same address, so this proves nothing"
printf 'ok   it comes back somewhere else (%s then %s)\n' "$FIRST" "$SECOND"

# Said in the room the message came from. Everything above can be true while
# this is the part that fails, and it is the part somebody notices.
AFTERWARDS="a message sent after the daemon moved"
echo "$AFTERWARDS" >> "$WORK/pending"

waitForAnswerQuoting "$AFTERWARDS" ||
	fail "the message sent after the restart was never answered; the gateway is still
talking to the address the daemon had before: $(tail -5 "$WORK/gw.err")"
printf 'ok   and a message sent after the restart is still answered\n'

# The gateway is the one that survived, not a replacement started underneath.
kill -0 "$GATEWAY" 2>/dev/null ||
	fail "the gateway did not survive the daemon restarting: $(tail -8 "$WORK/gw.err")"
printf 'ok   by the same gateway, which never restarted\n'

printf '\nall checks passed\n'
