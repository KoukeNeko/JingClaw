#!/bin/sh
# Proves a program can outlive the run that started it, be talked to, and be
# ended — through the real daemon, since that is where the seams are.
#
# The interesting case is not that a process starts. It is that the run ends
# and the process is still there, that reading twice does not hand a model the
# same output again, and that nothing outlives the daemon.
#
# Driven by the offline provider's script rather than by a real model: what is
# under test is the daemon's process handling, and a real model would make the
# check slow and make it fail for reasons that have nothing to do with this.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

# The script: start something that keeps running, read what it printed, read
# again, then answer. Written here rather than driven turn by turn because a
# scripted provider is only interesting if the whole exchange is one run.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Starting it."
tool = "start_process"
args = '{"program":"sh","args":["-c","echo listening on 3000; (while true; do touch still-running; sleep 0.1; done) & wait"]}'

# The script cannot read the process back: the id is chosen at runtime and a
# scripted turn is fixed before the daemon starts. Reading is checked in
# internal/tool/builtin instead, where the id can be threaded; what only this
# check can show is that the program is still there once the run has ended.
[[provider.fake_script]]
text = "It is up."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7795"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

start_daemon() {
	"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
		sleep 0.1
	done
}

# Everything the run asks for is approved, because starting a program needs a
# decision and nobody is here to make one.
approve_everything() {
	WAITED=0
	while [ "$WAITED" -lt 150 ]; do
		WAITING=$("$WORK/jingclaw" --config "$WORK/config.toml" approvals "$1" 2>/dev/null |
			grep -o 'apr_[A-Za-z0-9]*' | head -1 || true)
		if [ -n "$WAITING" ]; then
			"$WORK/jingclaw" --config "$WORK/config.toml" approve "$WAITING" >/dev/null 2>&1 || true
		fi
		WAITED=$((WAITED + 1))
		sleep 0.2
	done
}

start_daemon

SESSION=$("$WORK/jingclaw" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" --output >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.3

approve_everything "$SESSION" &
APPROVER=$!

"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" "start the dev server" >/dev/null

WAITED=0
while ! grep -q 'started sh as' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "nothing was ever started: $(cat "$WORK/events")"
	sleep 0.1
done
printf 'ok   a model can start a program that keeps running\n'

PROCESS=$(sed -n 's/.*started sh as \(prc_[A-Za-z0-9]*\).*/\1/p' "$WORK/events" | head -1)
[ -n "$PROCESS" ] || fail "no process id in $(cat "$WORK/events")"

# It is still there after the call that started it returned. This is the
# difference between these tools and exec_command, and it is the whole point.
WAITED=0
while [ ! -f "$WORK/ws/still-running" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the program is not running after the call returned"
	sleep 0.1
done
printf 'ok   and it is still running after the call returned\n'

# The run ends and the program does not. This is the whole difference between
# these tools and exec_command.
WAITED=0
while ! grep -q 'run.completed\|run.failed' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the run never finished: $(cut -c1-140 "$WORK/events" | tail -8)"
	sleep 0.1
done
grep -q 'run.failed' "$WORK/events" &&
	fail "the run failed: $(cut -c1-160 "$WORK/events" | tail -8)"

kill "$APPROVER" 2>/dev/null || true
kill "$ATTACH" 2>/dev/null || true
wait "$APPROVER" 2>/dev/null || true
wait "$ATTACH" 2>/dev/null || true

sleep 0.5
rm -f "$WORK/ws/still-running"
sleep 0.5
[ -f "$WORK/ws/still-running" ] || fail "the program stopped when the run ended"
printf 'ok   the run ends and the program does not\n'

# Nothing outlives the daemon. A process nobody can name any more keeps a port
# bound until somebody notices at the machine.
PID=$(python3 -c "
import re, sys
text = open(sys.argv[1]).read()
found = re.search(r'pid (\d+)', text)
print(found.group(1) if found else '')
" "$WORK/events")
[ -n "$PID" ] || fail "no pid was reported: $(cat "$WORK/events")"
kill -0 "$PID" 2>/dev/null || fail "process $PID was not running before the daemon stopped"

kill "$DAEMON"
DAEMON=""
WAITED=0
while kill -0 "$PID" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "process $PID outlived the daemon"
	sleep 0.1
done
printf 'ok   and nothing outlives the daemon\n'

printf '\nPASS: programs outlive their run and nothing else\n'
