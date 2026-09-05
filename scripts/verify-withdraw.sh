#!/bin/sh
# Proves a message can be taken back while it waits its turn.
#
# One session answers one message at a time; a second arrives and waits. The
# person who sent it changes their mind. What must hold: the sender can take
# it back and nobody else can; the model is never shown it; the view does not
# draw it; the channel is told it was put away rather than that something was
# stopped; and the next message still gets its turn.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's.
export JINGCLAW_HOME="$WORK"
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here: under set -e a kill of something already gone
	# would end this function before the work directory is removed.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHANNEL=900000000000000011
TENANT=900000000000000012
PERSON=900000000000000013
STRANGER=900000000000000014

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

# The fake provider echoes what it was asked, pausing before the echo. The
# pause is what holds the session busy long enough for the messages behind
# the first to be waiting, and for somebody to change their mind.
cat > "$WORK/config.toml" <<CONFIG
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "2500ms"
[server]
addr = "127.0.0.1:7799"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "discord"
[gateway.discord]
account_id = "main"
[[gateway.discord.channels]]
channel_ids = ["$CHANNEL"]
tenant_id = "$TENANT"
workspace_id = "default"
users = ["$PERSON"]
CONFIG

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE="http://127.0.0.1:7799"
LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

# say delivers a message from the channel and prints the daemon's answer.
say() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $GATEWAY" \
		-d "{
			\"message\": {
				\"platform\": \"discord\", \"accountId\": \"main\",
				\"tenantId\": \"$TENANT\", \"channelId\": \"$CHANNEL\",
				\"platformMessageId\": \"$1\", \"idempotencyKey\": \"discord:$1\",
				\"principalId\": \"$PERSON\", \"principalDisplayName\": \"someone\",
				\"text\": \"$2\", \"trigger\": \"MESSAGE_TRIGGER_MENTION\"
			}
		}" \
		"$BASE/jingclaw.control.v1.GatewayIngressService/DeliverInbound"
}

# withdraw is somebody pressing the waiting mark on a message.
withdraw() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $GATEWAY" \
		-d "{
			\"platform\": \"discord\", \"accountId\": \"main\", \"tenantId\": \"$TENANT\",
			\"principalId\": \"$2\", \"idempotencyKey\": \"discord:$1\", \"platformMessageId\": \"$1\"
		}" \
		"$BASE/jingclaw.control.v1.GatewayIngressService/WithdrawInbound"
}

# status_of is a run's status as the control plane reports it.
status_of() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $LOCAL" \
		-d "{\"sessionId\":\"$SESSION\"}" "$BASE/jingclaw.control.v1.SessionService/ListRuns" |
		python3 -c '
import json, sys
runs = json.load(sys.stdin).get("runs", [])
print(next((run.get("status", "") for run in runs if run.get("id") == sys.argv[1]), ""))
' "$1"
}

wait_for_status() {
	WAITED=0
	while [ "$(status_of "$1")" != "$2" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 300 ] && fail "$1 is $(status_of "$1"), never $2: $(tail -5 "$WORK/daemon.err")"
		sleep 0.1
	done
}

run_of() { printf '%s' "$1" | sed -n 's/.*"runId":"\([^"]*\)".*/\1/p'; }

FIRST=$(say 1 "first question")
SESSION=$(printf '%s' "$FIRST" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
RUN1=$(run_of "$FIRST")
[ -n "$RUN1" ] || fail "the first message started nothing: $FIRST"

# A second, and a third, both waiting behind it.
RUN2=$(run_of "$(say 2 "second thoughts")")
RUN3=$(run_of "$(say 3 "still wanted")")
[ -n "$RUN2" ] && [ -n "$RUN3" ] || fail "the waiting messages started nothing"
wait_for_status "$RUN2" RUN_STATUS_QUEUED
wait_for_status "$RUN3" RUN_STATUS_QUEUED
printf 'ok   two messages are waiting behind the one being answered\n'

# 1. Somebody else pressing the mark on the sender's message does nothing.
ANSWER=$(withdraw 2 "$STRANGER")
printf '%s' "$ANSWER" | grep -q '"withdrawn":true' &&
	fail "a stranger took somebody else's message back: $ANSWER"
[ "$(status_of "$RUN2")" = RUN_STATUS_QUEUED ] || fail "a stranger's press changed the line"
printf 'ok   only the sender can take a message back\n'

# 2. The sender takes the second back. It is cancelled now, not when its turn
#    would have come.
ANSWER=$(withdraw 2 "$PERSON")
printf '%s' "$ANSWER" | grep -q '"withdrawn":true' ||
	fail "the sender could not take their own waiting message back: $ANSWER"
wait_for_status "$RUN2" RUN_STATUS_CANCELLED
[ "$(status_of "$RUN3")" = RUN_STATUS_QUEUED ] || fail "taking one back disturbed the one behind it"
printf 'ok   the sender takes it back and it is cancelled at once\n'

# 3. Pressing again finds nothing: it is already out of the line.
ANSWER=$(withdraw 2 "$PERSON")
printf '%s' "$ANSWER" | grep -q '"withdrawn":true' &&
	fail "a message was taken back twice: $ANSWER"
printf 'ok   and a second press finds nothing to take back\n'

# 4. The rest of the line goes on. The first finishes and the third gets its
#    turn and its answer.
wait_for_status "$RUN1" RUN_STATUS_COMPLETED
wait_for_status "$RUN3" RUN_STATUS_COMPLETED
printf 'ok   the message behind it still gets its turn\n'

# 5. The view does not draw what was taken back; the log still has it. The
#    conversation the model was shown is proved in the runtime's own tests;
#    what a person sees is proved here.
VIEW=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $LOCAL" \
	-d "{\"sessionId\":\"$SESSION\"}" "$BASE/jingclaw.control.v1.SessionService/GetSessionView")
printf '%s' "$VIEW" | grep -q 'second thoughts' &&
	fail "the view still draws a message that was taken back: $VIEW"
printf '%s' "$VIEW" | grep -q 'still wanted' ||
	fail "the view lost the message that was still wanted: $VIEW"
LOGGED=$(sqlite3 -readonly "$WORK/data/jingclaw.db" \
	"select count(*) from events where kind='user.message' and payload like '%second thoughts%';")
[ "$LOGGED" = 1 ] || fail "the log was rewritten: $LOGGED copies of the withdrawn message"
printf 'ok   the view leaves it out and the log keeps it\n'

# 6. The channel is told it was put away, not that something was stopped.
SAID=$(sqlite3 -readonly "$WORK/data/jingclaw.db" \
	"select group_concat(json_extract(payload,'$.state'), ',') from gateway_dispatches where run_id='$RUN2' and kind='status' order by seq;")
[ "$SAID" = "queued,withdrawn" ] ||
	fail "the channel was told '$SAID' about the withdrawn message, want queued,withdrawn"
printf 'ok   the channel is told it was put away, and nothing else\n'

printf '\nPASS: a waiting message can be taken back by whoever sent it\n'
