#!/bin/sh
# Proves a conversation started from a chat channel can be taken over at the
# machine: the same session, the same run, and an approval the channel cannot
# answer decided by whoever is at a control-plane client.
#
# This is the one M2 acceptance criterion about the two planes meeting. A
# gateway turn and a local turn are different trust, and the whole design rests
# on them being the same session anyway — if they were not, taking over would
# mean starting again.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHANNEL=900000000000000001
TENANT=900000000000000002
PERSON=900000000000000003

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
printf 'timeout := 30\n' > "$WORK/ws/settings.go"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Looking at it."
tool = "read_file"
args = '{"path":"settings.go"}'

[[provider.fake_script]]
text = "Raising it."
tool = "edit_file"
args = '{"path":"settings.go","edits":[{"old_text":"timeout := 30","new_text":"timeout := 120"}]}'

[[provider.fake_script]]
text = "Done."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7797"
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
EOF

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE="http://127.0.0.1:7797"
LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

# The turn arrives from a channel, as one would.
DELIVERED=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $GATEWAY" \
	-d "{
		\"meta\": {\"clientId\": \"verify\"},
		\"message\": {
			\"platform\": \"discord\", \"accountId\": \"main\",
			\"tenantId\": \"$TENANT\", \"channelId\": \"$CHANNEL\",
			\"platformMessageId\": \"1\", \"idempotencyKey\": \"takeover-1\",
			\"principalId\": \"$PERSON\", \"principalDisplayName\": \"someone\",
			\"text\": \"raise the timeout\", \"trigger\": \"MESSAGE_TRIGGER_MENTION\"
		}
	}" \
	"$BASE/jingclaw.control.v1.GatewayIngressService/DeliverInbound")

SESSION=$(printf '%s' "$DELIVERED" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
RUN=$(printf '%s' "$DELIVERED" | sed -n 's/.*"runId":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "the channel's message started nothing: $DELIVERED"
printf 'ok   a message from a channel starts a run\n'

# 1. The same session is there for somebody at the machine, by its own id and
#    in the list. A conversation only the channel can see is one nobody can
#    take over.
LISTED=$("$WORK/jingclaw" --config "$WORK/config.toml" session list)
printf '%s' "$LISTED" | grep -q "$SESSION" ||
	fail "the channel's session is not in the local list:
$LISTED"
printf 'ok   and the same session is listed at the machine\n'

# 2. The same run, with its events. Not a copy: the ids match.
WAITED=0
while ! curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $LOCAL" \
	-d "{\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/ListApprovals" | grep -q 'apr_'; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the run never stopped to ask: $(cat "$WORK/daemon.err" | tail -5)"
	sleep 0.1
done

VIEW=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $LOCAL" \
	-d "{\"meta\":{\"clientId\":\"verify\"},\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/GetSessionView")
printf '%s' "$VIEW" | grep -q 'raise the timeout' ||
	fail "the local view does not carry what the channel said: $VIEW"
printf '%s' "$VIEW" | grep -q "$RUN" ||
	printf 'note the view does not name the run id, which it need not\n'
printf 'ok   carrying what was said in the channel\n'

# 3. The approval the channel cannot answer. A request from chat and its
#    approval from the same chat is one unbroken chain: whoever holds that
#    account holds both halves.
LISTED=$("$WORK/jingclaw" --config "$WORK/config.toml" approvals "$SESSION")
APPROVAL=$(printf '%s' "$LISTED" | grep -o 'apr_[A-Za-z0-9]*' | head -1)
[ -n "$APPROVAL" ] || fail "nothing is waiting: $LISTED"

printf '%s' "$LISTED" | grep -q -- '-timeout := 30' ||
	fail "the operator is not shown what the channel's run would change:
$LISTED"
printf 'ok   what it would change is reviewable at the machine\n'

# 4. Decided from the control plane, and the work goes on.
"$WORK/jingclaw" --config "$WORK/config.toml" approve "$APPROVAL" >/dev/null

WAITED=0
while ! grep -q 'timeout := 120' "$WORK/ws/settings.go" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "approving from the machine did not resume the channel's run"
	sleep 0.1
done
printf 'ok   approving it at the machine resumes the run\n'

# 5. And the channel is told. A run taken over must not go quiet on the people
#    who started it.
QUEUED=$(python3 -c "
import sqlite3, sys
db = sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True)
print(db.execute('SELECT COUNT(*) FROM gateway_dispatches').fetchone()[0])
" "$WORK/data/jingclaw.db")
[ "$QUEUED" -gt 0 ] || fail "nothing was queued for the channel it came from"
printf 'ok   and the channel is still told what happened (%s dispatches)\n' "$QUEUED"

printf '\nPASS: a channel conversation can be taken over at the machine\n'
