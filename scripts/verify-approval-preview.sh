#!/bin/sh
# Proves a person asked to approve a change can actually see the change.
#
# The arguments of an edit are the old text and the new text in full, which is
# not something anybody reviews. What reaches a client is the diff between
# them, rendered once in the daemon by the tool that defined the arguments —
# so three clients show the same review rather than three implementations of
# guessing at it.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
printf 'timeout := 30\n' > "$WORK/ws/settings.go"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

# Read first: an edit refuses a file this session has not looked at, which is
# a rule worth keeping and not worth working around here.
[[provider.fake_script]]
text = "Looking at it."
tool = "read_file"
args = '{"path":"settings.go"}'

[[provider.fake_script]]
text = "Raising it."
tool = "edit_file"
args = '{"path":"settings.go","edits":[{"old_text":"timeout := 30","new_text":"timeout := 120"}]}'

[[provider.fake_script]]
text = "Done."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7796"
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

"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.3

"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" "raise the timeout" >/dev/null

WAITED=0
while ! grep -q 'approval.requested' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 200 ] && fail "no approval was raised: $(cat "$WORK/events")"
	sleep 0.1
done
printf 'ok   an edit stops and asks\n'

# 1. What a person reviewing it is shown.
LISTED=$("$WORK/jingclaw" --config "$WORK/config.toml" approvals "$SESSION")
printf '%s' "$LISTED" | grep -q -- '-timeout := 30' ||
	fail "the review does not show what is being removed:
$LISTED"
printf '%s' "$LISTED" | grep -q -- '+timeout := 120' ||
	fail "the review does not show what is being added:
$LISTED"
printf 'ok   and shows the change as a diff, not as raw arguments\n'

printf '%s' "$LISTED" | grep -q 'settings.go' ||
	fail "the review does not name the file:
$LISTED"
printf 'ok   naming the file it would change\n'

# 2. A preview must not be the edit happening early. Nothing has been decided,
#    so the file has to be untouched.
BEFORE=$(cat "$WORK/ws/settings.go")
[ "$BEFORE" = "timeout := 30" ] ||
	fail "the file changed before anybody approved: $BEFORE"
printf 'ok   and nothing was changed by rendering it\n'

# 3. The arguments are still there. A decision is made against the call, not
#    against a rendering that might disagree with it.
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
RAW=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" \
	-d "{\"sessionId\":\"$SESSION\"}" \
	"http://127.0.0.1:7796/jingclaw.control.v1.SessionService/ListApprovals")
printf '%s' "$RAW" | grep -q '"arguments"' ||
	fail "the exact arguments are no longer offered: $RAW"
printf '%s' "$RAW" | grep -q '"preview"' ||
	fail "the preview is not on the wire: $RAW"
printf 'ok   alongside the exact arguments, not instead of them\n'

# 4. Approving it does what the review said it would.
APPROVAL=$(printf '%s' "$LISTED" | grep -o 'apr_[A-Za-z0-9]*' | head -1)
"$WORK/jingclaw" --config "$WORK/config.toml" approve "$APPROVAL" >/dev/null

WAITED=0
while ! grep -q 'timeout := 120' "$WORK/ws/settings.go" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "approving it did not make the change: $(cat "$WORK/ws/settings.go")
$(cut -c1-160 "$WORK/events" | tail -10)"
	sleep 0.1
done
printf 'ok   and approving it makes exactly that change\n'

kill "$ATTACH" 2>/dev/null || true

printf '\nPASS: a change can be reviewed before it happens\n'
