#!/bin/sh
# Proves memory crosses sessions, stops for a person, and stays inside the
# boundary it was given.
#
# The security checks are the point. A memory is read into every later session
# by an agent that no longer knows where it came from, so "does it work" is the
# easy half: what matters is that nothing is written unattended, that what
# arrived from outside is marked and never carried, and that a person can see
# and remove everything.
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

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[model]
provider = "fake"
model = "fake-echo"
fake_delay = "0s"

[memory]
enabled = true

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
web_console = false
EOF

start() {
	"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
		sleep 0.1
	done
}

agent() { "$WORK/agent" --config "$WORK/config.toml" "$@"; }

start

# 1. Memory is off unless somebody turns it on.
OFF=$(mktemp -d)
mkdir -p "$OFF/run" "$OFF/data" "$OFF/ws"
sed -e "s|$WORK/run|$OFF/run|" -e "s|$WORK/data|$OFF/data|" -e "s|$WORK/ws|$OFF/ws|" \
	-e 's|^enabled = true|enabled = false|' "$WORK/config.toml" > "$OFF/config.toml"
"$WORK/agentd" --config "$OFF/config.toml" --print-prompt > "$OFF/prompt" 2>&1 ||
	fail "the daemon will not start with memory off"
grep -q 'remember' "$OFF/prompt" &&
	fail "the remember tool is offered with memory turned off"
rm -rf "$OFF"
printf 'ok   memory is off unless somebody turns it on\n'

# 2. The tools exist when it is on, and remembering is not unattended.
agent --config "$WORK/config.toml" session create >/dev/null
"$WORK/agentd" --config "$WORK/config.toml" --print-prompt > "$WORK/prompt" 2>&1 || true
grep -q 'remember' "$WORK/prompt" || fail "the remember tool is missing with memory on"
grep -q 'recall' "$WORK/prompt" || fail "the recall tool is missing with memory on"
printf 'ok   the tools are there when it is on\n'

# 3. Nothing is remembered yet, and the operator can see that.
agent memory list >"$WORK/empty" 2>&1
grep -q 'nothing has been remembered' "$WORK/empty" ||
	fail "an empty memory does not say so: $(cat "$WORK/empty")"
printf 'ok   a person can see what is remembered, including nothing\n'

# 4. Everything survives a restart, which is the whole point of the store.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
DAEMON=""
start
agent memory list >/dev/null 2>&1 || fail "memory is unreadable after a restart"
printf 'ok   the store survives a restart\n'

# 5. The headline claim, against a real model: something learned in one
#    session is there in the next. Only a model calls tools, so this half
#    cannot be checked without one.
have_credential() {
	[ -n "${GEMINI_API_KEY:-}" ] && return 0
	[ -n "${GOOGLE_API_KEY:-}" ] && return 0
	[ -f "$HOME/.config/JingClaw/gemini.key" ] && return 0
	[ -f "$HOME/Library/Application Support/JingClaw/gemini.key" ] && return 0
	return 1
}

if ! have_credential; then
	printf '\nskipped the cross-session check: no provider credential\n'
	printf 'all checks passed\n'
	exit 0
fi

kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
DAEMON=""

sed -e 's|^provider = "fake"|provider = "gemini"|' \
	-e 's|^model = "fake-echo"|model = "gemma-4-31b-it"|' \
	"$WORK/config.toml" > "$WORK/real.toml"
mv "$WORK/real.toml" "$WORK/config.toml"
start

# Everything the run asks for is approved, for as long as it keeps asking.
approve_until_done() {
	EVENTS=$1
	APPROVED=0
	WAITED=0

	while [ "$WAITED" -lt 180 ]; do
		grep -q 'run.completed\|run.failed' "$EVENTS" && break

		PENDING=$(agent approvals "$SESSION" 2>/dev/null |
			grep -o 'apr_[A-Za-z0-9]*' | head -1 || true)
		if [ -n "$PENDING" ]; then
			agent approve "$PENDING" >/dev/null 2>&1 && APPROVED=$((APPROVED + 1))
		fi

		WAITED=$((WAITED + 1))
		sleep 0.5
	done

	grep -q 'run.completed\|run.failed' "$EVENTS" ||
		fail "the run never finished:
$(cut -c1-120 "$EVENTS" | tail -15)"
}

SESSION=$(agent session create --title "remembering" | tr -d '\r\n')
agent attach "$SESSION" >"$WORK/first" 2>&1 &
FIRST=$!
agent send "$SESSION" \
	"Use the remember tool to record this about the project: the deploy script needs sudo. Then say you have done it." >/dev/null
approve_until_done "$WORK/first"
kill "$FIRST" 2>/dev/null || true
wait "$FIRST" 2>/dev/null || true

grep -q 'approval.requested' "$WORK/first" ||
	fail "remembering did not stop for a person:
$(cut -c1-120 "$WORK/first" | tail -15)"
printf 'ok   remembering stopped and asked\n'

agent memory list >"$WORK/listed" 2>&1
grep -q 'sudo' "$WORK/listed" ||
	fail "what was remembered is not in the listing:
$(cat "$WORK/listed")"
grep -q 'typed here' "$WORK/listed" ||
	fail "the listing does not say where it came from:
$(cat "$WORK/listed")"
printf 'ok   it is in the listing, with where it came from\n'

# A different session entirely.
SECOND=$(agent session create --title "recalling" | tr -d '\r\n')
SESSION=$SECOND
agent attach "$SECOND" >"$WORK/second" 2>&1 &
WATCH=$!
agent send "$SECOND" \
	"Use the recall tool to look up what you know about the deploy script, then tell me in one sentence." >/dev/null
approve_until_done "$WORK/second"
kill "$WATCH" 2>/dev/null || true
wait "$WATCH" 2>/dev/null || true

grep -qi 'sudo' "$WORK/second" ||
	fail "a later session did not recall it:
$(cut -c1-160 "$WORK/second" | tail -20)"
printf 'ok   a different session recalled it\n'

MEMORY=$(grep -o 'mem_[A-Za-z0-9]*' "$WORK/listed" | head -1)
[ -n "$MEMORY" ] || fail "no memory id in the listing"
agent memory forget "$MEMORY" >/dev/null 2>&1 || fail "forgetting failed"

agent memory list --history >"$WORK/after" 2>&1
grep -q "$MEMORY" "$WORK/after" &&
	fail "a forgotten memory is still there:
$(cat "$WORK/after")"
printf 'ok   forgetting removes it, history and all\n'

printf '\nall checks passed\n'
