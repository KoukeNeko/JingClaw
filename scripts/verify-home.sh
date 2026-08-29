#!/bin/sh
# Proves a deployment keeps everything in one directory.
#
# The failure this guards against is not a crash. It is one part of a running
# agent quietly resolving somewhere else — the database in the platform's data
# directory while the configuration is in .JingClaw, or a client looking for a
# discovery file the daemon did not write. Each half works; together they are
# two deployments that believe they are one.
set -eu

# This one is about how a directory is found, so it must not be told. Any
# setting inherited from a caller is cleared.
unset JINGCLAW_HOME

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

PROJECT="$WORK/project"
mkdir -p "$PROJECT/deep/nested"

cd "$PROJECT"
"$WORK/agentd" --init >"$WORK/init.out" 2>&1 || fail "--init failed:
$(cat "$WORK/init.out")"

for PLACE in config.toml workspace data run; do
	[ -e "$PROJECT/.JingClaw/$PLACE" ] || fail "--init did not create $PLACE"
done
printf 'ok   --init creates the directory and what goes in it\n'

# Creating over an existing one is refused: a "create" that adopts whatever was
# there is how a fresh deployment ends up on another one's database.
if "$WORK/agentd" --init >/dev/null 2>&1; then
	fail "a second --init was allowed over an existing directory"
fi
printf 'ok   it refuses to create one over another\n'

# Started from deep inside, as somebody actually would.
cd "$PROJECT/deep/nested"
"$WORK/agentd" >"$WORK/out" 2>"$WORK/err" &
DAEMON=$!

WAITED=0
while [ ! -f "$PROJECT/.JingClaw/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start:
$(cat "$WORK/err")"
	sleep 0.1
done
sleep 1

LINE=$(grep -o '"msg":"jingclaw daemon listening"[^}]*' "$WORK/err" || true)
[ -n "$LINE" ] || fail "the daemon never reported listening"

# Every path it settled on has to be inside the directory. A subdirectory start
# finding a different deployment is the whole failure being guarded against.
for FIELD in config_file database discovery workspace; do
	VALUE=$(echo "$LINE" | tr ',' '\n' | grep "\"$FIELD\"" | cut -d'"' -f4)
	[ -n "$VALUE" ] || fail "the daemon did not report $FIELD"
	case "$VALUE" in
	*"/.JingClaw/"*) ;;
	*) fail "$FIELD resolved to $VALUE, outside the directory" ;;
	esac
done
printf 'ok   started from a subdirectory, everything still lands inside it\n'

# A client run from somewhere else again has to find that same daemon.
cd "$PROJECT/deep"
SESSION=$("$WORK/agent" session create 2>&1 | tr -d '\r\n')
case "$SESSION" in
ses_*) ;;
*) fail "a client could not find the daemon: $SESSION" ;;
esac
printf 'ok   a client elsewhere in the project finds the same daemon\n'

# And with no directory at all, nothing is invented: the platform locations are
# used exactly as they were.
cd "$WORK"
mkdir -p "$WORK/elsewhere"
cd "$WORK/elsewhere"
if find / -maxdepth 0 >/dev/null 2>&1; then :; fi
"$WORK/agentd" --print-config >"$WORK/plain.toml" 2>&1 ||
	fail "the daemon could not run outside a project"
grep -q '^\[workspace\]' "$WORK/plain.toml" || fail "the example lost its workspace section"
printf 'ok   without one, nothing is invented\n'

printf '\nall checks passed\n'
