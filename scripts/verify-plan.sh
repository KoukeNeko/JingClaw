#!/bin/sh
# Proves the agent can keep a plan, that the plan is put back in front of it,
# and that everything else can see it.
#
# The point is not that a tool call succeeds. It is that the plan is state: it
# survives the turn that wrote it, the model is shown it again rather than
# having to find it in its own earlier output, and a client that opened the
# session afterwards sees the same list.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
ATTACH=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$ATTACH" ] && kill "$ATTACH" 2>/dev/null || true
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null || true
	wait 2>/dev/null || true
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

[[provider.fake_script]]
text = "Here is what I will do."
tool = "todo_update"
args = '{"operations":[{"op":"add","title":"read the failing test"},{"op":"add","title":"fix the timeout"},{"op":"add","title":"run the suite"}]}'

[[provider.fake_script]]
text = "Starting on the first."
tool = "todo_update"
args = '{"operations":[{"op":"set_status","id":"todo_1","status":"completed"},{"op":"set_status","id":"todo_2","status":"in_progress"}]}'

[[provider.fake_script]]
text = "Done for now."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7806"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

# 1. The tool is offered at all. One nothing lists is one no model can reach.
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
BASE="http://127.0.0.1:7806"

SESSION=$("$WORK/jingclaw" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.3

"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" "fix the failing test" >/dev/null

WAITED=0
while ! grep -q 'run.completed\|run.failed' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the run never finished: $(cut -c1-140 "$WORK/events" | tail -8)"
	sleep 0.1
done
grep -q 'run.failed' "$WORK/events" &&
	fail "the run failed: $(cut -c1-160 "$WORK/events" | tail -8)"
printf 'ok   a model can keep a plan without the run failing\n'

# 2. Every change is announced, and each announcement carries the whole plan.
CHANGES=$(grep -c '^[0-9]* *plan ' "$WORK/events" || true)
[ "$CHANGES" -ge 2 ] || fail "only $CHANGES plan events for two changes:
$(cut -c1-140 "$WORK/events" | tail -12)"
printf 'ok   and every change is announced (%s)\n' "$CHANGES"

grep -q '^[0-9]* *plan *1/3' "$WORK/events" ||
	fail "the announcement does not carry the whole plan:
$(grep '^[0-9]* *plan ' "$WORK/events")"
printf 'ok   carrying where the whole plan has got to\n'

# 3. A client that opened the session afterwards sees the same list. Without
#    this, a run working through a plan is one nobody watching can see.
VIEW=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"meta\":{\"clientId\":\"verify\"},\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/GetSessionView")
printf '%s' "$VIEW" | grep -q '"plan"' ||
	fail "the session view carries no plan: $VIEW"
printf '%s' "$VIEW" | grep -q 'read the failing test' ||
	fail "the view does not carry the steps: $VIEW"
printf '%s' "$VIEW" | grep -q 'PLAN_STATUS_IN_PROGRESS' ||
	fail "the view does not carry where each step got to: $VIEW"
printf 'ok   and a client opening the session afterwards sees it\n'

# 4. It survives a restart. A plan that did not would be forgotten every time
#    the daemon was updated, which is exactly when somebody is watching.
kill "$DAEMON"
wait "$DAEMON" 2>/dev/null || true
"$WORK/jingclaw" daemon --config "$WORK/config.toml" >>"$WORK/daemon.out" 2>>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not come back"
	sleep 0.1
done
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")

AFTER=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"meta\":{\"clientId\":\"verify\"},\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/GetSessionView")
printf '%s' "$AFTER" | grep -q 'fix the timeout' ||
	fail "the plan did not survive a restart: $AFTER"
printf 'ok   and survives a restart\n'

# 5. A run started after the restart still works with the plan in place.
#
# That the plan is actually put in front of the model is checked in Go, where
# the provider request can be read: nothing outside the daemon can see a system
# prompt, so asserting it from here would be asserting that a turn ran.
"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events2" 2>&1 &
ATTACH=$!
sleep 0.3
"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" "carry on" >/dev/null

WAITED=0
while ! grep -q 'run.completed\|run.failed' "$WORK/events2" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the run after the restart never finished"
	sleep 0.1
done
grep -q 'run.failed' "$WORK/events2" &&
	fail "the run after the restart failed:
$(cut -c1-160 "$WORK/events2" | tail -8)"
printf 'ok   and a run started afterwards still works with it in place\n'

printf '\nPASS: the agent can keep a plan, and everything can see it\n'
