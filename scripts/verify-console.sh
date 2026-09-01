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

# Large enough that the result is stored rather than sent inline, which is
# what makes it something `open` can be asked for.
python3 -c 'import sys
with open(sys.argv[1], "w") as out:
    for line in range(20000):
        out.write("line %d of a build log nobody wants truncated\n" % line)
' "$WORK/workspace/big-output-from-a-command-with-a-long-argument-tail.txt"

# A stand-in for the machine's opener, ahead of the real one on PATH, so this
# check records what would have been opened rather than launching a reader on
# somebody's desktop. Intercepted through PATH rather than through a setting,
# because a way to override the opener that exists only for checks is a way to
# override the opener.
mkdir -p "$WORK/bin"
for NAME in open xdg-open; do
	printf '#!/bin/sh\nprintf "%%s\\n" "$1" >> "$OPENED_LOG"\n' > "$WORK/bin/$NAME"
	chmod +x "$WORK/bin/$NAME"
done
OPENED_LOG="$WORK/opened"
export OPENED_LOG

# The console writes what it opens under the system temporary directory, which
# outlives a run. Pointed at this check's own so a file left by the last run is
# not the one being examined.
TMPDIR="$WORK/tmp"
mkdir -p "$TMPDIR"
export TMPDIR
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

# One call that stops for a decision and then leaves output too large to have
# been sent inline. Both halves are real rather than states this check
# invented, and they are what the show and open commands are for. One call and
# not two, because the second would stop for a decision of its own and this
# check types a fixed list of commands: it cannot name an id it has not seen.
#
# No backticks in here: this heredoc interpolates, so a word in backticks
# would be run as a command while the settings file is being written.
[[provider.fake_script]]
text = "Reading the big one."
tool = "exec_command"
args = '{"program":"cat","args":["big-output-from-a-command-with-a-long-argument-tail.txt"],"timeout_seconds":45}'

[[provider.fake_script]]
text = "Done."
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

# Two rounds: what can be typed straight away, and what has to wait for the
# agent to get somewhere first. Typed on a timer instead, the check would be
# racing a tool that takes as long as it takes — and every failure would read
# as the console ignoring a key, whichever step actually broke.
first = [line.encode() + b"\r" for line in os.environ["CONSOLE_FIRST"].split(",")]
after = [line.encode() + b"\r" for line in os.environ["CONSOLE_AFTER"].split(",")]
marker = os.environ["CONSOLE_WAIT_FOR"].encode()

pid, fd = pty.fork()
if pid == 0:
	os.environ["JINGCLAW_HOME"] = "none"
	os.execvp(sys.argv[1], [sys.argv[1], "console", "--runtime-dir", os.environ["RUNTIME_DIR"]])

seen = bytearray()
deadline = time.time() + 90
stage = 0

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

	if stage == 0 and b"> " in seen:
		time.sleep(1.0)
		for line in first:
			os.write(fd, line)
			time.sleep(1.5)
		stage = 1

	elif stage == 1 and marker in seen:
		time.sleep(1.0)
		for line in after:
			os.write(fd, line)
			time.sleep(2.0)
		stage = 2
		deadline = time.time() + 5

try:
	os.write(fd, b"\x03")
except OSError:
	pass
sys.stdout.buffer.write(bytes(seen))
DRIVE

# The waiting call, read before driving so the commands below can name it.
APPROVAL=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" -d "{\"sessionId\":\"$SESSION\"}" \
	"$BASE/jingclaw.control.v1.SessionService/ListApprovals" |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["approvals"][0]["id"])' 2>/dev/null)
[ -n "$APPROVAL" ] || fail "the scripted call did not stop for a decision"

# The marker is the log saying a call stored output. Waiting for it is what
# keeps this from typing `open` at a tool that has not finished: the run does
# not stop being slow because the check is in a hurry.
CONSOLE_FIRST="help,sessions,approvals,show $APPROVAL,not-a-command,approve $APPROVAL" \
	CONSOLE_WAIT_FOR="· output " \
	CONSOLE_AFTER="approvals,open,quit" \
	PATH="$WORK/bin:$PATH" RUNTIME_DIR="$WORK/run" \
	python3 "$WORK/drive.py" "$WORK/jingclaw" > "$WORK/screen" 2>&1 || true

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

# `show` is where the clipping stops. Deciding whether to run something means
# deciding about that thing, and a decision made against the first seventy
# characters of it is a decision about a prefix.
# The end of the arguments, past where the listing clips and absent from the
# rendered preview. Checking for the label would pass with the arguments gone;
# checking for their start would pass on the clipped line the listing already
# prints; checking for the file name would pass on the preview, which names it
# too; and checking for the field's name alone would pass on the summary, which
# renders every field as key=value. In its JSON form it appears in one place
# only: the arguments, in full.
grep -q '"timeout_seconds":45' "$WORK/screen" ||
	fail "show printed the arguments clipped, or not at all: $(tail -c 600 "$WORK/screen")"
grep -q "· Runs a program on this machine" "$WORK/screen" ||
	fail "show printed the call without saying what it touches: $(tail -c 600 "$WORK/screen")"
printf 'ok   the whole of a waiting call can be read, not just the clipped line\n'

# Every verb the table lists is wired to something. A command that is in help,
# completes, and then does nothing is worse than one that does not exist.
grep -q "is listed and not wired up" "$WORK/screen" &&
	fail "a command in the table does nothing: $(grep -o "\`[a-z]*\` is listed and not wired up" "$WORK/screen" | head -1)"
printf 'ok   and no listed command silently does nothing\n'

# Stored output, handed to the machine rather than drawn. A terminal is a poor
# image viewer and a worse PDF reader.
grep -q "· output " "$WORK/screen" ||
	fail "a call that stored output was not named in the log: $(tail -c 600 "$WORK/screen")"
printf 'ok   a call that stored output is named in the log\n'

[ -s "$WORK/opened" ] ||
	fail "open handed nothing to the machine: $(tail -c 600 "$WORK/screen")"
HANDED=$(tail -1 "$WORK/opened")
[ -f "$HANDED" ] || fail "the machine was handed $HANDED, which is not there"
grep -q "line 19999 of a build log" "$HANDED" ||
	fail "what was handed over is not the stored output: $(head -c 200 "$HANDED")"
case "$HANDED" in
	*.txt) ;;
	*) fail "the file was named $HANDED, whose extension is not the media type's" ;;
esac
# Never executable. A mode is not a judgement about the contents, and this is
# the difference between opening a document and running one.
[ -x "$HANDED" ] && fail "the file handed to the machine is executable: $HANDED"
printf 'ok   and open writes it out, named for what it is and not executable\n'

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
