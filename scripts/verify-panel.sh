#!/bin/sh
# Proves the panel draws a real session from a real daemon.
#
# The checks in the tui package drive the model directly, which is the right
# way to check what it draws and proves nothing about whether it is wired to
# anything. This one starts a daemon, gives it a turn, and reads what appears
# on a pseudo-terminal — the assembly seam, which is where this project's
# defects have actually lived.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's.
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

# A call that stops to ask, so there is a real decision on the screen rather
# than one this check invented.
[[provider.fake_script]]
text = "Writing it."
tool = "write_file"
args = '{"path":"notes.md","content":"written"}'

[[provider.fake_script]]
text = "Done."

[server]
addr = "127.0.0.1:7789"
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

SESSION=$(call CreateSession '{"title":"the watched one"}' |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"a turn to find on the screen\"}" >/dev/null
sleep 2

# The request the run stopped on, captured before the panel opens so the check
# can name the one it expects the panel to settle.
ASKED=$(call ListApprovals "{\"sessionId\":\"$SESSION\"}" |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["approvals"][0]["id"])' 2>/dev/null)
[ -n "$ASKED" ] || fail "the scripted call did not stop to ask, so there is no decision to make"

# Drives the panel through a pty and collects what it drew. Keys rather than
# commands, because that is the whole interface: there is nothing to type at.
cat > "$WORK/drive.py" <<'DRIVE'
import fcntl, os, pty, select, struct, subprocess, sys, termios, time

keys = os.environ["PANEL_KEYS"].split(",")

# Run once the session is open, to make something happen while the panel is
# watching. Without it the check only ever sees the view the panel asked for,
# and a broken live stream would pass.
after = os.environ.get("PANEL_AFTER", "")

pid, fd = pty.fork()
if pid == 0:
	os.environ["JINGCLAW_HOME"] = "none"
	os.execvp(sys.argv[1], [sys.argv[1], "panel", "--runtime-dir", os.environ["RUNTIME_DIR"]])

# A size, because a pty forked without one is 0x0 and a full-screen program
# given no columns draws nothing at all. Set from the parent after the fork so
# the child's first size message carries it.
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 100, 0, 0))

seen = bytearray()
deadline = time.time() + 30
pressed = False

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

	# Once the list has drawn, press the keys one at a time so each redraw
	# lands in the capture separately.
	if not pressed and b"Sessions" in seen:
		time.sleep(1.0)
		for key in keys:
			os.write(fd, key.encode().decode("unicode_escape").encode("latin-1"))
			time.sleep(1.0)
		pressed = True
		if after:
			subprocess.run(["/bin/sh", "-c", after], check=False)
		deadline = time.time() + 12

try:
	os.write(fd, b"\x03")
except OSError:
	pass

# Kept reading, because what is being checked is what the panel writes on its
# way out. A capture that stopped at the interrupt would show a program that
# never restored the terminal whether or not it did.
leaving = time.time() + 3
while time.time() < leaving:
	ready, _, _ = select.select([fd], [], [], 0.2)
	if not ready:
		continue
	try:
		chunk = os.read(fd, 65536)
	except OSError:
		break
	if not chunk:
		break
	seen.extend(chunk)

sys.stdout.buffer.write(bytes(seen))
DRIVE

# Two turns sent while the panel is open, which only the live stream can
# deliver: the view it asked for on the way in cannot contain them.
#
# Two rather than one, because one arrives on a single read. A panel that took
# the first thing off the stream and never asked again would pass a check that
# sent one turn, and would then sit there stale for the rest of the session.
LIVE="a turn sent while the panel was watching"
LIVE_AGAIN="and then a second one"

