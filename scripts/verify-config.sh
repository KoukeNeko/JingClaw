#!/bin/sh
# Proves the configuration file reaches the running daemon, rather than merely
# parsing: that a missing one is created, that a wrong one is explained instead
# of stack-traced, that an address nobody should write is refused, and that a
# setting which moves where the daemon publishes itself is honoured.
set -eu

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse. Each
# command below is pointed at a throwaway home instead — "none" would say
# there is no deployment at all, and then there is nowhere to create the file
# this file is about.

cd "$(dirname "$0")/../core"


WORK=$(mktemp -d)
BIN="$WORK/jingclaw daemon"
CLI="$WORK/jingclaw"
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

# 1. Nothing configured yet: the daemon puts the file where it belongs.
#
# Nothing may name the deployment here, including anything inherited from a
# caller: this check is about the default, and "none" would say there is no
# deployment at all and so nowhere to put the file.
FRESH="$WORK/fresh-home"
mkdir -p "$FRESH"
HOME="$FRESH" JINGCLAW_HOME= $BIN --print-prompt >/dev/null 2>&1 || true
CREATED=$(find "$FRESH" -name config.toml | head -1)
[ -n "$CREATED" ] || fail "no configuration file was created"
# The header rather than any one setting. Which settings are shown
# uncommented is a presentation decision that changes; this line is what
# identifies the file.
grep -q '^# JingClaw configuration' "$CREATED" || fail "what was created is not the example"
grep -q '^\[gateway\]' "$CREATED" || fail "the created file is missing sections"
printf 'ok   creates a configuration file when there is none\n'

# The settings it shows live are at their defaults, so a file somebody has read
# and not edited behaves exactly as one they never opened. Without this, the
# example is a set of choices nobody made.
HOME="$FRESH" JINGCLAW_HOME= $BIN --print-config > "$WORK/regenerated" 2>/dev/null ||
	fail "the created file could not be read back"
diff -q "$CREATED" "$WORK/regenerated" >/dev/null ||
	fail "reading the created file back produces different settings:
$(diff "$CREATED" "$WORK/regenerated" | head -20)"
printf 'ok   what it writes live is what it would have defaulted to anyway\n'

# The rest name their own configuration, so they need a deployment to belong
# to rather than the operator's. The workspace comes with it: where the agent
# may read and write is a fact about the deployment rather than a setting, so
# a deployment without one is a daemon that cannot start.
export JINGCLAW_HOME="$WORK/deployment/.jingclaw"
mkdir -p "$JINGCLAW_HOME/workspace"

# 2. A wrong setting is explained, not stack-traced.
cat > "$WORK/wrong.toml" <<'EOF'
[server]
addr = "0.0.0.0:9977"
log_level = "verbose"

[provider]
backend = "openai"
EOF
if $BIN --config "$WORK/wrong.toml" >"$WORK/out" 2>&1; then
	fail "agentd started with three broken settings"
fi
for EXPECTED in "$WORK/wrong.toml" 'server.addr = "0.0.0.0:9977"' loopback \
	'server.log_level = "verbose"' 'provider.backend = "openai"' --print-config; do
	grep -q -- "$EXPECTED" "$WORK/out" || fail "the report never mentions $EXPECTED: $(cat "$WORK/out")"
done
printf 'ok   names the file and every wrong setting, not just the first\n'

# 3. The configured runtime_dir is where clients are told to look.
mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
cat > "$WORK/good.toml" <<EOF
[agent]
name = "設定檔測試"
max_iterations = 3

[provider]
backend = "fake"
fake_delay = "1ms"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
log_level = "warn"
EOF

$BIN --config "$WORK/good.toml" >"$WORK/daemon.log" 2>&1 &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "no discovery file in the configured runtime_dir: $(cat "$WORK/daemon.log")"
	sleep 0.1
done
grep -q "$WORK/good.toml" "$WORK/daemon.log" || fail "the daemon does not say which file it read"
printf 'ok   publishes discovery into the configured runtime_dir\n'

# 4. The daemon answers there, and a run completes end to end.
SESSION=$("$CLI" --config "$WORK/good.toml" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session id"
"$CLI" --config "$WORK/good.toml" send "$SESSION" "hello" >/dev/null
printf 'ok   serves a run through the configured location\n'

printf '\nall checks passed\n'
