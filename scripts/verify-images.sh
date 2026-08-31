#!/bin/sh
# Proves the agent can actually look at a picture.
#
# Every layer of this is a reference passed between components — the adapter
# fetches, the ingress stores, the event names an artifact, the request reads
# it back — and a chain of references is exactly the kind of thing that
# type-checks all the way through while carrying nothing. So this sends a real
# image to a real model and asks a question only the picture answers.
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
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

# Three colour bands, and nothing in the filename or the text says which.
python3 - "$WORK/bands.png" <<'PY'
import struct, sys, zlib

WIDTH, HEIGHT = 120, 90
BANDS = [(220, 30, 30), (30, 200, 60), (40, 70, 230)]

raw = b""
for y in range(HEIGHT):
    raw += b"\x00" + bytes(BANDS[min(y * 3 // HEIGHT, 2)]) * WIDTH

def chunk(tag, data):
    body = tag + data
    return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))

png = b"\x89PNG\r\n\x1a\n"
png += chunk(b"IHDR", struct.pack(">IIBBBBB", WIDTH, HEIGHT, 8, 2, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(raw))
png += chunk(b"IEND", b"")

with open(sys.argv[1], "wb") as out:
    out.write(png)
PY

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

# Configured but not selected, so the switch below is one line.
[provider.gemini]
model = "gemma-4-31b-it"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"

# A room, so a message can arrive the way one from a chat platform does. The
# ingress refuses a channel it has no binding for, which is the point of the
# binding and not something to work around.
[gateway]
platform = "discord"
[gateway.discord]
account_id = "main"
[[gateway.discord.channels]]
channel_ids = ["c1"]
tenant_id = "t1"
workspace_id = "default"
EOF

start() {
	"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
		sleep 0.1
	done
}

agent() { "$WORK/jingclaw" --config "$WORK/config.toml" "$@"; }

start

# 1. A file that is not what it says it is never reaches the store.
printf '#!/bin/sh\nrm -rf /\n' > "$WORK/liar.png"
SESSION=$(agent session create | tr -d '\r\n')
if agent send "$SESSION" "look" --attach "$WORK/liar.png" >/dev/null 2>&1; then
	fail "a shell script named .png was accepted"
fi
printf 'ok   a file that is not what it says it is, is refused\n'

# 2. A real one is accepted and kept.
agent send "$SESSION" "look at this" --attach "$WORK/bands.png" >/dev/null ||
	fail "a real PNG was refused"
sleep 1

agent attach "$SESSION" >"$WORK/events" 2>&1 &
WATCH=$!
sleep 2
kill "$WATCH" 2>/dev/null || true
wait "$WATCH" 2>/dev/null || true
printf 'ok   a real image is accepted\n'

# 3. And it is in the store, not in the event.
DB="$WORK/data/jingclaw.db"
python3 - "$DB" <<'PY' || exit 1
import json, sqlite3, sys

con = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
rows = con.execute("SELECT payload FROM events WHERE kind='user.message'").fetchall()

found = None
for (payload,) in rows:
    message = json.loads(payload)
    for attachment in message.get("attachments") or []:
        found = attachment

if not found:
    print("FAIL: no attachment reached the event log", file=sys.stderr)
    raise SystemExit(1)
if not found.get("artifact_id", "").startswith("sha256-"):
    print("FAIL: the event does not point at an artifact:", found, file=sys.stderr)
    raise SystemExit(1)
if len(payload) > 4000:
    print("FAIL: the event is", len(payload), "bytes — the picture is in it", file=sys.stderr)
    raise SystemExit(1)
PY
printf 'ok   the event names the picture rather than carrying it\n'

# 3b. The other door in, which is the one people actually use.
#
# Everything above goes through SendTurn, and a picture from a chat platform
# does not: it arrives at the gateway ingress, in a message whose bytes the
# adapter fetched. That path was silently dropping every attachment — the
# field did not exist on the wire — and every check in this file passed the
# whole time, because none of them came in through that door.
BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")
PICTURE=$(python3 -c 'import base64,sys;print(base64.b64encode(open(sys.argv[1],"rb").read()).decode())' "$WORK/bands.png")

FROM_CHAT=$(curl -s -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $GATEWAY" -d "{
		\"meta\": {\"clientId\": \"verify\"},
		\"message\": {
			\"platform\": \"discord\", \"accountId\": \"main\",
			\"tenantId\": \"t1\", \"channelId\": \"c1\",
			\"platformMessageId\": \"m1\", \"idempotencyKey\": \"picture-1\",
			\"principalId\": \"p1\", \"principalDisplayName\": \"somebody\",
			\"text\": \"look at this\", \"trigger\": \"MESSAGE_TRIGGER_MENTION\",
			\"attachments\": [{
				\"id\": \"a1\", \"name\": \"bands.png\",
				\"contentType\": \"image/png\", \"size\": 0,
				\"data\": \"$PICTURE\"
			}]
		}
	}" "$BASE/jingclaw.control.v1.GatewayIngressService/DeliverInbound")

