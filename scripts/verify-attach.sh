#!/bin/sh
# Proves that watching a deployment somebody else started actually shows it.
#
# The failure this exists for was silent in the worst way: finding a daemon
# already running, the command said "watching it" and then waited on a signal.
# Nothing was drawn, nothing could be typed, and the terminal looked attached
# to something for as long as anybody was willing to stare at it. A check on
# the exit status would have passed; only reading what reached the screen
# catches it.
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
[server]
addr = "127.0.0.1:7790"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

# Started separately, which is the whole situation: the command under test
# must find this one rather than start its own.
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

SESSION=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" -d '{"title":"already going"}' \
	"$BASE/jingclaw.control.v1.SessionService/CreateSession" |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"sessionId\":\"$SESSION\",\"text\":\"said before anybody attached\"}" \
	"$BASE/jingclaw.control.v1.SessionService/SendTurn" >/dev/null
sleep 2

cat > "$WORK/drive.py" <<'DRIVE'
import fcntl, os, pty, select, struct, sys, termios, time

pid, fd = pty.fork()
if pid == 0:
	os.execvp(sys.argv[1], [sys.argv[1]])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 100, 0, 0))

seen = bytearray()
deadline = time.time() + 20
asked = False

while time.time() < deadline:
	ready, _, _ = select.select([fd], [], [], 0.5)
	if ready:
		try:
			chunk = os.read(fd, 65536)
		except OSError:
			break
		if not chunk:
			break
		seen.extend(chunk)

	# Something typed, because a console that draws a prompt and answers
	# nothing is still not attached to anything.
	if not asked and b"> " in seen:
		time.sleep(1.0)
		os.write(fd, b"sessions\r")
		time.sleep(2.0)
		asked = True
		deadline = time.time() + 5

try:
	os.write(fd, b"\x03")
except OSError:
	pass
sys.stdout.buffer.write(bytes(seen))
DRIVE

# Rebuilt after the deployment started, which is the situation the wrapper
# script creates every time somebody edits code while it is running. Faked by
# touching the file, because what the check compares is when it was written.
touch "$WORK/jingclaw"

JINGCLAW_HOME="$WORK" python3 "$WORK/drive.py" "$WORK/jingclaw" > "$WORK/screen" 2>&1 || true

grep -q "already running" "$WORK/screen" ||
	fail "the command did not find the daemon that was already up: $(head -c 400 "$WORK/screen")"
printf 'ok   it finds a deployment somebody else started\n'

# The part that was missing. Everything above passed while the terminal sat
# there showing one line and nothing else — and "JingClaw" is in that one
# line, so what is looked for is the greeting only a console prints.
grep -q "Type help for what you can do here" "$WORK/screen" ||
	fail "it said it was watching and drew no console: $(head -c 600 "$WORK/screen")"
printf 'ok   and draws a console rather than waiting silently\n'

grep -q "$SESSION" "$WORK/screen" ||
	fail "the console answered nothing about the running daemon: $(tail -c 600 "$WORK/screen")"
printf 'ok   and the console answers about the daemon it attached to\n'

# Quitting must not stop what it did not start, and the console says which it
# will do. The greeting is what is checked, because it is the observable half:
# nothing in this path terminates anything, so a console that promised to stop
# the daemon would simply be lying to whoever reads that line.
grep -q "quit to leave it running" "$WORK/screen" ||
	fail "the console offered to stop a daemon this command never started: $(head -c 600 "$WORK/screen")"
kill -0 "$DAEMON" 2>/dev/null ||
	fail "leaving the console stopped a daemon this command never started"
printf 'ok   and says leaving will not stop what it did not start\n'

# The trap the note closes: build, attach, and every change since that process
# started is absent while the screen says a build finished.
grep -q "newer than what is running" "$WORK/screen" ||
	fail "it attached to a deployment older than this build and said nothing: $(head -c 600 "$WORK/screen")"
printf 'ok   and says so when the build never reached what is running\n'

# And that the advice it gives works. `stop` sends a signal, which returns
# before the process has acted on it, so a script that stops and starts in one
# line looked at something still dying, attached to it, and printed the note
# above — advice to run exactly what had just been run.
JINGCLAW_HOME="$WORK" "$WORK/jingclaw" stop >"$WORK/stopped" 2>&1 ||
	fail "stop failed: $(cat "$WORK/stopped")"

kill -0 "$DAEMON" 2>/dev/null &&
	fail "stop returned while pid $DAEMON was still running: $(cat "$WORK/stopped")"
grep -q "stopped pid" "$WORK/stopped" ||
	fail "stop did not say it had stopped anything: $(cat "$WORK/stopped")"
printf 'ok   and stopping has stopped it by the time it returns\n'

DAEMON=""

printf '\nall checks passed\n'
