#!/bin/sh
# Proves a long session keeps working: the daemon reads the window from the
# configuration, notices the conversation is past it, records the compaction as
# an event, and goes on serving turns.
set -eu

# A .JingClaw directory above this checkout must not decide anything here: a
# check that reaches the operator's own deployment would read its settings and,
# worse, write to its database. Stated rather than relied on.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"


WORK=$(mktemp -d)
BIN="$WORK/agentd"
CLI="$WORK/agent"
go build -o "$BIN" ./cmd/agentd
go build -o "$CLI" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
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

[context]
# Far below what the fake provider reports, so a handful of turns is enough to
# go past it. This is the setting under test: the file has to win.
window = 2000
compact_at = 0.7
keep_fraction = 0.3
# No margin, so the check sees pruning happen rather than a default hiding it.
keep_after_fold = 0

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
log_level = "info"
EOF

"$BIN" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

grep -q '"context_window":2000' "$WORK/daemon.err" ||
	fail "the daemon did not take the window from the configuration: $(cat "$WORK/daemon.err")"
printf 'ok   takes the context window from the configuration\n'

SESSION=$("$CLI" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session id"

BIG=$(head -c 3000 /dev/zero | tr '\0' 'y')
TURN=1
while [ "$TURN" -le 5 ]; do
	"$CLI" --config "$WORK/config.toml" send "$SESSION" "turn $TURN $BIG" >/dev/null
	TURN=$((TURN + 1))
	sleep 0.4
done

"$CLI" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 2
kill "$ATTACH" 2>/dev/null || true
wait "$ATTACH" 2>/dev/null || true

grep -q 'conversation.compacted' "$WORK/events" ||
	fail "a session well past the window recorded no compaction:
$(tail -20 "$WORK/events")"
printf 'ok   records the compaction as an event clients can see\n'

grep -q 'folded .* messages' "$WORK/events" ||
	fail "the event says nothing about what was folded"
printf 'ok   says how much history was folded away\n'

# Still alive and still answering after all of that.
"$CLI" --config "$WORK/config.toml" send "$SESSION" "and one more" >/dev/null
sleep 0.6
grep -c 'run.failed' "$WORK/daemon.err" >/dev/null 2>&1 &&
	fail "a run failed during compaction: $(grep run.failed "$WORK/daemon.err")"
printf 'ok   keeps serving turns afterwards\n'

# Folding is what makes the turns behind a summary safe to discard, and
# discarding them is what stops the log growing forever. Checked by the
# numbering: sequences name an event for the life of a session, so an oldest
# sequence above one is proof that earlier events went and that the count did
# not restart behind them.
OLDEST=$(python3 -c "import sqlite3,sys;print(sqlite3.connect('file:'+sys.argv[1]+'?mode=ro',uri=True).execute('SELECT MIN(seq) FROM events').fetchone()[0])" "$WORK/data/jingclaw.db")
FOLDS=$(python3 -c "import sqlite3,sys;print(sqlite3.connect('file:'+sys.argv[1]+'?mode=ro',uri=True).execute(\"SELECT COUNT(*) FROM events WHERE kind='conversation.compacted'\").fetchone()[0])" "$WORK/data/jingclaw.db")

[ "$FOLDS" -gt 0 ] || fail "nothing was folded, so retention proves nothing"
[ "$OLDEST" -gt 1 ] ||
	fail "the oldest surviving sequence is $OLDEST: a fold happened and nothing was discarded"
printf 'ok   %s folds, and what they replaced is gone (oldest sequence %s)\n' "$FOLDS" "$OLDEST"

printf '\nall checks passed\n'
