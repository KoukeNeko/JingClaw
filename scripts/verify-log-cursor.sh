#!/bin/sh
# Proves a client can follow every session from one stream and resume where it
# stopped.
#
# The point of the check is the resume, not the reading. A console watching
# three conversations reconnects with one number, and if that number means
# anything less than "how far through the log I have read", it silently misses
# whatever happened in the sessions it was not counting.
set -eu

# The operator's own deployment must not decide anything here: a check that
# reached it would read its settings and, worse, write to its database.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
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

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7788"
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

# Two conversations, interleaved. One is the case a per-session number can
# describe; two at once is the case it cannot.
FIRST=$(call CreateSession '{"title":"first"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
SECOND=$(call CreateSession '{"title":"second"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')

call SendTurn "{\"sessionId\":\"$FIRST\",\"text\":\"one\"}" >/dev/null
call SendTurn "{\"sessionId\":\"$SECOND\",\"text\":\"two\"}" >/dev/null
call SendTurn "{\"sessionId\":\"$FIRST\",\"text\":\"three\"}" >/dev/null

# Let the runs finish so the log stops growing under the reads below.
sleep 2

# A server-streaming call is not a plain POST: the body is one length-prefixed
# frame, a flag byte then a big-endian length. Written in Python rather than in
# shell because getting it wrong by a byte is not a mistake this check is for.
read_log() {
	python3 - "$1" "$WORK/framed" <<'FRAME'
import json, struct, sys

body = json.dumps({"clientId": "verify", "afterCursor": sys.argv[1]}).encode()
with open(sys.argv[2], "wb") as out:
	out.write(struct.pack(">BI", 0, len(body)))
	out.write(body)
FRAME

	curl -s -X POST --data-binary "@$WORK/framed" \
		-H 'content-type: application/connect+json' \
		-H 'connect-protocol-version: 1' \
		-H "authorization: Bearer $TOKEN" \
		--max-time 4 \
		"$BASE/jingclaw.control.v1.SessionService/SubscribeAllEvents" 2>/dev/null || true
}

# The response is framed the same way the request is: five bytes of prefix in
# front of each message. Split once, here, rather than in each check below.
cat > "$WORK/frames.py" <<'FRAMES'
import json, struct, sys


def messages(path):
	"""Yield each JSON message in a Connect stream."""
	with open(path, "rb") as stream:
		raw = stream.read()

	at = 0
	while at + 5 <= len(raw):
		_, length = struct.unpack(">BI", raw[at:at + 5])
		at += 5
		if at + length > len(raw):
			break
		yield json.loads(raw[at:at + length])
		at += length


def positions(path):
	"""Yield (global position, session) for each event in the stream."""
	for message in messages(path):
		event = message.get("event")
		if event:
			yield int(event.get("globalSeq", 0)), event["sessionId"]
FRAMES

read_log 0 > "$WORK/all"

grep -q '"hello"' "$WORK/all" || fail "the stream did not open with where the log is: $(head -c 300 "$WORK/all")"
printf 'ok   the stream says where the whole log is\n'

python3 - "$WORK/all" "$FIRST" "$SECOND" <<'CHECK' || exit 1
import sys

sys.path.insert(0, __import__("os").path.dirname(sys.argv[1]))
from frames import positions as read

path, first, second = sys.argv[1], sys.argv[2], sys.argv[3]

positions, sessions = [], set()
for position, session in read(path):
    positions.append(position)
    sessions.add(session)

if {first, second} - sessions:
    print(f"FAIL: one stream did not carry both sessions: {sessions}", file=sys.stderr)
    raise SystemExit(1)

if not positions:
    print("FAIL: no events arrived at all", file=sys.stderr)
    raise SystemExit(1)

if 0 in positions:
    print("FAIL: an event arrived with no position in the log", file=sys.stderr)
    raise SystemExit(1)

if positions != sorted(positions):
    print(f"FAIL: the log arrived out of order: {positions[:12]}", file=sys.stderr)
    raise SystemExit(1)

if len(set(positions)) != len(positions):
    print("FAIL: two events claimed the same position", file=sys.stderr)
    raise SystemExit(1)

CHECK

printf 'ok   one stream carries every session, in one order\n'

MIDDLE=$(python3 - "$WORK/all" <<'PICK'
import os, sys

sys.path.insert(0, os.path.dirname(sys.argv[1]))
from frames import positions as read

at = [position for position, _ in read(sys.argv[1])]
print(at[len(at) // 2])
PICK
)

read_log "$MIDDLE" > "$WORK/resumed"

python3 - "$WORK/resumed" "$MIDDLE" <<'CHECK' || exit 1
import os, sys

sys.path.insert(0, os.path.dirname(sys.argv[1]))
from frames import positions as read

path, after = sys.argv[1], int(sys.argv[2])
positions = [position for position, _ in read(path)]

if not positions:
    print(f"FAIL: resuming after {after} returned nothing at all", file=sys.stderr)
    raise SystemExit(1)

if min(positions) <= after:
    print(f"FAIL: resuming after {after} replayed {min(positions)}", file=sys.stderr)
    raise SystemExit(1)

if positions[0] != after + 1:
    print(f"FAIL: resuming after {after} skipped to {positions[0]}", file=sys.stderr)
    raise SystemExit(1)
CHECK

printf 'ok   and a reader resumes exactly where it stopped\n'

printf '\nall checks passed\n'
