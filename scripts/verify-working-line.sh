#!/bin/sh
# Proves a run says what it is doing while it is doing it.
#
# A channel that hears nothing for a minute cannot tell working from broken,
# and that is the state somebody reaches for the logs about. There were
# reactions before this — a globe, a notebook, a brain — and they say which
# kind of thing is happening and never which page it went to or which file it
# opened, which is the part somebody watching actually wants.
#
# Driven through Telegram because that is the platform with a stub. The line
# itself is assembled in the projector and the renderer, which both platforms
# share; what Discord does with it after that has no stub to be checked
# against, like the rest of that adapter.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
GATEWAY=""
STUB=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands,
	# leaving the rest running and holding their ports.
	set +e
	[ -n "$GATEWAY" ] && kill "$GATEWAY" 2>/dev/null
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$STUB" ] && kill "$STUB" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

CHAT_ID=900000000000000031
USER_ID=900000000000000032

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
printf 'the notes\n' > "$WORK/workspace/notes.md"

python3 ../scripts/support/telegram-stub.py 7806 "$WORK/calls.jsonl" \
	"$CHAT_ID" "$USER_ID" 2>/dev/null &
STUB=$!

WAITED=0
while ! curl -s -o /dev/null -X POST "http://127.0.0.1:7806/botx/getMe"; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub never came up"
	sleep 0.1
done
: > "$WORK/calls.jsonl"

# Scripted calls, so the run has things to say it is doing. A run that only
# thinks goes straight from starting to answering, and the lines that name a
# file and an address are never drawn.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Looking."
tool = "read_file"
args = '{"path":"notes.md"}'

[[provider.fake_script]]
text = "Nothing much in it."

[server]
addr = "127.0.0.1:7805"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "telegram"
[gateway.telegram]
account_id = "main"
api_base = "http://127.0.0.1:7806"
[[gateway.telegram.channels]]
channel_ids = ["$CHAT_ID"]
tenant_id = "$CHAT_ID"
workspace_id = "default"
users = ["$USER_ID"]
EOF

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

TELEGRAM_BOT_TOKEN=stub-token \
	"$WORK/jingclaw" gateway --config "$WORK/config.toml" \
	>"$WORK/gw.out" 2>"$WORK/gw.err" &
GATEWAY=$!

WAITED=0
while ! grep -q '"method": "sendMessage"' "$WORK/calls.jsonl" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the run never said anything: $(tail -5 "$WORK/gw.err")"
	sleep 0.1
done
sleep 2

python3 - "$WORK/calls.jsonl" <<'CHECK' || exit 1
import json, sys

calls = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
said = [c["body"].get("text", "") for c in calls
        if c["method"] in ("sendMessage", "editMessageText")]


def fail(why):
    print("FAIL: " + why, file=sys.stderr)
    raise SystemExit(1)


if not said:
    fail("nothing was posted at all")

# It says it has started. This is the first thing anybody sees, and it used to
# be blank on a platform that draws words: the state carried nothing to draw.
if not any("thinking" in text for text in said):
    fail("the run never said it had started: %r" % said)
print("ok   a run that starts says it is thinking")

# And which file. The whole reason for a line over an emoji is that it names
# the thing: every file read shows the same notebook otherwise.
if not any("notes.md" in text for text in said):
    fail("nothing said which file it opened: %r" % said)
print("ok   and names the file it opened, not just that it opened one")

# One line, rewritten. A run that reads six files must not leave six of these.
working = [text for text in said if "⋯" in text]
posted = [c for c in calls
          if c["method"] == "sendMessage" and "⋯" in c["body"].get("text", "")]
if len(working) < 2:
    fail("the line was never rewritten, so it says one thing per run: %r" % working)
if len(posted) > 1:
    fail("%d separate working lines were posted rather than one rewritten" % len(posted))
print("ok   one line, rewritten, rather than one message per thing it does")
CHECK

printf '\nall checks passed\n'
