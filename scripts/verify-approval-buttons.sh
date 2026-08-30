#!/bin/sh
# Proves who may press an approval control in a shared channel.
#
# Three separate powers, and the point of the check is that they stay separate:
# being in a room, being allowed to ask the agent for something, and being
# allowed to permit what it asks. A deployment where the second implies the
# third is one nobody wrote down.
#
# The press itself arrives on the gateway ingress rather than on the session
# service. That is the whole security shape: a process holding a bot token
# reports that a named person pressed something in a named channel, and the
# daemon decides whether that counts. If the gateway could answer that question
# itself, the bot token would be an approval.
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
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHANNEL=900000000000000011
TENANT=900000000000000012
ASKER=900000000000000013
APPROVER=900000000000000014
STRANGER=900000000000000015

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Writing it."
tool = "write_file"
args = '{"path":"notes.md","content":"written"}'

[[provider.fake_script]]
text = "Done."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7836"
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
users = ["$ASKER", "$APPROVER", "$STRANGER"]
approvers = ["$APPROVER"]
EOF

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE="http://127.0.0.1:7836"
LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

ingress() {
	curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $GATEWAY" -d "$2" \
		"$BASE/jingclaw.control.v1.GatewayIngressService/$1"
}

# Somebody in the room asks for something that stops for a person.
DELIVERED=$(ingress DeliverInbound "{
	\"meta\": {\"clientId\": \"verify\"},
	\"message\": {
		\"platform\": \"discord\", \"accountId\": \"main\",
		\"tenantId\": \"$TENANT\", \"channelId\": \"$CHANNEL\",
		\"platformMessageId\": \"1\", \"idempotencyKey\": \"buttons-1\",
		\"principalId\": \"$ASKER\", \"principalDisplayName\": \"asker\",
		\"text\": \"write notes.md\", \"trigger\": \"MESSAGE_TRIGGER_MENTION\"
	}
}")
SESSION=$(printf '%s' "$DELIVERED" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "the channel's message started nothing: $DELIVERED"

WAITED=0
while :; do
	PENDING=$(curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $LOCAL" -d "{\"sessionId\":\"$SESSION\"}" \
		"$BASE/jingclaw.control.v1.SessionService/ListApprovals")
	printf '%s' "$PENDING" | grep -q 'apr_' && break
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the run never stopped to ask: $(tail -5 "$WORK/daemon.err")"
	sleep 0.1
done
APPROVAL=$(printf '%s' "$PENDING" | sed -n 's/.*"id":"\(apr_[^"]*\)".*/\1/p')
[ -n "$APPROVAL" ] || fail "no approval id in: $PENDING"
printf 'ok   a channel message stopped for a person\n'

press() {
	ingress DeliverDecision "{
		\"meta\": {\"clientId\": \"verify\"},
		\"platform\": \"discord\", \"accountId\": \"main\",
		\"tenantId\": \"$TENANT\", \"channelId\": \"$CHANNEL\",
		\"principalId\": \"$1\", \"approvalId\": \"$2\", \"allow\": $3
	}"
}

# 1. Being allowed to ask is not being allowed to permit. The asker is in
#    users and not in approvers, and that is the whole difference.
REFUSED=$(press "$ASKER" "$APPROVAL" true)
printf '%s' "$REFUSED" | grep -q 'DECISION_OUTCOME_REFUSED' ||
	fail "the person who asked was allowed to approve it: $REFUSED"
printf 'ok   asking for something does not permit it\n'

# 2. Nor does merely being in the room.
STRANGER_TRY=$(press "$STRANGER" "$APPROVAL" true)
printf '%s' "$STRANGER_TRY" | grep -q 'DECISION_OUTCOME_REFUSED' ||
	fail "somebody who is not an approver decided it: $STRANGER_TRY"
printf 'ok   being in the room does not permit it either\n'

# 3. A refusal says the same thing whether the approval exists or not.
#    Telling somebody which it was tells them about a room they are not
#    trusted in.
INVENTED=$(press "$STRANGER" "apr_00000000000000000000000000" true)
printf '%s' "$INVENTED" | grep -q 'DECISION_OUTCOME_REFUSED' ||
	fail "an invented id was answered differently: $INVENTED"
printf 'ok   and a refusal reveals nothing about what exists\n'

# 4. It is still pending. A refused press that quietly decided something would
#    pass every check above.
STILL=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $LOCAL" -d "{\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/ListApprovals")
printf '%s' "$STILL" | grep -q "$APPROVAL" ||
	fail "a refused press decided it anyway: $STILL"
printf 'ok   nothing was decided by any of that\n'

# 5. The named approver decides it, and the run goes on.
ALLOWED=$(press "$APPROVER" "$APPROVAL" true)
printf '%s' "$ALLOWED" | grep -q 'DECISION_OUTCOME_RECORDED' ||
	fail "the named approver was refused: $ALLOWED"
printf 'ok   the named approver decides it\n'

WAITED=0
while [ ! -f "$WORK/ws/notes.md" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the run did not continue after approval"
	sleep 0.1
done
printf 'ok   and the run continued\n'

# 6. A second press cannot decide it again. Two approvers can press in the
#    same instant and exactly one can win; the store settles that, not the UI.
AGAIN=$(press "$APPROVER" "$APPROVAL" false)
printf '%s' "$AGAIN" | grep -q 'DECISION_OUTCOME_ALREADY' ||
	fail "a second press was not reported as already decided: $AGAIN"
printf 'ok   a second press changes nothing\n'

# 7. The gateway credential reaches this and nothing that settles an approval
#    by id alone. A bot token that could call DecideApproval would be an
#    approval.
ESCALATED=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H 'content-type: application/json' -H "authorization: Bearer $GATEWAY" \
	-d "{\"approvalId\":\"$APPROVAL\",\"allow\":true}" \
	"$BASE/jingclaw.control.v1.SessionService/DecideApproval")
[ "$ESCALATED" = "401" ] || [ "$ESCALATED" = "403" ] ||
	fail "the gateway credential reached SessionService.DecideApproval (HTTP $ESCALATED)"
printf 'ok   and the gateway credential cannot decide by id alone\n'

printf 'PASS: only a named approver may press\n'
