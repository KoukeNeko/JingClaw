#!/bin/sh
# Proves the console shows what the agent is doing and takes commands about it,
# driven through a real pseudo-terminal.
#
# A real one because the thing being checked only exists there. The console
# puts the terminal into raw mode, owns the bottom line, and redraws it around
# every line of log; none of that happens against a pipe, and a check that used
# one would be checking something else.
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
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
[server]
addr = "127.0.0.1:7786"
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

BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run/daemon.json")
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")

call() {
	curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $TOKEN" -d "$2" "$BASE/jingclaw.control.v1.SessionService/$1"
}

SESSION=$(call CreateSession '{"title":"watched"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"hello from the check\"}" >/dev/null
sleep 2

# Drives the console through a pty: waits for the prompt, types, and collects
# everything the console drew.
cat > "$WORK/drive.py" <<'DRIVE'
import os, pty, select, sys, time

typed = [line.encode() + b"\r" for line in sys.argv[2:]]
pid, fd = pty.fork()
if pid == 0:
	os.environ["JINGCLAW_HOME"] = "none"
	os.execvp(sys.argv[1], [sys.argv[1], "console", "--runtime-dir", os.environ["RUNTIME_DIR"]])

seen = bytearray()
deadline = time.time() + 30
sent = False

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

	if not sent and b"> " in seen:
		time.sleep(1.0)
		for line in typed:
			os.write(fd, line)
			time.sleep(0.8)
		sent = True
		deadline = time.time() + 5

try:
	os.write(fd, b"\x03")
except OSError:
	pass
sys.stdout.buffer.write(bytes(seen))
DRIVE

RUNTIME_DIR="$WORK/run" python3 "$WORK/drive.py" "$WORK/jingclaw" \
	help sessions approvals "not-a-command" quit > "$WORK/screen" 2>&1 || true

[ -s "$WORK/screen" ] || fail "the console drew nothing at all"

grep -q "JingClaw" "$WORK/screen" || fail "the console did not say what it is: $(head -c 200 "$WORK/screen")"
printf 'ok   the console opens and says what it is\n'

# The event stream, which is the whole reason it exists.
grep -q "hello from the check" "$WORK/screen" ||
	fail "the console did not show the turn that was sent: $(head -c 600 "$WORK/screen")"
printf 'ok   it shows what happened in a session it was not told about\n'

grep -q "$SESSION" "$WORK/screen" ||
	fail "the session listing is missing"
printf 'ok   and it answers a command about them\n'

# An unknown command has to say so rather than being sent anywhere: everything
# typed here is an instruction to this program.
grep -q 'there is no "not-a-command"' "$WORK/screen" ||
	fail "an unknown command was not refused: $(tail -c 400 "$WORK/screen")"
printf 'ok   something that is not a command is refused, not forwarded\n'

# The mechanism the whole thing rests on: a line of log arriving while
# somebody is typing erases the input line and puts it back, rather than
# landing in the middle of it.
python3 - "$WORK/screen" <<'CHECK' || exit 1
import re, sys

drawn = open(sys.argv[1], "rb").read().decode("utf-8", "replace")

# CR then erase-line is how the input line is taken down before a log line.
if "\r\x1b[2K" not in drawn:
	print("FAIL: the input line was never erased before a log line", file=sys.stderr)
	raise SystemExit(1)

# The interleaving this exists to stop: a log line written into the middle of
# what somebody is typing. It would show as the prompt and its half-finished
# command with the log line appended, on one drawn line, with no carriage
# return between them.
#
# Looked for by shape rather than by content, since a payload may contain
# anything -- including "> ", which is why this cannot simply search for it.
for piece in re.split(r"[\r\n]", drawn):
	piece = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", piece)
	if not piece.startswith("> "):
		continue
	typed = piece[2:]
	# A timestamp is how every log line starts. One after a prompt on the same
	# drawn line means it landed inside the input.
	if re.search(r"\d\d:\d\d:\d\d\s+#", typed):
		print(f"FAIL: a log line was drawn inside the input: {piece!r}", file=sys.stderr)
		raise SystemExit(1)
CHECK
printf 'ok   the input line is taken down and put back around each log line\n'

grep -q "leaving the console" "$WORK/screen" ||
	fail "quit did not say what it was doing"
printf 'ok   and leaving says what happens to the agent\n'

printf '\nall checks passed\n'
