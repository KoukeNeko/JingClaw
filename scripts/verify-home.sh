#!/bin/sh
# Proves where a deployment lives, and that standing somewhere else does not
# move it.
#
# The failure this guards against happened three times in one day. Two
# directories both looked like real deployments; which one answered depended on
# where the daemon was started from; and the settings somebody edited were not
# the settings that ran. Nothing crashed, and nothing said anything was wrong.
#
# So the check is not "does it start". It is: the same deployment, addressed
# the same way, from three different directories, resolves to the same paths.
set -eu

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

mkdir -p "$WORK/home" "$WORK/project/deep/nested" "$WORK/elsewhere"

# Isolated by moving the user's home, not by naming the deployment.
#
# JINGCLAW_HOME would answer the question this file is asking: it wins over
# everything, so a resolver that still searched the working directory would
# never be consulted and the decoy below would prove nothing. Moving HOME
# leaves the default path in play, which is the path that has to be right.
export HOME="$(cd "$WORK/home" && pwd -P)"
unset JINGCLAW_HOME

# The physical path, because on macOS the temp directory is reached through a
# symlink and the daemon reports where things actually are.
HOME_DIR="$HOME/.jingclaw"

cd "$WORK/project"
"$WORK/agentd" --init >"$WORK/init.out" 2>&1 || fail "--init failed:
$(cat "$WORK/init.out")"

for PLACE in config.toml workspace data run; do
	[ -e "$HOME_DIR/$PLACE" ] || fail "--init did not create $PLACE"
done
printf 'ok   --init creates the directory and what goes in it\n'

# The standing-instruction files, beside the settings and not in the
# workspace. They describe the agent, and the workspace is what the agent may
# change: in there, its own instructions are a file it can edit while doing a
# job, and they sit among a project's files as though they belonged to it.
for NAMED in AGENTS.md PERSONA.md; do
	[ -f "$HOME_DIR/$NAMED" ] || fail "--init did not create $NAMED"
	[ -e "$HOME_DIR/workspace/$NAMED" ] &&
		fail "$NAMED was put in the workspace, where the agent can edit it"
done
printf 'ok   and the instruction files, outside the workspace\n'

# It went where the deployment is, not where somebody was standing.
[ -e "$WORK/project/.jingclaw" ] &&
	fail "--init made a directory in the working directory"
printf 'ok   and not in the directory it was run from\n'

# Creating over an existing one is refused: a "create" that adopts whatever was
# there is how a fresh deployment ends up on another one's database.
if "$WORK/agentd" --init >/dev/null 2>&1; then
	fail "a second --init was allowed over an existing directory"
fi
printf 'ok   it refuses to create one over another\n'

# The check this file exists for. Directories that look exactly like
# deployments sit above two of the three places we start from, which is the
# arrangement that used to decide which one answered.
for DECOY in "$WORK/project" "$WORK/project/deep"; do
	mkdir -p "$DECOY/.jingclaw/data" "$DECOY/.jingclaw/run" "$DECOY/.jingclaw/workspace"
	: > "$DECOY/.jingclaw/config.toml"
done

EXPECTED=$(cd "$WORK/project" && "$WORK/agentd" --print-paths 2>&1) ||
	fail "--print-paths failed: $EXPECTED"

for FROM in "$WORK/project/deep/nested" "$WORK/elsewhere"; do
	ACTUAL=$(cd "$FROM" && "$WORK/agentd" --print-paths 2>&1) ||
		fail "--print-paths failed in $FROM: $ACTUAL"
	[ "$ACTUAL" = "$EXPECTED" ] || fail "starting in $FROM resolved differently:
$ACTUAL

against, from the first directory:
$EXPECTED"
done
printf 'ok   three starting directories, one set of paths\n'

# Every path is inside the deployment, including the workspace: "whatever you
# happened to be standing in" is not a workspace.
cd "$WORK/elsewhere"
REPORTED=$("$WORK/agentd" --print-paths 2>&1)
printf '%s\n' "$REPORTED" | while IFS= read -r LINE; do
	VALUE=$(printf '%s' "$LINE" | sed -n 's/.*: *//p')
	case "$VALUE" in
	"" | "$HOME_DIR"*) ;;
	*) printf 'FAIL: %s is outside the deployment\n' "$LINE" >&2; exit 1 ;;
	esac
done || exit 1
printf 'ok   and every one of them is inside it\n'

# The daemon agrees with what it printed, and a client elsewhere finds it.
cd "$WORK/project/deep/nested"
"$WORK/agentd" >"$WORK/out" 2>"$WORK/err" &
DAEMON=$!

WAITED=0
while [ ! -f "$HOME_DIR/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start:
$(cat "$WORK/err")"
	sleep 0.1
done

LINE=$(grep -o '"msg":"jingclaw daemon listening"[^}]*' "$WORK/err" || true)
[ -n "$LINE" ] || fail "the daemon never reported listening"

for FIELD in config_file database discovery workspace; do
	VALUE=$(echo "$LINE" | tr ',' '\n' | grep "\"$FIELD\"" | cut -d'"' -f4)
	[ -n "$VALUE" ] || fail "the daemon did not report $FIELD"
	case "$VALUE" in
	"$HOME_DIR"/*) ;;
	*) fail "$FIELD resolved to $VALUE, outside the deployment" ;;
	esac
done
printf 'ok   the running daemon resolved everything inside it\n'

cd "$WORK/elsewhere"
SESSION=$("$WORK/agent" session create 2>&1 | tr -d '\r\n')
case "$SESSION" in
ses_*) ;;
*) fail "a client started elsewhere could not find the daemon: $SESSION" ;;
esac
printf 'ok   a client started anywhere finds the same daemon\n'

printf 'PASS: one deployment, wherever it is started from\n'
