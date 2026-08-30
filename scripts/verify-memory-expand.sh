#!/bin/sh
# Proves a memory lookup that matched nothing is tried again with other words.
#
# Memory is searched by word, and the way that fails is quiet. "Prefer reusing
# an existing component over building a second one" and "should I add a new
# modal?" are the same subject with no word in common: the index has nothing to
# match, returns nothing, and reports nothing missing. The agent then does the
# thing it was told not to, having asked.
#
# What is checked here is both halves of the fix. The note is found under words
# the agent did not use, and the answer says the search was widened — a result
# that answers a question near the one asked, presented as answering that one,
# is the same failure pointed the other way.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse.
# Where the agent may read and write is this directory's workspace, which is
# why the check has to have one rather than simply having none.
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

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

# The offline provider answers both the run and the request for other words,
# and it chooses its reply by counting the tool results already in the request
# it was given. The request asking for other words carries none — it is one
# question, with no conversation behind it — so it always gets the first entry,
# whatever turn the run itself is on. That is what makes "component reuse" the
# vocabulary offered here while the run walks through the rest of the script.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "component reuse"
tool = "remember"
args = '{"text":"prefer reusing an existing component over building a second one","scope":"workspace"}'

[[provider.fake_script]]
text = "Let me check what I know."
tool = "recall"
args = '{"query":"modal"}'

[[provider.fake_script]]
text = "Checked."

[memory]
enabled = true
expand_queries = true
[server]
addr = "127.0.0.1:7834"
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

A() { "$WORK/jingclaw" --config "$WORK/config.toml" "$@"; }

SESSION=$(A session create | tr -d '\r\n')
A attach --output "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!

A send "$SESSION" "note that, then tell me whether to build a new modal" >/dev/null

WAITED=0
while [ "$WAITED" -lt 120 ]; do
	grep -q 'run.completed\|run.failed' "$WORK/events" && break
	WAITED=$((WAITED + 1))
	sleep 0.5
done
grep -q 'run.completed' "$WORK/events" ||
	fail "the run never finished:
$(cut -c1-160 "$WORK/events" | tail -20)"

# 1. The words the agent chose match nothing. Without that this proves
#    nothing: a lookup that hit on its own would be widened by no one.
grep -q 'modal' "$WORK/events" || fail "the run never asked about a modal"
MEMORY=$(A memory list 2>&1)
printf '%s' "$MEMORY" | grep -q 'modal' &&
	fail "the note contains the word searched for, so nothing was missed: $MEMORY"
printf 'ok   the word searched for appears in no memory\n'

# 2. It was found anyway, under words the agent never used.
grep -q 'reusing an existing component' "$WORK/events" ||
	fail "the note was not found under other words:
$(cut -c1-160 "$WORK/events" | tail -25)"
printf 'ok   the note is found under other words\n'

# 3. And the answer says the search was widened, so the agent can tell a
#    memory that answered its question from one that answered a nearby one.
grep -q 'broadened' "$WORK/events" ||
	fail "a widened search did not report itself:
$(cut -c1-160 "$WORK/events" | tail -25)"
grep -q 'related words' "$WORK/events" ||
	fail "the result was not labelled for the model, only for the log"
printf 'ok   the widened search is reported as widened\n'

printf 'PASS: a lookup whose words miss is tried again with others\n'
