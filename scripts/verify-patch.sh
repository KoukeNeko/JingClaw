#!/bin/sh
# Proves a change that spans files lands together or not at all.
#
# The reason apply_patch is worth its own tool rather than a loop over
# edit_file: one approval, and a workspace that never passes through a state
# nobody asked for. Half of a rename is worse than none of it — the code does
# not build, and the model's next read gets the mixture.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
ATTACH=""
APPROVER=""
cleanup() {
	[ -n "$APPROVER" ] && kill "$APPROVER" 2>/dev/null || true
	[ -n "$ATTACH" ] && kill "$ATTACH" 2>/dev/null || true
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
printf 'x := oldName()\n' > "$WORK/ws/caller.go"
printf 'func oldName() int { return 1 }\n' > "$WORK/ws/defn.go"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

# Read both first: a patch refuses a file this session has not looked at, and
# that rule is worth keeping rather than working around here.
[[provider.fake_script]]
text = "Looking at the caller."
tool = "read_file"
args = '{"path":"caller.go"}'

[[provider.fake_script]]
text = "And the definition."
tool = "read_file"
args = '{"path":"defn.go"}'

[[provider.fake_script]]
text = "Renaming it everywhere."
tool = "apply_patch"
args = '{"operations":[{"op":"update","path":"caller.go","edits":[{"old_text":"oldName()","new_text":"newName()"}]},{"op":"update","path":"defn.go","edits":[{"old_text":"func oldName()","new_text":"func newName()"}]},{"op":"create","path":"notes/why.md","content":"renamed for clarity\\n"}]}'

[[provider.fake_script]]
text = "Renamed."

[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7812"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

SESSION=$("$WORK/agent" --config "$WORK/config.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session"

"$WORK/agent" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!
sleep 0.3

# The review is captured while it is still pending, because that is when a
# person sees it — and then approved. Approving in a loop would race past the
# thing being checked.
approve_everything() {
	WAITED=0
	while [ "$WAITED" -lt 200 ]; do
		LISTING=$("$WORK/agent" --config "$WORK/config.toml" approvals "$1" 2>/dev/null || true)
		WAITING=$(printf '%s' "$LISTING" | grep -o 'apr_[A-Za-z0-9]*' | head -1 || true)
		if [ -n "$WAITING" ]; then
			printf '%s\n' "$LISTING" >> "$WORK/reviews"
			"$WORK/agent" --config "$WORK/config.toml" approve "$WAITING" >/dev/null 2>&1 || true
			echo "$WAITING" >> "$WORK/approved"
		fi
		WAITED=$((WAITED + 1))
		sleep 0.1
	done
}
: > "$WORK/approved"
: > "$WORK/reviews"
approve_everything "$SESSION" &
APPROVER=$!

"$WORK/agent" --config "$WORK/config.toml" send "$SESSION" "rename oldName to newName" >/dev/null

WAITED=0
while ! grep -q 'run.completed\|run.failed' "$WORK/events" 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 400 ] && fail "the run never finished:
$(cut -c1-140 "$WORK/events" | tail -8)"
	sleep 0.1
done
grep -q 'run.failed' "$WORK/events" &&
	fail "the run failed:
$(cut -c1-160 "$WORK/events" | tail -10)"
printf 'ok   a model can patch several files in one call\n'

kill "$APPROVER" 2>/dev/null || true
APPROVER=""

# Every file changed.
grep -q 'newName()' "$WORK/ws/caller.go" || fail "caller.go was not updated"
grep -q 'func newName()' "$WORK/ws/defn.go" || fail "defn.go was not updated"
[ -f "$WORK/ws/notes/why.md" ] || fail "the new file was not created"
printf 'ok   and every file in it changed, including a new one\n'

# One approval, not one per file. This is what makes the patch worth having
# over three edit_file calls.
DECISIONS=$(sort -u "$WORK/approved" | wc -l | tr -d ' ')
[ "$DECISIONS" = 1 ] || fail "the patch asked for $DECISIONS approvals, want one"
printf 'ok   asking once rather than once per file\n'

# What the person was shown before allowing it covers the whole patch: a
# review that showed one of three files would have them approving the rest
# unseen.
grep -q -- '-oldName()' "$WORK/reviews" ||
	fail "the review did not show what would be removed:
$(cat "$WORK/reviews")"
grep -q -- '+func newName()' "$WORK/reviews" ||
	fail "the review did not show the other file's change:
$(cat "$WORK/reviews")"
grep -q 'notes/why.md' "$WORK/reviews" ||
	fail "the review did not mention the file being created:
$(cat "$WORK/reviews")"
printf 'ok   and the review covers every file it touches\n'

printf '\nPASS: a change across files lands as one thing\n'
