#!/bin/sh
# Proves the bare command is one process that runs the daemon and the gateway,
# and that it stops in order.
#
# `jingclaw` used to be a supervisor with a daemon, a gateway and a watchdog
# under it. What this checks is the shape that replaced it, with the real
# binary and a stub standing in for Telegram: one process and no children; the
# gateway in it still answers a message, through the daemon, over loopback; a
# second `jingclaw` attaches instead of starting another; `stop` ends it, the
# gateway before the daemon, and takes the discovery file with it; and a daemon
# that came up without an agent keeps running with no gateway.
set -eu

# Nothing inherited may supply a document or override a setting: the settings
# below are the only ones this check's daemon is given, and a variable from the
# caller would be another.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done
unset GEMINI_API_KEY TELEGRAM_BOT_TOKEN

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK/main"
mkdir -p "$JINGCLAW_HOME"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

RUN=""
STUB=""
cleanup() {
	set +e
	[ -n "$RUN" ] && kill "$RUN" 2>/dev/null
	[ -n "$STUB" ] && kill "$STUB" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHAT_ID=900000000000000031
USER_ID=900000000000000032

: > "$WORK/pending"
python3 ../scripts/support/telegram-stub.py 7870 "$WORK/calls.jsonl" \
	"$CHAT_ID" "$USER_ID" "$WORK/pending" 2>/dev/null &
STUB=$!

WAITED=0
while ! curl -s -o /dev/null -X POST "http://127.0.0.1:7870/botx/getMe"; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub never came up"
	sleep 0.1
done
: > "$WORK/calls.jsonl"

export JINGCLAW_CONFIG="[provider]
backend = \"fake\"
fake_model = \"fake-echo\"
fake_delay = \"0s\"
[server]
addr = \"127.0.0.1:7871\"
[gateway]
platform = \"telegram\"
[gateway.telegram]
account_id = \"main\"
api_base = \"http://127.0.0.1:7870\"
[[gateway.telegram.channels]]
channel_ids = [\"$CHAT_ID\"]
tenant_id = \"$CHAT_ID\"
workspace_id = \"default\"
users = [\"$USER_ID\"]
"

# No terminal: stdin is not one, so there is no console and the process writes
# to standard error, which is what a service captures.
TELEGRAM_BOT_TOKEN=stub-token "$WORK/jingclaw" </dev/null >"$WORK/out" 2>"$WORK/err" &
RUN=$!

waitUntil() {
	WAITED=0
	while ! eval "$1"; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 300 ] && return 1
		sleep 0.1
	done
}

waitUntil '[ -f "$JINGCLAW_HOME/run/daemon.json" ]' ||
	fail "it never published itself: $(tail -5 "$WORK/err")"

# The process in the discovery file is this one, not a child of it.
PUBLISHED=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["pid"])' "$JINGCLAW_HOME/run/daemon.json")
[ "$PUBLISHED" = "$RUN" ] || fail "the daemon is pid $PUBLISHED, not the process that was started ($RUN)"
printf 'ok   the process that was started is the daemon\n'

waitUntil 'grep "\"method\": \"sendMessage\"" "$WORK/calls.jsonl" 2>/dev/null | grep -q "say something back"' ||
	fail "the gateway in it never answered a message: $(tail -8 "$WORK/err")"
printf 'ok   and the gateway in it answers a message, through the daemon\n'

# And nothing else: no daemon or gateway child, no watchdog.
CHILDREN=$(ps -eo pid,ppid,args | awk -v p="$RUN" '$2 == p' || true)
[ -z "$CHILDREN" ] || fail "it has child processes:
$CHILDREN"
COPIES=$(ps -eo args | grep -c "^$WORK/jingclaw" || true)
[ "$COPIES" = 1 ] || fail "there are $COPIES processes of this binary, not one:
$(ps -eo pid,ppid,args | grep "$WORK/jingclaw" | grep -v grep)"
printf 'ok   and there is nothing else: no children, one process\n'

# The second one attaches rather than starting another daemon.
set +e
timeout -s TERM 3 "$WORK/jingclaw" </dev/null >"$WORK/second.out" 2>&1
set -e
grep -q "already running" "$WORK/second.out" || fail "a second run did not find the first: $(cat "$WORK/second.out")"
kill -0 "$RUN" 2>/dev/null || fail "leaving a second run stopped the first"
[ "$(grep -c 'jingclaw daemon listening' "$WORK/err")" = 1 ] || fail "a second daemon was started"
printf 'ok   a second run attaches to it and leaves it running\n'

# Stopped the way a person, and a container, stop it.
"$WORK/jingclaw" stop >"$WORK/stopped" 2>&1 || fail "stop failed: $(cat "$WORK/stopped")"
grep -q "stopped pid $RUN" "$WORK/stopped" || fail "stop did not say it stopped it: $(cat "$WORK/stopped")"
kill -0 "$RUN" 2>/dev/null && fail "stop returned while the process was still running"
wait "$RUN" && STATUS=0 || STATUS=$?
RUN=""
[ "$STATUS" = 0 ] || fail "it did not exit cleanly when stopped: status $STATUS: $(tail -5 "$WORK/err")"
[ ! -e "$JINGCLAW_HOME/run/daemon.json" ] || fail "stop left the discovery file behind"
printf 'ok   stop ends it cleanly and takes the discovery file with it\n'

GATEWAY_LINE=$(grep -n '"msg":"gateway stopped"' "$WORK/err" | head -1 | cut -d: -f1)
DAEMON_LINE=$(grep -n '"msg":"shutting down"' "$WORK/err" | head -1 | cut -d: -f1)
[ -n "$GATEWAY_LINE" ] || fail "the gateway never said it stopped: $(tail -8 "$WORK/err")"
[ -n "$DAEMON_LINE" ] || fail "the daemon never said it was shutting down: $(tail -8 "$WORK/err")"
[ "$GATEWAY_LINE" -lt "$DAEMON_LINE" ] || fail "the daemon began shutting down (line $DAEMON_LINE) before the gateway had stopped (line $GATEWAY_LINE)"
grep -q '"msg":"stopped"' "$WORK/err" || fail "the daemon never said it was done"
printf 'ok   the gateway stops first and the daemon after it\n'

# A daemon with no agent: no gateway, and it stays until it is stopped.
export JINGCLAW_HOME="$WORK/repair"
mkdir -p "$JINGCLAW_HOME"
# A settings file is imported as it is, wrong or not, which is how settings that
# will not start get into the database; the variable would be refused outright.
unset JINGCLAW_CONFIG
printf '[server]\nlog_level = "verbose"\n' > "$JINGCLAW_HOME/config.toml"
TELEGRAM_BOT_TOKEN=stub-token "$WORK/jingclaw" </dev/null >"$WORK/repair.out" 2>"$WORK/repair.err" &
RUN=$!
waitUntil 'grep -q "agent not running" "$WORK/repair.out"' ||
	fail "it did not say there is no agent: $(cat "$WORK/repair.out") $(tail -5 "$WORK/repair.err")"
grep -q "run: jingclaw dashboard" "$WORK/repair.out" || fail "it did not point at the dashboard: $(cat "$WORK/repair.out")"
sleep 2
kill -0 "$RUN" 2>/dev/null || fail "a daemon with no agent ended the run"
grep -q "JingClaw gateway" "$WORK/repair.out" "$WORK/repair.err" && fail "a gateway was started against a daemon with no agent"
"$WORK/jingclaw" stop >/dev/null 2>&1 || fail "stop failed on a daemon with no agent"
wait "$RUN" && STATUS=0 || STATUS=$?
RUN=""
[ "$STATUS" = 0 ] || fail "a daemon with no agent did not exit cleanly when stopped: status $STATUS"
[ ! -e "$JINGCLAW_HOME/run/daemon.json" ] || fail "stopping a daemon with no agent left the discovery file behind"
printf 'ok   with no agent it runs no gateway, says why, and stays until it is stopped\n'

printf '\nall checks passed\n'