# statusOf reads how one request ended, out of the log rather than out of the
# pending list. A settled request leaves that list whichever way it was
# settled, so a check that only saw it gone would pass on a panel whose allow
# key sent a deny.
statusOf() {
	sqlite3 -readonly "$WORK/data/jingclaw.db" \
		"select json_extract(payload,'\$.status') from events
		 where kind='approval.resolved'
		   and json_extract(payload,'\$.approval_id')='$1';"
}
PANEL_KEYS='\r,a' \
	PANEL_AFTER="curl -s -X POST -H 'content-type: application/json' \
		-H 'authorization: Bearer $TOKEN' \
		-d '{\"sessionId\":\"$SESSION\",\"text\":\"$LIVE\"}' \
		'$BASE/jingclaw.control.v1.SessionService/SendTurn' >/dev/null
	sleep 2
	curl -s -X POST -H 'content-type: application/json' \
		-H 'authorization: Bearer $TOKEN' \
		-d '{\"sessionId\":\"$SESSION\",\"text\":\"$LIVE_AGAIN\"}' \
		'$BASE/jingclaw.control.v1.SessionService/SendTurn' >/dev/null" \
	RUNTIME_DIR="$WORK/run" python3 "$WORK/drive.py" "$WORK/jingclaw" > "$WORK/screen" 2>&1 || true

[ -s "$WORK/screen" ] || fail "the panel drew nothing at all"
if [ -n "${PANEL_DEBUG:-}" ]; then cat -v "$WORK/screen"; fi

grep -q "Sessions" "$WORK/screen" ||
	fail "the panel did not draw a session list: $(head -c 400 "$WORK/screen")"
printf 'ok   the panel opens on the list of sessions\n'

grep -q "the watched one" "$WORK/screen" ||
	fail "the session the daemon has is not in the list: $(head -c 600 "$WORK/screen")"
printf 'ok   and the list is the daemon'\''s, not an empty one\n'

# Enter opened it, so the turn is on the screen. This is the seam: the list
# came from one call and the conversation from another, and either could work
# while the step between them does not.
grep -q "a turn to find on the screen" "$WORK/screen" ||
	fail "opening the session drew no conversation: $(tail -c 800 "$WORK/screen")"
printf 'ok   opening a session draws what was said in it\n'

grep -q "$LIVE" "$WORK/screen" ||
	fail "a turn sent while the panel was open never reached it: $(tail -c 800 "$WORK/screen")"
grep -q "$LIVE_AGAIN" "$WORK/screen" ||
	fail "the panel stopped following after the first thing it was told: $(tail -c 800 "$WORK/screen")"
printf 'ok   and what happens next keeps arriving without asking again\n'

# The decision, which is what the panel is for. The scripted call stops to
# ask, the panel draws it, and "a" answers it — checked against the daemon
# rather than against the screen, because a panel that drew a decision it
# never sent would look exactly the same.
grep -q "waiting on you" "$WORK/screen" ||
	fail "the panel never drew the request the run stopped on: $(tail -c 800 "$WORK/screen")"
printf 'ok   a call the agent stopped to ask about is drawn\n'

# On the line that names the request, not anywhere on the screen: the tool is
# also listed in the conversation above, so a looser check would pass on a
# panel that drew "waiting on you" and nothing about what for.
grep -q "waiting on you: write_file" "$WORK/screen" ||
	fail "the request was drawn without saying what it was: $(tail -c 800 "$WORK/screen")"
printf 'ok   and it says which call is being decided\n'

SETTLED=$(statusOf "$ASKED")
[ -n "$SETTLED" ] ||
	fail "the panel drew the decision and did not send it: $ASKED was never resolved"
[ "$SETTLED" = "allowed" ] ||
	fail "the allow key settled the request as $SETTLED"
printf 'ok   allowing from the panel settles it, as an allow\n'

# Alternate screen, entered and left. A panel that took the screen and did not
# give it back leaves a terminal somebody has to close the window on.
grep -q "$(printf '\033')\[?1049h" "$WORK/screen" ||
	fail "the panel never took the screen"
grep -q "$(printf '\033')\[?1049l" "$WORK/screen" ||
	fail "the panel took the screen and did not give it back"
printf 'ok   it takes the alternate screen and gives it back\n'

printf '\nall checks passed\n'
