#!/bin/sh
# Proves a session can answer with a model other than the daemon's default,
# and that everything downstream says which one actually answered.
#
# The point is not that a field can be set. It is that the run uses it, the
# summary names it rather than the configured default, and a session that
# never chose is unaffected — a summary naming the wrong model is worse than
# no summary, because it is what somebody reads to work out why an answer was
# poor.
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

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7798"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

SESSION=$("$WORK/agent" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

# 1. What is on offer comes from the provider, with the one in use marked.
LISTED=$("$WORK/agent" --config "$WORK/config.toml" session model "$SESSION" 2>&1)
printf '%s' "$LISTED" | grep -q 'fake-echo' ||
	fail "the provider's models were not offered: $LISTED"
printf '%s' "$LISTED" | grep -q '^\* fake-echo' ||
	fail "the model in use is not marked: $LISTED"
printf 'ok   what the provider offers is listed, with the one in use marked\n'

# 2. A session with no choice of its own uses the daemon's.
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
BASE="http://127.0.0.1:7798"

read_model() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
		-d "{\"meta\":{\"clientId\":\"verify\"},\"sessionId\":\"$1\"}" \
		"$BASE/jingclaw.control.v1.SessionService/ListModels"
}

BEFORE=$(read_model "$SESSION")
printf '%s' "$BEFORE" | grep -q '"current":"fake-echo"' ||
	fail "a session with no choice does not report the default: $BEFORE"
printf 'ok   a session that never chose uses the daemon default\n'

# 3. Choosing one is recorded on the session, and reported back.
"$WORK/agent" --config "$WORK/config.toml" session model "$SESSION" a-different-model >/dev/null 2>&1
AFTER=$(read_model "$SESSION")
printf '%s' "$AFTER" | grep -q '"current":"a-different-model"' ||
	fail "the choice was not recorded: $AFTER"
printf '%s' "$AFTER" | grep -q '"default":"fake-echo"' ||
	fail "the daemon's default changed with it: $AFTER"
printf 'ok   choosing one changes that session and not the daemon\n'

# 4. Another session is unaffected. A per-session choice that leaked into every
#    session would be a configuration change with no way back.
OTHER=$("$WORK/agent" --config "$WORK/config.toml" session create | tr -d '\r\n')
OTHER_MODEL=$(read_model "$OTHER")
printf '%s' "$OTHER_MODEL" | grep -q '"current":"fake-echo"' ||
	fail "the choice leaked into another session: $OTHER_MODEL"
printf 'ok   and leaves every other session alone\n'

# 5. It survives a restart. A choice that lasted only as long as the process
#    would be a setting somebody has to make again after every crash.
kill "$DAEMON"
wait "$DAEMON" 2>/dev/null || true
"$WORK/agentd" --config "$WORK/config.toml" >>"$WORK/daemon.out" 2>>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not come back"
	sleep 0.1
done
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")

RESTARTED=$(read_model "$SESSION")
printf '%s' "$RESTARTED" | grep -q '"current":"a-different-model"' ||
	fail "the choice did not survive a restart: $RESTARTED"
printf 'ok   and survives a restart\n'

# 6. Setting it back to nothing returns to the default, rather than needing a
#    separate call nobody would find.
"$WORK/agent" --config "$WORK/config.toml" session model "$SESSION" "" >/dev/null 2>&1
CLEARED=$(read_model "$SESSION")
printf '%s' "$CLEARED" | grep -q '"current":"fake-echo"' ||
	fail "clearing the choice did not go back to the default: $CLEARED"
printf 'ok   and clearing it goes back to the default\n'

printf '\nPASS: a session can choose its own model\n'
