#!/bin/sh
# Proves a run in flight is waited for when the daemon is asked to stop, even
# while a client is holding a stream open.
#
# The whole durability argument rests on this. A daemon that says "stopped"
# while a run goroutine is still mid-write is one whose log can end without a
# terminal event, and a client that reattaches afterwards sees a run that never
# finished and never failed.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
ATTACH=""
cleanup() {
	# The status of the last command in a trap becomes the script's, so each
	# of these is allowed to fail: a process that has already gone is the
	# normal case here, not a reason to report the check as failed.
	[ -n "$ATTACH" ] && kill "$ATTACH" 2>/dev/null || true
	[ -n "$DAEMON" ] && kill -9 "$DAEMON" 2>/dev/null || true
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
# Slow enough that a run is genuinely in flight when the signal arrives.
fake_delay = "1s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7804"
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

SESSION=$("$WORK/jingclaw" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

# A held-open stream, which is what a gateway and a console both are. Without
# one the http server drains instantly and the defect this checks for is
# invisible.
"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.5

"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" "say something back" >/dev/null
sleep 0.3

STARTED=$(date +%s)
kill -TERM "$DAEMON"

WAITED=0
while kill -0 "$DAEMON" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 400 ] && fail "the daemon did not stop within 40 seconds"
	sleep 0.1
done
ELAPSED=$(( $(date +%s) - STARTED ))
DAEMON=""
printf 'ok   it stops when asked (%ss)\n' "$ELAPSED"

# The run has to have reached a terminal state in the log. A run left running
# is one a client reattaching sees as still going, forever.
LAST=$(python3 -c "
import sqlite3, sys
db = sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True)
rows = db.execute('SELECT status FROM runs').fetchall()
print(' '.join(r[0] for r in rows) if rows else 'none')
" "$WORK/data/jingclaw.db")
[ "$LAST" != none ] || fail "no run was recorded at all"
case "$LAST" in
	*running*|*awaiting*)
		fail "a run was left unfinished in the log: $LAST" ;;
esac
printf 'ok   and the run it was serving reached a terminal state: %s\n' "$LAST"

# Said plainly rather than warned about. A shutdown that always reports a
# deadline is one nobody reads, and it was hiding a real one.
if grep -q '"msg":"runtime shutdown"' "$WORK/daemon.err"; then
	fail "the runtime did not drain in time:
$(grep '"msg":"runtime shutdown"' "$WORK/daemon.err")"
fi
printf 'ok   without the runtime running out of time to drain\n'

grep -q '"msg":"stopped"' "$WORK/daemon.err" || fail "the daemon never said it stopped"
printf 'ok   and it says so when it is done\n'

printf '\nPASS: a run in flight is waited for\n'
