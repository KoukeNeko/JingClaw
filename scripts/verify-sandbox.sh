#!/bin/sh
# Proves an approved command still cannot reach outside the workspace.
#
# What a human approval answers is whether somebody meant to run something.
# What it cannot answer is what that something's dependencies will do, and for
# anything with a package manager in it that is most of what runs. So the
# command this check approves is one that tries to escape, and what is
# asserted is that approving it was not enough.
set -eu

# Nothing inherited may supply a document or override a setting: the settings
# below are the only ones this check's daemon is given, and a variable from the
# caller would be another.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's.
export JINGCLAW_HOME="$WORK"
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
BROKEN=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$BROKEN" ] && kill "$BROKEN" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# Only macOS has a backend so far. Skipping rather than failing: a check that
# fails on a machine it was never meant to run on tells nobody anything.
case "$(uname -s)" in
Darwin) ;;
*)
	printf 'skipped: no sandbox backend on %s\n' "$(uname -s)"
	exit 0
	;;
esac

# Somewhere outside the workspace, holding something worth not losing.
OUTSIDE="$WORK/outside"
mkdir -p "$OUTSIDE"
printf 'hunter2\n' > "$OUTSIDE/secret"

# The settings reach the first start the way a container's do: the daemon saves
# this text in its database, and there is no settings file.
export JINGCLAW_CONFIG="[provider]
backend = \"fake\"
fake_model = \"fake-echo\"
fake_delay = \"0s\"

# Asks to write outside the workspace, then to read something it should not
# see. Both are approved below; both must fail anyway.
[[provider.fake_script]]
text = \"Writing outside.\"
tool = \"exec_command\"
args = '{\"program\":\"/usr/bin/touch\",\"args\":[\"$OUTSIDE/escaped\"]}'

[[provider.fake_script]]
text = \"Reading the secret.\"
tool = \"exec_command\"
args = '{\"program\":\"/bin/cat\",\"args\":[\"$OUTSIDE/secret\"]}'

[[provider.fake_script]]
text = \"Done.\"

[server]
addr = \"127.0.0.1:7781\"

[sandbox]
enabled = true
hidden = [\"$OUTSIDE\"]
"

"$WORK/jingclaw" daemon >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

grep -q 'commands are confined' "$WORK/daemon.err" ||
	fail "the daemon did not say it was confining anything: $(cat "$WORK/daemon.err")"
printf 'ok   the daemon says what it is confining\n'

BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run/daemon.json")
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")

call() {
	curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $TOKEN" -d "$2" "$BASE/jingclaw.control.v1.SessionService/$1"
}

SESSION=$(call CreateSession '{"title":"sandbox"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"try to get out\"}" >/dev/null

# Approve everything it asks for. This is the point of the check: the human
# said yes, and the command still cannot do it.
APPROVED=0
WAITED=0
while [ "$APPROVED" -lt 2 ]; do
	PENDING=$(call ListApprovals "{\"sessionId\":\"$SESSION\"}")
	ID=$(printf '%s' "$PENDING" | sed -n 's/.*"id":"\(apr_[^"]*\)".*/\1/p' | head -1)
	if [ -n "$ID" ]; then
		"$WORK/jingclaw" approve "$ID" >/dev/null 2>&1 &&
			APPROVED=$((APPROVED + 1))
	fi
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 400 ] && fail "only $APPROVED of 2 commands ever asked: $(tail -3 "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   %d command(s) approved by a person\n' "$APPROVED"

WAITED=0
until grep -q '"msg":"run finished"\|run.completed' "$WORK/daemon.err" 2>/dev/null ||
	[ "$WAITED" -gt 300 ]; do
	WAITED=$((WAITED + 1))
	sleep 0.1
done
sleep 1

# The whole point.
[ -e "$OUTSIDE/escaped" ] &&
	fail "an approved command wrote outside the workspace"
printf 'ok   and one of them still could not write outside the workspace\n'

# And reading, which confining writes says nothing about.
python3 - "$WORK/data/jingclaw.db" <<'CHECK' || exit 1
import json, sqlite3, sys

db = sqlite3.connect(sys.argv[1])
rows = db.execute(
    "select payload from events where kind = 'tool.completed'"
).fetchall()

for (payload,) in rows:
    body = json.loads(payload)
    if "hunter2" in (body.get("content") or ""):
        print("FAIL: a confined command read a hidden file", file=sys.stderr)
        raise SystemExit(1)
CHECK
printf 'ok   nor read what it was told it could not see\n'

# And a machine that cannot confine refuses, rather than running unprotected.
# Refusing is not the daemon exiting any more: it stays up with no agent, so
# that the setting can be put right, and what is asserted is that it says which
# setting asked and that nothing which could run a command is there to be asked.
#
# This daemon has a database and a discovery file of its own, because settings
# are only saved into a database that has none.
export JINGCLAW_SERVER_DATA_DIR="$WORK/data2"
export JINGCLAW_SERVER_RUNTIME_DIR="$WORK/run2"
export JINGCLAW_CONFIG='[provider]
backend = "fake"
[server]
addr = "127.0.0.1:7780"
[sandbox]
enabled = true
'

# Pointed at a program that is not there, which is the one thing no real Mac
# can be: every one of them has sandbox-exec, so the case this feature turns
# on is unreachable without saying where to look.
JINGCLAW_SANDBOX_EXEC=/nonexistent/sandbox-exec \
	"$WORK/jingclaw" daemon >"$WORK/broken.out" 2>"$WORK/broken.err" &
BROKEN=$!

WAITED=0
while [ ! -f "$WORK/run2/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon with confinement on and no way to confine did not come up: $(cat "$WORK/broken.err")"
	sleep 0.1
done

BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run2/daemon.json")
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run2/daemon.json")

STATE=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" -d '{}' \
	"$BASE/jingclaw.control.v1.ConfigService/GetAgentState")
printf '%s' "$STATE" | python3 -c '
import json, sys

state = json.load(sys.stdin)
if state.get("mode") != "repair":
    print("FAIL: confinement it cannot provide did not leave a daemon with no agent: %s" % state, file=sys.stderr)
    raise SystemExit(1)

problems = state.get("problems") or []
if not any(p.get("source") == "startup" and "cannot confine" in p.get("why", "") for p in problems):
    print("FAIL: it stopped short of an agent without saying why: %s" % state, file=sys.stderr)
    raise SystemExit(1)
' || exit 1

# Nothing that runs a command is there to ask, whoever asks.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" -d '{"title":"unprotected"}' \
	"$BASE/jingclaw.control.v1.SessionService/CreateSession")
[ "$STATUS" = 404 ] ||
	fail "a daemon that could not confine answered a request for a session: $STATUS"
printf 'ok   confinement it cannot provide is refused, not skipped\n'

printf '\nall checks passed\n'
