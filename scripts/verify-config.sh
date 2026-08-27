#!/bin/sh
# Proves the configuration file reaches the running daemon, rather than merely
# parsing: that a missing one is created, that a wrong one is explained instead
# of stack-traced, that an address nobody should write is refused, and that a
# setting which moves where the daemon publishes itself is honoured.
set -eu

cd "$(dirname "$0")/../core"


WORK=$(mktemp -d)
BIN="$WORK/agentd"
CLI="$WORK/agent"
go build -o "$BIN" ./cmd/agentd
go build -o "$CLI" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# 1. Nothing configured yet: the daemon puts the file where it belongs.
FRESH="$WORK/fresh-home"
mkdir -p "$FRESH"
HOME="$FRESH" XDG_CONFIG_HOME="$FRESH/.config" "$BIN" --print-prompt >/dev/null 2>&1 || true
CREATED=$(find "$FRESH" -name config.toml | head -1)
[ -n "$CREATED" ] || fail "no configuration file was created"
grep -q '^# name = "JingClaw"' "$CREATED" || fail "what was created is not the commented example"
grep -q '^\[gateway\]' "$CREATED" || fail "the created file is missing sections"
printf 'ok   creates a configuration file when there is none\n'

# 2. A wrong setting is explained, not stack-traced.
cat > "$WORK/wrong.toml" <<'EOF'
[server]
addr = "0.0.0.0:9977"
log_level = "verbose"

[model]
provider = "openai"
EOF
if "$BIN" --config "$WORK/wrong.toml" >"$WORK/out" 2>&1; then
	fail "agentd started with three broken settings"
fi
for EXPECTED in "$WORK/wrong.toml" 'server.addr = "0.0.0.0:9977"' loopback \
	'server.log_level = "verbose"' 'model.provider = "openai"' --print-config; do
	grep -q -- "$EXPECTED" "$WORK/out" || fail "the report never mentions $EXPECTED: $(cat "$WORK/out")"
done
printf 'ok   names the file and every wrong setting, not just the first\n'

# 3. The configured runtime_dir is where clients are told to look.
mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/good.toml" <<EOF
[agent]
name = "設定檔測試"
max_iterations = 3

[model]
provider = "fake"
fake_delay = "1ms"

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
log_level = "warn"
EOF

"$BIN" --config "$WORK/good.toml" >"$WORK/daemon.log" 2>&1 &
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
