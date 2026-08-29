#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(dirname -- "$SCRIPT_DIR")
CORE_DIR=$PROJECT_DIR/core
GO_BIN=${GO_BIN:-go}
AGENTD_BIN=${AGENTD_BIN:-$PROJECT_DIR/core/bin/agentd}
AGENTD_LOG=${AGENTD_LOG:-$PROJECT_DIR/agentd.log}
GATEWAYD_BIN=${GATEWAYD_BIN:-$PROJECT_DIR/core/bin/gatewayd}
GATEWAYD_LOG=${GATEWAYD_LOG:-$PROJECT_DIR/gatewayd.log}
POLL_SECONDS=0.5
START_TIMEOUT=60
STOP_TIMEOUT=20

# Asked of the daemon rather than worked out here. Where the discovery file
# goes depends on a .JingClaw directory, a setting and the platform, and a
# script that reimplements that resolution is one that will disagree with the
# daemon the first time any of it changes — and then stop and start nothing.
if [ -n "${JINGCLAW_RUNTIME_DIR:-}" ]; then
	DISCOVERY_FILE="$JINGCLAW_RUNTIME_DIR/daemon.json"
elif [ -x "$AGENTD_BIN" ] &&
	DISCOVERY_FILE=$(cd "$PROJECT_DIR" && "$AGENTD_BIN" --print-paths 2>/dev/null |
		awk '$1 == "discovery" { $1 = ""; sub(/^ +/, ""); print }') &&
	[ -n "$DISCOVERY_FILE" ]; then
	:
else
	DISCOVERY_FILE="$HOME/Library/Application Support/JingClaw/run/daemon.json"
fi

read_pid() {
	if [ ! -f "$DISCOVERY_FILE" ]; then
		return 0
	fi
	sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$DISCOVERY_FILE" | head -n 1
}

process_is_agentd() {
	process_pid=$1
	process_command=$(ps -p "$process_pid" -o command= 2>/dev/null || true)
	case "$process_command" in
		*agentd*) return 0 ;;
		*) return 1 ;;
	esac
}

process_is_running() {
	kill -0 "$1" 2>/dev/null
}

stop_new_process() {
	if process_is_running "$NEW_PID"; then
		kill -TERM "$NEW_PID" 2>/dev/null || true
	fi
}

gatewayd_pids() {
	ps -ax -o pid= -o command= | awk -v binary="$GATEWAYD_BIN" \
		'$2 == binary || $2 == "./bin/gatewayd" { print $1 }'
}

stop_gatewayd() {
	for gatewayd_pid in $(gatewayd_pids); do
		printf 'Stopping previous gatewayd (PID %s)\n' "$gatewayd_pid"
		kill -TERM "$gatewayd_pid" 2>/dev/null || true
	done

	attempt=0
	while [ "$attempt" -lt "$STOP_TIMEOUT" ] && [ -n "$(gatewayd_pids)" ]; do
		attempt=$((attempt + 1))
		sleep "$POLL_SECONDS"
	done
	if [ -n "$(gatewayd_pids)" ]; then
		printf 'Previous gatewayd did not stop after SIGTERM\n' >&2
		exit 1
	fi
}

OLD_PID=$(read_pid)
if [ -n "$OLD_PID" ] && ! process_is_running "$OLD_PID"; then
	OLD_PID=
fi

if [ ! -x "$AGENTD_BIN" ]; then
	printf 'agentd executable not found: %s\n' "$AGENTD_BIN" >&2
	exit 1
fi
if [ ! -x "$GATEWAYD_BIN" ]; then
	printf 'gatewayd executable not found: %s\n' "$GATEWAYD_BIN" >&2
	exit 1
fi

printf 'Building agentd and gatewayd from current source\n'
(
	cd "$CORE_DIR"
	"$GO_BIN" build -o "$AGENTD_BIN" ./cmd/agentd
	"$GO_BIN" build -o "$GATEWAYD_BIN" ./cmd/gatewayd
)

printf 'Starting new agentd: %s\n' "$AGENTD_BIN"
"$AGENTD_BIN" "$@" >>"$AGENTD_LOG" 2>&1 &
NEW_PID=$!
trap stop_new_process INT TERM EXIT

attempt=0
while [ "$attempt" -lt "$START_TIMEOUT" ]; do
	if ! process_is_running "$NEW_PID"; then
		printf 'New agentd exited before becoming ready; see %s\n' "$AGENTD_LOG" >&2
		exit 1
	fi
	DISCOVERY_PID=$(read_pid)
	if [ "$DISCOVERY_PID" = "$NEW_PID" ]; then
		break
	fi
	attempt=$((attempt + 1))
	sleep "$POLL_SECONDS"
done

if [ "$(read_pid)" != "$NEW_PID" ]; then
	printf 'New agentd did not publish discovery information; see %s\n' "$AGENTD_LOG" >&2
	exit 1
fi

trap - EXIT
if [ -n "$OLD_PID" ] && [ "$OLD_PID" != "$NEW_PID" ]; then
	if ! process_is_agentd "$OLD_PID"; then
		printf 'Refusing to stop PID %s: it is not agentd\n' "$OLD_PID" >&2
		stop_new_process
		exit 1
	fi

	printf 'Stopping previous agentd (PID %s)\n' "$OLD_PID"
	kill -TERM "$OLD_PID"
	attempt=0
	while process_is_running "$OLD_PID" && [ "$attempt" -lt "$STOP_TIMEOUT" ]; do
		attempt=$((attempt + 1))
		sleep "$POLL_SECONDS"
	done
	if process_is_running "$OLD_PID"; then
		printf 'Previous agentd did not stop after SIGTERM\n' >&2
		exit 1
	fi
fi

stop_gatewayd

printf 'Starting new gatewayd: %s\n' "$GATEWAYD_BIN"
"$GATEWAYD_BIN" "$@" >>"$GATEWAYD_LOG" 2>&1 &
GATEWAYD_PID=$!
sleep "$POLL_SECONDS"
if ! process_is_running "$GATEWAYD_PID"; then
	printf 'New gatewayd exited during startup; see %s\n' "$GATEWAYD_LOG" >&2
	exit 1
fi

printf 'agentd and gatewayd are ready (agentd PID %s, gatewayd PID %s)\n' "$NEW_PID" "$GATEWAYD_PID"