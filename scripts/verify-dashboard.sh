#!/bin/sh
# Proves a browser can be let in, and can change the settings, without ever
# being handed the credential programs on this machine use.
#
# The seam this checks is the one a unit test cannot: a real daemon, a link a
# person is given, a page fetched with the headers a browser would see, and a
# settings file edited through the API and read back off the disk.
#
#   the link carries a code, never the operator's credential
#   the code works once, and a page elsewhere cannot reach the pairing
#   a paired browser reaches the control surface, and a gateway still cannot
#   a write changes the lines it was for, refuses a stale or invalid edit,
#     and keeps what the operator wrote around it
#   a browser's access ends when the daemon stops
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's.
export JINGCLAW_HOME="$WORK"
export JINGCLAW_NO_BROWSER=1
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort: see verify-api.sh for why this must not stop at the first
	# failure.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
cat > "$WORK/config.toml" <<EOF
# Written by hand, and it should still be here afterwards.
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[server]
addr = "127.0.0.1:7852"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF
printf '# who it is\n' > "$WORK/PERSONA.md"
ORIGINAL=$(cat "$WORK/config.toml")

start() {
	"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
		sleep 0.1
	done
}
start

BASE="http://127.0.0.1:7852"
LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["gateway_token"])' "$WORK/run/daemon.json")

jingclaw() { "$WORK/jingclaw" --config "$WORK/config.toml" "$@"; }

# call POSTs a Connect request as JSON and prints the body. $1 is the method,
# $2 the credential, $3 the JSON.
call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $2" -d "$3" \
		"$BASE/jingclaw.control.v1.$1"
}
status() {
	curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $2" -d "$3" "$BASE/jingclaw.control.v1.$1"
}
field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }

# 1. The link carries a code, and not the credential the CLI itself holds.
LINK=$(jingclaw dashboard --print)
case "$LINK" in
	"$BASE/ui/#pair="*) ;;
	*) fail "the link is not a dashboard link: $LINK" ;;
esac
case "$LINK" in
	*"$LOCAL"*|*"$GATEWAY"*) fail "the link contains a credential: $LINK" ;;
esac
CODE=${LINK#*#pair=}
printf 'ok   the link holds a one-time code and no credential\n'

# 2. Without a browser to open, the command still says where to go.
jingclaw dashboard 2>"$WORK/said" >/dev/null || fail "the dashboard command failed with no browser"
grep -q '/ui/#pair=' "$WORK/said" || fail "it did not say where to go: $(cat "$WORK/said")"
printf 'ok   with no browser it says where to go\n'

# 3. The page is served, with a policy, and only to a name that is this machine.
HEADERS=$(curl -s -D - -o "$WORK/page" "$BASE/ui/")
printf '%s' "$HEADERS" | grep -qi '^content-security-policy:.*frame-ancestors' || fail "the page has no policy: $HEADERS"
printf '%s' "$HEADERS" | grep -qi '^x-content-type-options: nosniff' || fail "the page is sniffable"
grep -qi '<html' "$WORK/page" || fail "the page is not a page"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com:7852' "$BASE/ui/")
[ "$STATUS" = 403 ] || fail "a rebound host was given the page: $STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Host: evil.example.com:7852' \
	-d "{\"code\":\"$CODE\"}" "$BASE/ui/pair")
[ "$STATUS" = 403 ] || fail "a rebound host reached the pairing: $STATUS"
printf 'ok   the page carries its policy, and a rebound host reaches neither it nor the pairing\n'

# 4. The code is traded for a session, once.
SESSION=$(curl -s -X POST -d "{\"code\":\"$CODE\"}" "$BASE/ui/pair" | field 'd["token"]')
[ -n "$SESSION" ] || fail "the code was not accepted"
[ "$SESSION" != "$LOCAL" ] || fail "the browser was given the operator's credential"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d "{\"code\":\"$CODE\"}" "$BASE/ui/pair")
[ "$STATUS" = 401 ] || fail "a spent code was accepted again: $STATUS"
printf 'ok   a code is traded for a session of its own, once\n'

