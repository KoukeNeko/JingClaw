#!/bin/sh
# Proves the agent can stop and ask a person something, and that the answer
# reaches it as the result of the call that asked.
#
# The point is what this replaces: without it a model writes a paragraph
# ending in a question and stops, which reads as an answer, leaves nothing
# downstream knowing the run is waiting, and arrives back as a new turn with
# no link to what was asked.
set -eu

# Nothing inherited may supply a document or override a setting: the settings
# below are the only ones this check's daemon is given, and a variable from the
# caller would be another.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse.
export JINGCLAW_HOME="$WORK"
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

# The settings reach the first start the way a container's do: the daemon saves
# this text in its database, and there is no settings file.
export JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "I need to know something first."
tool = "ask_user"
args = "{\"prompt\":\"Which migration strategy?\",\"kind\":\"choice\",\"options\":[{\"id\":\"a\",\"label\":\"keep the schema compatible\"},{\"id\":\"b\",\"label\":\"upgrade in place\"}]}"

[[provider.fake_script]]
text = "Right, doing that."

[server]
addr = "127.0.0.1:7810"
'

"$WORK/jingclaw" daemon >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
BASE="http://127.0.0.1:7810"

SESSION=$("$WORK/jingclaw" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

"$WORK/jingclaw" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.3

"$WORK/jingclaw" send "$SESSION" "migrate the database" >/dev/null

# 1. The run stops, and says it is waiting for an answer rather than for an
#    approval: every client offers a different control for the two.
WAITED=0
while ! grep -q 'run.awaiting_input' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the run never stopped to ask:
$(cut -c1-140 "$WORK/events" | tail -8)"
	sleep 0.1
done
printf 'ok   the run stops and says it is waiting for an answer\n'

# 2. What was asked is visible, with the options.
ASKED=$("$WORK/jingclaw" questions "$SESSION")
printf '%s' "$ASKED" | grep -q 'Which migration strategy' ||
	fail "the question is not listed: $ASKED"
printf '%s' "$ASKED" | grep -q 'upgrade in place' ||
	fail "the options are not listed: $ASKED"
printf 'ok   and what it asked is visible, with the options\n'

QUESTION=$(printf '%s' "$ASKED" | grep -o 'qst_[A-Za-z0-9]*' | head -1)
[ -n "$QUESTION" ] || fail "no question id in $ASKED"

# 3. An answer that was not on offer is refused. A model that listed two
#    options and is handed a third has been answered with something it cannot
#    interpret.
REFUSED=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"meta\":{\"clientId\":\"verify\"},\"questionId\":\"$QUESTION\",\"answer\":\"c\"}" \
	"$BASE/jingclaw.control.v1.SessionService/AnswerQuestion")
printf '%s' "$REFUSED" | grep -q '"code"' ||
	fail "an answer that was not on offer was accepted: $REFUSED"
printf 'ok   an answer that was not on offer is refused\n'

# 4. The real answer resumes the run and reaches the model as the result of
#    the call that asked. This is what makes it different from the person
#    simply sending another turn.
"$WORK/jingclaw" answer "$QUESTION" b >/dev/null 2>&1

WAITED=0
while ! grep -q 'run.completed\|run.failed' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "answering did not resume the run"
	sleep 0.1
done
grep -q 'run.failed' "$WORK/events" &&
	fail "the run failed after being answered:
$(cut -c1-160 "$WORK/events" | tail -8)"
printf 'ok   answering it resumes the run\n'

grep -q 'upgrade in place' "$WORK/events" ||
	fail "the model was never told what was chosen:
$(cut -c1-160 "$WORK/events" | tail -10)"
printf 'ok   and the model is told what was chosen\n'

# 5. Nothing is left waiting.
STILL=$("$WORK/jingclaw" questions "$SESSION" 2>&1)
printf '%s' "$STILL" | grep -q 'nothing waiting' ||
	fail "the answered question is still listed: $STILL"
printf 'ok   and nothing is left waiting\n'

printf '\nPASS: the agent can ask, and the answer reaches it\n'
