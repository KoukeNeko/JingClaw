#!/bin/sh
# Proves a tool server reaches the model and back: the daemon starts the child
# process, its tools arrive in the registry under namespaced names, they are
# offered to the model, and the child is gone when the daemon stops.
#
# The server is the internal/mcp test binary, which runs as a real stdio MCP
# server when JINGCLAW_TEST_MCP_SERVER is set. That keeps this offline and
# deterministic instead of depending on somebody's npm package.
set -eu

cd "$(dirname "$0")/../core"


WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse.
# Where the agent may read and write is this directory's workspace, which is
# why the check has to have one rather than simply having none.
export JINGCLAW_HOME="$WORK"

BIN="$WORK/jingclaw daemon"
SERVER="$WORK/mcp-server"
go build -o "$WORK/jingclaw" ./cmd/jingclaw
go test -c ./internal/mcp -o "$SERVER"

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
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
log_level = "info"

[[mcp.servers]]
name = "helper"
command = "$SERVER"
level = "workspace_read"

[mcp.servers.env]
JINGCLAW_TEST_MCP_SERVER = "1"
EOF

# 1. The tools reach the prompt the model is given.
PROMPT=$($BIN --config "$WORK/config.toml" --print-prompt 2>"$WORK/prompt.err") ||
	fail "printing the prompt failed: $(cat "$WORK/prompt.err")"

printf '%s' "$PROMPT" | grep -q 'mcp_helper_echo' ||
	fail "the server's tools never reached the prompt"
printf '%s' "$PROMPT" | grep -q 'read_file' ||
	fail "the built-in tools went missing once a server was configured"
printf 'ok   a server tool reaches the model, alongside the built-ins\n'

printf '%s' "$PROMPT" | grep -q 'mcp_helper_read_file' &&
	fail "a server tool was allowed to shadow a built-in"
printf 'ok   namespacing keeps read_file meaning read_file\n'

# 2. The daemon starts the child and says how many answered.
$BIN --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

grep -q 'of 1 mcp servers' "$WORK/daemon.out" ||
	fail "the banner does not say how many servers answered: $(cat "$WORK/daemon.out")"
grep -q '1 of 1' "$WORK/daemon.out" ||
	fail "the configured server did not connect: $(cat "$WORK/daemon.out")"
printf 'ok   starts the server and says so in the banner\n'

CHILDREN=$(pgrep -f "$SERVER" | wc -l | tr -d ' ')
[ "$CHILDREN" -ge 1 ] || fail "no server process is running"
printf 'ok   the server is a live child process\n'

# 3. Stopping the daemon stops its children. A daemon restart that leaves
#    orphans behind fills a machine with them.
kill -TERM "$DAEMON"
wait "$DAEMON" 2>/dev/null || true
DAEMON=""

WAITED=0
while [ "$(pgrep -f "$SERVER" | wc -l | tr -d ' ')" -ne 0 ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the server outlived the daemon that started it"
	sleep 0.1
done
printf 'ok   the server stops when the daemon does\n'

printf '\nall checks passed\n'
