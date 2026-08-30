#!/bin/sh
# Proves a delegated search is smaller than the run that asked for it.
#
# The reason to have one at all is context: a search costs a hundred tool
# results, and only its conclusion is worth carrying. Everything else about it
# is containment, and containment is what this checks — not that delegation
# works, which is easy, but that a worker cannot do more than the thing that
# sent it, cannot send one of its own, and cannot get a person's attention.
#
# So the worker here is scripted to reach for both. What is asserted is that
# it fails, and that the same calls from the conversation do not.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

export JINGCLAW_HOME="$WORK"
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
printf 'package main\n\nfunc main() {}\n' > "$WORK/workspace/main.go"

# The offline provider answers from where the conversation it was handed has
# got to, which is what makes this work: the worker starts a conversation of
# its own, so it starts at the top of the same script. Both of them reach for
# the same two things, and only one of them is allowed either.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Handing this over."
tool = "investigate"
args = '{"question":"Which Go files are in the workspace?"}'

[[provider.fake_script]]
text = "Writing it down."
tool = "write_file"
args = '{"path":"taken.md","content":"should have stopped"}'

[[provider.fake_script]]
text = "There is one Go file: main.go."

[server]
addr = "127.0.0.1:7793"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

# Named to the model, or it will not be used. The tools whose collaborator is
# the runtime are registered before the prompt is assembled precisely so this
# holds, and the ordering that breaks it is invisible from anywhere else.
PROMPT=$("$WORK/jingclaw" daemon --config "$WORK/config.toml" --print-prompt 2>"$WORK/prompt.err") ||
	fail "printing the prompt failed: $(cat "$WORK/prompt.err")"
printf '%s' "$PROMPT" | grep -q 'Tools available:.*investigate' ||
	fail "the model is never told it can delegate: $(printf '%s' "$PROMPT" | grep -i 'tools available')"
printf 'ok   the model is told it can delegate\n'

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

SESSION=$(call CreateSession '{"title":"investigate"}' |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"how many go files are there\"}" >/dev/null

# The conversation reaches for write_file and stops, which is the ordinary
# path and the reference point for everything below: it says write_file is
# registered and gated, so a worker failing to call it failed on the
# restriction rather than on the tool being absent.
WAITED=0
while : ; do
	WAITING=$(call ListApprovals "{\"sessionId\":\"$SESSION\"}")
	printf '%s' "$WAITING" | grep -q 'apr_' && break
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] &&
		fail "the conversation never reached the write: $(tail -5 "$WORK/daemon.err")"
	sleep 0.1
done
printf 'ok   the conversation asks to write and is stopped for a person\n'

[ -f "$WORK/workspace/taken.md" ] &&
	fail "the file was written before anybody allowed it"
printf 'ok   and nothing was written first\n'

# Everything else is in the log, which is where the worker is: it is not a
# session, not a client, and nothing but the log says it happened.
python3 - "$WORK/data/jingclaw.db" <<'CHECK' || exit 1
import json, sqlite3, sys

db = sqlite3.connect(sys.argv[1])
db.row_factory = sqlite3.Row


def fail(said):
    print(f"FAIL: {said}", file=sys.stderr)
    raise SystemExit(1)


runs = db.execute("select id, kind, parent_run_id, origin from runs").fetchall()
workers = [r for r in runs if r["kind"] == "worker"]
conversations = [r for r in runs if not r["kind"]]

if len(conversations) != 1:
    fail(f"want one conversation run, got {len(conversations)}")
if len(workers) != 1:
    fail(f"want one worker run, got {len(workers)}: {[dict(r) for r in runs]}")

worker, asking = workers[0], conversations[0]

if worker["parent_run_id"] != asking["id"]:
    fail(f"the worker does not say whose question it was: {dict(worker)}")
print("ok   the search ran as its own run, and says who asked")

if worker["origin"] != asking["origin"]:
    fail(
        "the worker did not run as whoever asked:\n"
        f"  asked by {asking['origin']}\n"
        f"  ran as   {worker['origin']}"
    )
print("ok   and as whoever asked, not as something more trusted")


def calls(run_id):
    rows = db.execute(
        "select payload from events where run_id = ? and kind = 'tool.completed'",
        (run_id,),
    ).fetchall()
    return [json.loads(row["payload"]) for row in rows]


tried = calls(worker["id"])
by_name = {}
for one in tried:
    by_name[one.get("name") or one.get("tool_name")] = one

for wanted in ("investigate", "write_file"):
    if wanted not in by_name:
        fail(f"the worker never reached for {wanted}: {tried}")
    if not by_name[wanted].get("is_error"):
        fail(f"the worker was allowed to call {wanted}: {by_name[wanted]}")
print("ok   it could not write, and could not delegate again")

# Nobody was asked about any of it. A worker that could park on an approval
# would be a run waiting on a person that no person is looking at, underneath
# a tool call that is already waiting.
parked = db.execute(
    "select count(*) as n from approvals where run_id = ?", (worker["id"],)
).fetchone()
if parked["n"]:
    fail(f"the worker asked somebody for permission {parked['n']} times")
print("ok   and asked nobody for permission, having nothing to ask about")

# What came back, and only what came back. This is the whole reason to
# delegate: the conversation gets the conclusion and none of the steps.
answers = [one for one in calls(asking["id"]) if one.get("name") == "investigate"]
if len(answers) != 1:
    fail(f"the conversation did not get one answer: {answers}")

answer = answers[0]
if answer.get("is_error"):
    fail(f"the search came back as a failure: {answer}")
if "There is one Go file" not in answer["content"]:
    fail(f"what the worker concluded did not reach the conversation: {answer}")
print("ok   and what it concluded came back to the conversation")

# The refusals are the worker's own business. A parent reading them would be
# reading the steps it delegated in order not to read.
if "no tool named" in answer["content"]:
    fail(f"the worker's own dead ends were replayed to the conversation: {answer}")
print("ok   and its dead ends were not, which is what delegating bought")

CHECK
printf 'ok   every one of the worker'"'"'s steps is in the log, under the worker\n'

printf '\nall checks passed\n'
