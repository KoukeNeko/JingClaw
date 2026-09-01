#!/bin/sh
# Proves the agent can find out what time it is, and that the answer is now.
#
# Two things a unit test cannot see. The first is whether the tool is named to
# the model at all: tools registered after the prompt is assembled exist,
# answer correctly when called, and are never offered — which has happened
# here before, to four of them at once.
#
# The second is whether the answer is current. A clock that returns a fixed
# time passes every check written against a fixed time, and this is the one
# tool where being plausibly wrong is the whole failure: a wrong date is not
# obviously wrong to anybody reading the answer.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands,
	# leaving the daemon holding its port.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Checking."
tool = "current_time"
args = '{}'

[[provider.fake_script]]
text = "That is when it is."

[server]
addr = "127.0.0.1:7804"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

# Named to the model, or it will never be reached for. A model cannot ask for
# something it has not been told exists, and nothing else in the system
# notices that it never did.
PROMPT=$("$WORK/jingclaw" daemon --config "$WORK/config.toml" --print-prompt 2>"$WORK/prompt.err") ||
	fail "printing the prompt failed: $(cat "$WORK/prompt.err")"
printf '%s' "$PROMPT" | grep -q 'Tools available:.*current_time' ||
	fail "the model is never told it can ask the time: $(printf '%s' "$PROMPT" | grep -i 'tools available')"
printf 'ok   the model is told it can ask what time it is\n'

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run/daemon.json")
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")

call() {
	curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $TOKEN" -d "$2" "$BASE/jingclaw.control.v1.SessionService/$1"
}

SESSION=$(call CreateSession '{"title":"asking the time"}' |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"what time is it\"}" >/dev/null

WAITED=0
while ! sqlite3 -readonly "$WORK/data/jingclaw.db" \
	"select 1 from events where kind='tool.completed'
	 and json_extract(payload,'\$.name')='current_time' limit 1;" | grep -q 1; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "the call never completed: $(tail -5 "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   and calling it comes back with something\n'

SAID=$(sqlite3 -readonly "$WORK/data/jingclaw.db" \
	"select json_extract(payload,'\$.content') from events where kind='tool.completed'
	 and json_extract(payload,'\$.name')='current_time' limit 1;")

# Within a minute of this machine's own clock. A fixed time passes any check
# written against a fixed time, and there is no other way to tell a clock from
# a constant.
python3 - "$SAID" <<'CHECK' || exit 1
import datetime, re, sys

said = sys.argv[1]


def fail(why):
    print("FAIL: " + why, file=sys.stderr)
    raise SystemExit(1)


stamps = re.findall(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:[+-]\d{2}:\d{2}|Z)", said)
if not stamps:
    fail("nothing in the answer is a timestamp: %r" % said)

now = datetime.datetime.now(datetime.timezone.utc)
for stamp in stamps:
    when = datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00"))
    drift = abs((now - when).total_seconds())
    if drift > 60:
        fail("the clock says %s, which is %.0f seconds from now" % (stamp, drift))

# Both readings are the same instant, or one of them is telling somebody the
# wrong time in a timezone they trust.
instants = {datetime.datetime.fromisoformat(s.replace("Z", "+00:00")) for s in stamps}
if len(instants) != 1:
    fail("the readings disagree about the instant: %r" % stamps)
CHECK
printf 'ok   what it says is now, and its readings agree\n'

# The offset is what makes an instant unambiguous, so it has to be there even
# where there is no zone name to give.
printf '%s' "$SAID" | grep -qE '[+-][0-9]{2}:[0-9]{2}|Z' ||
	fail "the answer carries no offset, so the instant is a guess: $SAID"
printf 'ok   with an offset, so the instant is not a guess\n'

printf '\nall checks passed\n'