# 5. The session reaches the control surface, and a gateway's credential
#    still does not reach the settings.
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 200 ] || fail "a session cannot list sessions"
[ "$(status ConfigService/GetConfig "$SESSION" '{}')" = 200 ] || fail "a session cannot read the settings"
[ "$(status ConfigService/GetConfig "$GATEWAY" '{}')" = 403 ] || fail "the gateway credential reached the settings"
[ "$(status ConfigService/GetConfig not-a-credential '{}')" = 401 ] || fail "a made-up credential was accepted"
printf 'ok   a session reaches the control surface; the gateway credential does not reach the settings\n'

# 6. What the file says, and what is running.
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
HASH=$(printf '%s' "$GOT" | field 'd["hash"]')
[ -n "$HASH" ] || fail "no hash: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["file"]["provider.backend"]')" = fake ] || fail "the file's setting is not shown: $GOT"
[ "$(printf '%s' "$GOT" | field 'int(d["running"]["agent.max_iterations"])')" = 12 ] || fail "the running value is not shown: $GOT"
printf 'ok   it shows what the file says next to what is running\n'

# 7. An edit is checked without being written, and refused when it would not start.
CHECK=$(call ConfigService/CheckConfig "$SESSION" '{"edits":[{"key":"context.compact_at","value":1.5}]}')
[ "$(printf '%s' "$CHECK" | field 'd["problems"][0]["key"]')" = context.compact_at ] || fail "the check missed it: $CHECK"
[ "$(cat "$WORK/config.toml")" = "$ORIGINAL" ] || fail "a check changed the file"
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"context.compact_at\",\"value\":1.5}]}")
[ "$(printf '%s' "$WROTE" | field 'd.get("written", False)')" = False ] || fail "an invalid edit was written: $WROTE"
[ "$(cat "$WORK/config.toml")" = "$ORIGINAL" ] || fail "a refused write changed the file"
printf 'ok   an edit that would not start is checked, and refused, with the file untouched\n'

# 8. A real write changes what it was for and leaves the rest as the operator
#    wrote it, comment included.
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"agent.max_iterations\",\"value\":7}]}")
[ "$(printf '%s' "$WROTE" | field 'd["written"]')" = True ] || fail "the write was refused: $WROTE"
grep -q '^max_iterations = 7$' "$WORK/config.toml" || fail "the setting is not in the file: $(cat "$WORK/config.toml")"
case "$(cat "$WORK/config.toml")" in
	"$ORIGINAL"*) ;;
	*) fail "what the operator wrote was disturbed: $(cat "$WORK/config.toml")" ;;
esac
[ -f "$WORK/config.toml.bak.1" ] || fail "the file it replaced was not kept"
[ "$(cat "$WORK/config.toml.bak.1")" = "$ORIGINAL" ] || fail "the backup is not the file it replaced"
printf 'ok   a write changes what it was for, keeps what was written around it, and keeps the old file\n'

# 9. An edit that began from a version of the file that is gone is refused.
STATUS=$(status ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"agent.max_iterations\",\"value\":9}]}")
[ "$STATUS" = 400 ] || fail "a stale write got $STATUS, want 400 (failed precondition)"
grep -q '^max_iterations = 7$' "$WORK/config.toml" || fail "a stale write changed the file"
printf 'ok   a write that started from an older file is refused\n'

# 10. The instruction files are read and written with the same care.
GOT=$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_PERSONA"}')
# $(...) drops the trailing newline, so the text is compared without it.
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = '# who it is' ] || fail "the persona was not read: $GOT"
PH=$(printf '%s' "$GOT" | field 'd["hash"]')
call ConfigService/PutInstructionFile "$SESSION" "{\"file\":\"INSTRUCTION_FILE_PERSONA\",\"text\":\"Be brief.\\n\",\"hash\":\"$PH\"}" >/dev/null
[ "$(cat "$WORK/PERSONA.md")" = "Be brief." ] || fail "the persona was not written: $(cat "$WORK/PERSONA.md")"
printf 'ok   the persona can be read and written\n'

# 11. A browser's access ends when the daemon stops. Nothing of it is kept.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
rm -f "$WORK/run/daemon.json"
start
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 401 ] || fail "a session outlived the daemon"
printf 'ok   a session does not outlive the daemon\n'

printf '\nall dashboard checks passed\n'
