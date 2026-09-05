#!/bin/sh
# Proves that when the machine takes a note from what a person said, the
# channel they said it in is told, on the line under the answer.
#
# The fake provider answers from a script and counts tool turns to find its
# place in it, so the curator's own question — which has no tool turns — gets
# the first entry too. The first entry is therefore written as what the
# curator expects back: one claim, quoting the person verbatim. The channel
# sees that JSON as the answer, which is ugly and beside the point.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK"
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHANNEL=900000000000000021
TENANT=900000000000000022
PERSON=900000000000000023

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
cat > "$WORK/config.toml" <<CONFIG
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = '[{"claim":"They edit in vim.","quote":"I edit everything in vim","message":1,"about":"person"}]'

[server]
addr = "127.0.0.1:7801"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[memory]
enabled = true
curate = true
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

BASE="http://127.0.0.1:7801"
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

DELIVERED=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $GATEWAY" \
	-d "{
		\"message\": {
			\"platform\": \"discord\", \"accountId\": \"main\",
			\"tenantId\": \"$TENANT\", \"channelId\": \"$CHANNEL\",
			\"platformMessageId\": \"1\", \"idempotencyKey\": \"discord:1\",
			\"principalId\": \"$PERSON\", \"principalDisplayName\": \"someone\",
			\"text\": \"for the record, I edit everything in vim\", \"trigger\": \"MESSAGE_TRIGGER_MENTION\"
		}
	}" \
	"$BASE/jingclaw.control.v1.GatewayIngressService/DeliverInbound")
RUN=$(printf '%s' "$DELIVERED" | sed -n 's/.*"runId":"\([^"]*\)".*/\1/p')
[ -n "$RUN" ] || fail "the message started nothing: $DELIVERED"

# said is what the channel has been told about the run, in order.
said() {
	sqlite3 -readonly "$WORK/data/jingclaw.db" \
		"select group_concat(json_extract(payload,'$.state') || ':' || coalesce(json_extract(payload,'$.detail'),''), ' ') from (select payload from gateway_dispatches where run_id='$RUN' and kind='status' order by seq);"
}

WAITED=0
while ! said | grep -q 'noted:'; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the channel was never told a note was taken; it heard: $(said)
$(tail -5 "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   the channel is told a note was taken: %s\n' "$(said)"

said | grep -q 'completed:.* noted:1' ||
	fail "the note is not said after the run's account, or the count is off: $(said)"
printf 'ok   after the account of the run, with the count\n'

LISTED=$("$WORK/jingclaw" --config "$WORK/config.toml" memory list)
printf '%s' "$LISTED" | grep -q 'vim' ||
	fail "the note the channel was told about is not in memory:
$LISTED"
printf 'ok   and the note is there to be listed\n'

printf '\nPASS: what the machine notes is said on the line under the answer\n'