CHAT_SESSION=$(printf '%s' "$FROM_CHAT" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
[ -n "$CHAT_SESSION" ] || fail "the message from a chat platform started nothing: $FROM_CHAT"
sleep 1

python3 - "$DB" "$CHAT_SESSION" <<'FROMCHAT' || exit 1
import json, sqlite3, sys

con = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
rows = con.execute(
	"SELECT payload FROM events WHERE kind='user.message' AND session_id=?",
	(sys.argv[2],)).fetchall()

if not rows:
	print("FAIL: the message from a chat platform is not in the log", file=sys.stderr)
	raise SystemExit(1)

for (payload,) in rows:
	for attachment in json.loads(payload).get("attachments") or []:
		if attachment.get("artifact_id", "").startswith("sha256-"):
			raise SystemExit(0)

print("FAIL: a picture sent from a chat platform reached the log with no attachment",
	file=sys.stderr)
raise SystemExit(1)
FROMCHAT
printf 'ok   and a picture from a chat platform survives the trip in\n'

# 4. The half that only a model can answer.
# Finds the operator's own credential and puts it in the environment.
#
# The environment rather than a file, because a daemon looks for a credential
# file inside its own deployment and this check runs in a throwaway one. Where
# the operator keeps theirs is not where this daemon will look.
have_credential() {
	[ -n "${GEMINI_API_KEY:-}" ] && return 0
	[ -n "${GOOGLE_API_KEY:-}" ] && return 0

	for CANDIDATE in \
		"$HOME/.jingclaw/gemini.key" \
		"$HOME/.config/JingClaw/gemini.key" \
		"$HOME/Library/Application Support/JingClaw/gemini.key"; do
		if [ -f "$CANDIDATE" ]; then
			GEMINI_API_KEY=$(tr -d '\r\n' < "$CANDIDATE")
			export GEMINI_API_KEY
			return 0
		fi
	done
	return 1
}

if ! have_credential; then
	printf '\nskipped the seeing check: no provider credential\n'
	printf 'all checks passed\n'
	exit 0
fi

kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
DAEMON=""

sed -e 's|^backend = "fake"|backend = "gemini"|' \
	"$WORK/config.toml" > "$WORK/real.toml"
mv "$WORK/real.toml" "$WORK/config.toml"
start

SEEING=$(agent session create | tr -d '\r\n')
agent attach "$SEEING" >"$WORK/seeing" 2>&1 &
WATCH=$!
agent send "$SEEING" \
	"This image has three horizontal colour bands. Name them from top to bottom, in three words." \
	--attach "$WORK/bands.png" >/dev/null

# Ninety seconds, the same as the other checks that wait on a real model. A
# picture is a large request and this runs alongside twenty-six other checks;
# sixty was enough alone and not enough in the suite, which is the shape of a
# limit set from one machine at rest.
WAITED=0
until grep -q 'run.completed\|run.failed' "$WORK/seeing"; do
	WAITED=$((WAITED + 1))
	if [ "$WAITED" -gt 180 ]; then
		# Said separately because an empty stream and a slow model fail the
		# same way here, and the message that printed nothing at all was the
		# reason one failure could not be told from the other.
		if [ -s "$WORK/seeing" ]; then
			fail "the run never finished:
$(cut -c1-120 "$WORK/seeing" | tail -10)"
		else
			fail "nothing arrived on the event stream at all; the daemon said:
$(tail -5 "$WORK/daemon.err")"
		fi
	fi
	sleep 0.5
done
kill "$WATCH" 2>/dev/null || true
wait "$WATCH" 2>/dev/null || true

grep -q 'run.failed' "$WORK/seeing" &&
	fail "the run failed:
$(cut -c1-160 "$WORK/seeing" | tail -10)"

# Nothing in the prompt or the filename says which colours. Only the picture does.
ANSWER=$(tr 'A-Z' 'a-z' < "$WORK/seeing")
for COLOUR in red green blue; do
	printf '%s' "$ANSWER" | grep -q "$COLOUR" ||
		fail "the model did not name $COLOUR, so it did not see the picture:
$(cut -c1-160 "$WORK/seeing" | tail -10)"
done
printf 'ok   the model named the colours only the picture could tell it\n'

printf '\nall checks passed\n'
