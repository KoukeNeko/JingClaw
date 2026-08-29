#!/bin/sh
# Proves the model's working-out goes where it may and nowhere else: recorded
# under its own kind, answered to a control-plane client, and refused for every
# chat platform — console channel included.
#
# End to end rather than at the projector alone. The projector's own test is
# what stops the refusal being deleted; this is what stops it being bypassed by
# some other path to a channel.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

SECRET="the token is in discord.token and must not be repeated"

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[model]
provider = "fake"
model = "fake-echo"
fake_delay = "0s"
fake_reasoning = "$SECRET"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7794"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "discord"
account_id = "main"
[[gateway.consoles]]
channel_ids = ["900000000000000001"]
tenant_id = "900000000000000002"
workspace_id = "default"
users = ["900000000000000003"]
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

# The turn arrives the way a channel's does, so the outbox actually fills. A
# run started from the terminal owes its answer to the terminal and queues
# nothing, which would make the check below pass over an empty table.
GATEWAY_TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

DELIVERED=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $GATEWAY_TOKEN" \
	-d '{
		"meta": {"clientId": "verify"},
		"message": {
			"platform": "discord",
			"accountId": "main",
			"tenantId": "900000000000000002",
			"channelId": "900000000000000001",
			"platformMessageId": "1",
			"idempotencyKey": "verify-1",
			"principalId": "900000000000000003",
			"principalDisplayName": "operator",
			"text": "what time is it",
			"trigger": "MESSAGE_TRIGGER_MENTION"
		}
	}' \
	"http://127.0.0.1:7794/jingclaw.control.v1.GatewayIngressService/DeliverInbound")

SESSION=$(printf '%s' "$DELIVERED" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "the message did not start a run: $DELIVERED"

sleep 2

# Attached after the fact rather than before: the session did not exist until
# the message created it, and attaching replays from the beginning anyway.
"$WORK/agent" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 1
kill "$ATTACH" 2>/dev/null
EVENTS=$(cat "$WORK/events")

# 1. It is in the log, under its own kind rather than as text.
printf '%s' "$EVENTS" | grep -q 'assistant.thinking' ||
	fail "the working-out was never recorded: $EVENTS"
printf 'ok   the working-out is recorded under its own kind\n'

printf '%s' "$EVENTS" | grep 'assistant.delta' | grep -q "$SECRET" &&
	fail "the working-out was recorded as the answer"
printf 'ok   and not as the answer\n'

# 2. A control-plane client is answered with it. This is the plane that may
#    see it, and a field nothing ever fills is a promise rather than a feature.
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
[ -n "$TOKEN" ] || fail "the daemon published no local credential"

VIEW=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" \
	-d "{\"meta\":{\"clientId\":\"verify\"},\"sessionId\":\"$SESSION\"}" \
	"http://127.0.0.1:7794/jingclaw.control.v1.SessionService/GetSessionView")
printf '%s' "$VIEW" | grep -q '"reasoning"' ||
	fail "the session view carries no working-out: $VIEW"
printf 'ok   a control-plane client is answered with it\n'

# 3. Nothing was queued for a channel carrying it. Read from the database
#    rather than from a platform, because a platform this never reached is
#    indistinguishable from a platform nothing was sent to at all.
COUNTS=$(python3 -c "
import sqlite3, sys
db = sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True)
leaked = db.execute(\"SELECT COUNT(*) FROM gateway_dispatches WHERE payload LIKE '%discord.token%'\").fetchone()[0]
total = db.execute('SELECT COUNT(*) FROM gateway_dispatches').fetchone()[0]
print(leaked, total)
" "$WORK/data/jingclaw.db")
LEAKED=${COUNTS% *}
TOTAL=${COUNTS#* }

[ "$LEAKED" = 0 ] || fail "$LEAKED dispatches carry the working-out"
printf 'ok   and nothing carrying it was queued for a channel\n'

# The outbox has to have had something in it, or the check above passed by
# there being no traffic at all.
[ "$TOTAL" -gt 0 ] || fail "the outbox is empty, so nothing was actually checked"
printf 'ok   %s dispatches were queued, none of them this\n' "$TOTAL"

printf '\nPASS: the working-out stays on the control plane\n'
