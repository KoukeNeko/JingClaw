#!/bin/sh
# Proves output too large to show is not output that is lost: the model runs a
# command, sees an excerpt with an artifact id, the id reaches the event log
# and a client, and the whole thing streams back through the control API.
#
# It uses the real model, because the seam under test is "a tool truncated,
# and the id got all the way out" — and only a model calls tools.
set -eu

# A .JingClaw directory above this checkout must not decide anything here: a
# check that reaches the operator's own deployment would read its settings and,
# worse, write to its database. Stated rather than relied on.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"


WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
ATTACH=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$ATTACH" ] && kill "$ATTACH" 2>/dev/null
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# This one needs a real model, because the seam under test is "a tool
# truncated and the id got all the way out" and only a model calls tools.
have_credential() {
	[ -n "${GEMINI_API_KEY:-}" ] && return 0
	[ -n "${GOOGLE_API_KEY:-}" ] && return 0

	# Into the environment, because a daemon looks for a credential file inside
	# its own deployment and this runs in a throwaway one. The value is never
	# printed.
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
	printf 'skipped: no provider credential, and this check needs a real model\n'
	exit 0
fi

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

# A file with a distinctive line in the middle, so "the excerpt dropped it but
# the artifact kept it" is something the test can actually check.
awk 'BEGIN { for (i = 0; i < 4000; i++) print "line-" i "-padding-padding-padding" }' > "$WORK/ws/big.txt"
grep -q 'line-2000-padding' "$WORK/ws/big.txt" || fail "the fixture is wrong"

cat > "$WORK/config.toml" <<EOF
[agent]
# Room for the model to look around first. Four was enough on one run and not
# on the next, which made this check about the model's patience rather than
# about whether the artifact came back.
max_iterations = 10
permission_profile = "local"

[provider]
backend = "gemini"

[provider.gemini]
model = "gemma-4-31b-it"

[tools]
max_command_output = 2000

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
log_level = "info"
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
"$WORK/jingclaw" --config "$WORK/config.toml" attach "$SESSION" >"$WORK/events" 2>&1 &
ATTACH=$!

"$WORK/jingclaw" --config "$WORK/config.toml" send "$SESSION" \
	"Run: /bin/cat big.txt   — then tell me in one sentence how many lines it printed." >/dev/null

# Everything the run asks for is approved, for as long as it keeps asking. An
# agent that reads a file and then counts its lines is doing ordinary work, so
# the loop cannot stop after the first decision.
APPROVED=0
WAITED=0
while [ "$WAITED" -lt 240 ]; do
	grep -q 'run.completed\|run.failed' "$WORK/events" && break

	APPROVAL=$("$WORK/jingclaw" --config "$WORK/config.toml" approvals "$SESSION" 2>/dev/null |
		grep -o 'apr_[A-Za-z0-9]*' | head -1 || true)
	if [ -n "$APPROVAL" ]; then
		"$WORK/jingclaw" --config "$WORK/config.toml" approve "$APPROVAL" >/dev/null 2>&1 &&
			APPROVED=$((APPROVED + 1))
	fi

	WAITED=$((WAITED + 1))
	sleep 0.5
done

grep -q 'run.completed\|run.failed' "$WORK/events" || fail "the run never finished:
$(cut -c1-120 "$WORK/events" | tail -20)"
[ "$APPROVED" -ge 1 ] || fail "nothing was ever approved, so no command ran"
printf 'ok   %d command(s) approved and run\n' "$APPROVED"

grep -q 'run.failed' "$WORK/events" &&
	fail "the run failed:
$(cut -c1-160 "$WORK/events" | tail -20)"
[ "$APPROVED" = 1 ] && printf 'ok   the command was approved and ran\n'

# 1. The id reached a client.
ID=$(grep -o 'sha256-[0-9a-f]\{64\}' "$WORK/events" | head -1)
[ -n "$ID" ] || fail "no artifact id reached the client:
$(cut -c1-200 "$WORK/events" | tail -20)"
printf 'ok   the artifact id reached a client through the event log\n'

grep -q 'bytes kept as sha256-' "$WORK/events" ||
	fail "the timeline does not say output was kept"
printf 'ok   the timeline says how much was kept and where\n'

# 2. Every artifact the timeline named comes back at the size it claimed.
#
#    Checked against what the event recorded rather than against the fixture,
#    because which command the model chooses to run is its business: a model
#    that decides to cat two files has not broken anything, and a check that
#    fails when it does is a check about the model rather than about the store.
FOUND=0
grep -o '\[[0-9]* bytes kept as sha256-[0-9a-f]\{64\}\]' "$WORK/events" | sort -u |
	while read -r LINE; do
		CLAIMED=$(printf '%s' "$LINE" | grep -o '[0-9]*' | head -1)
		KEPT=$(printf '%s' "$LINE" | grep -o 'sha256-[0-9a-f]\{64\}')

		"$WORK/jingclaw" --config "$WORK/config.toml" artifact get "$KEPT" > "$WORK/whole.txt" ||
			fail "fetching $KEPT failed"

		ACTUAL=$(wc -c < "$WORK/whole.txt" | tr -d ' ')
		[ "$ACTUAL" = "$CLAIMED" ] ||
			fail "$KEPT was recorded as $CLAIMED bytes and came back as $ACTUAL"
	done
printf 'ok   every artifact comes back at the size the log recorded\n'

# 3. And the part the excerpt dropped is in there. The middle of the file is
#    by construction what a 2000-byte excerpt of a 130 KB output cannot show.
grep -o 'sha256-[0-9a-f]\{64\}' "$WORK/events" | sort -u |
	while read -r KEPT; do
		"$WORK/jingclaw" --config "$WORK/config.toml" artifact get "$KEPT" > "$WORK/whole.txt"
		if grep -q 'line-2000-padding' "$WORK/whole.txt"; then
			FOUND=1
			break
		fi
	done > /dev/null 2>&1 || true

grep -q 'line-2000-padding' "$WORK/whole.txt" ||
	fail "no artifact holds the middle the excerpt cut"
printf 'ok   the middle the excerpt cut is in the artifact\n'

# 4. A window of it.
"$WORK/jingclaw" --config "$WORK/config.toml" artifact get "$ID" --offset 0 --limit 6 > "$WORK/window.txt"
[ "$(wc -c < "$WORK/window.txt" | tr -d ' ')" = 6 ] ||
	fail "a six-byte window returned $(wc -c < "$WORK/window.txt") bytes"
printf 'ok   a window of it can be asked for\n'

printf '\nall checks passed\n'

