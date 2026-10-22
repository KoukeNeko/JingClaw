#!/bin/sh
# Proves the dashboard's password (initial, setup session, change, reset) and its restart, with the real binary.
#
# The unit tests cover the hash, the throttle and the argument list a restart
# runs. What they cannot see is the process: that a restart asked for over the
# API shuts the whole thing down, comes back as the same pid on the same port
# (so a forwarded dashboard and a supervisor both still have it), that a
# browser's session survives it, and that the password never reaches a file,
# a log or a reply. Run for the bare command, and for the daemon alone.
set -eu

# Nothing inherited may supply a document or override a setting.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done
unset GEMINI_API_KEY GOOGLE_API_KEY ANTHROPIC_API_KEY BRAVE_SEARCH_TOKEN DISCORD_BOT_TOKEN TELEGRAM_BOT_TOKEN

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
CLI="$WORK/jingclaw"
go build -o "$CLI" ./cmd/jingclaw

PORT=7880
BASE="http://127.0.0.1:$PORT"
PASSWORD="e2e-password-canary-Qw8Rt3Yu"
CHANGED="e2e-changed-canary-Lk5Jh2Gf"
INITIAL="jingclaw-initial"

export JINGCLAW_HOME="$WORK/home"
mkdir -p "$JINGCLAW_HOME"
chmod 700 "$JINGCLAW_HOME"
export JINGCLAW_NO_BROWSER=1
export JINGCLAW_CONFIG="[provider]
backend = \"fake\"
fake_model = \"fake-echo\"
fake_delay = \"0s\"

[server]
addr = \"127.0.0.1:$PORT\"
"

RUN=""
cleanup() {
	set +e
	[ -n "$RUN" ] && kill "$RUN" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }
discovered() {
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$JINGCLAW_HOME/run/daemon.json" "$1"
}
mode() { python3 -c 'import os,sys;print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$1"; }

# call METHOD BODY TOKEN prints the reply; status METHOD BODY TOKEN the code.
call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $3" -d "$2" \
		"$BASE/jingclaw.control.v1.$1"
}
status() {
	curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $3" -d "$2" "$BASE/jingclaw.control.v1.$1"
}
# login PASSWORD prints the reply to the page's own login.
login() {
	curl -s -X POST -H 'content-type: application/json' -d "{\"password\":\"$1\"}" "$BASE/ui/login"
}
logincode() {
	curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
		-d "{\"password\":\"$1\"}" "$BASE/ui/login"
}

# up waits for a daemon that published a credential other than $1 and answers.
up() {
	WAITED=0
	while :; do
		NOW=$(discovered token 2>/dev/null || true)
		if [ -n "$NOW" ] && [ "$NOW" != "$1" ] &&
			curl -s -o /dev/null "$BASE/ui/auth" 2>/dev/null; then
			return 0
		fi
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 300 ] && fail "nothing came up on $BASE: $(cat "$WORK/err" 2>/dev/null)"
		sleep 0.1
	done
}

# nowhere says no password is anywhere this check collected, or on the disk
# of the deployment.
nowhere() {
	for WHERE in "$WORK/out" "$WORK/err" "$WORK/replies" "$JINGCLAW_HOME"/*.json "$JINGCLAW_HOME"/data/* "$JINGCLAW_HOME"/log/*; do
		[ -f "$WHERE" ] || continue
		for VALUE in "$PASSWORD" "$CHANGED" "$INITIAL"; do
			if grep -qF -- "$VALUE" "$WHERE"; then
				fail "a password is in $WHERE"
			fi
		done
	done
}

# exercise runs the whole sequence against whatever is on $PORT, started as $RUN.
exercise() {
	LABEL=$1
	: >"$WORK/replies"

	up ""
	PID=$RUN
	TOKEN=$(discovered token)
	[ "$(discovered pid)" = "$PID" ] || fail "$LABEL: the discovery file names pid $(discovered pid), not $PID"

	[ "$(curl -s "$BASE/ui/auth")" = '{"initial":true,"passwordSet":true}' ] || fail "$LABEL: a fresh deployment is not in the initial state: $(curl -s "$BASE/ui/auth")"
	[ "$(logincode "$PASSWORD")" = 401 ] || fail "$LABEL: a password nobody set logged in"
	NONE=$(login "$PASSWORD")
	SETUP_REPLY=$(login "$INITIAL")
	SETUP=$(printf '%s' "$SETUP_REPLY" | field 'd["token"]')
	[ "$(printf '%s' "$SETUP_REPLY" | field 'd["scope"]')" = setup ] || fail "$LABEL: the initial password did not give a setup session: $SETUP_REPLY"
	[ "$(status ConfigService/GetAgentState '{}' "$SETUP")" = 403 ] || fail "$LABEL: a setup session can read the agent state"
	[ "$(status BrowserService/CreatePairing '{}' "$SETUP")" = 403 ] || fail "$LABEL: a setup session can make a pairing"
	[ ! -e "$JINGCLAW_HOME/dashboard-sessions.json" ] || fail "$LABEL: a setup session was written to disk"
	printf 'ok   %s: the initial password gives a setup session that can call nothing\n' "$LABEL"

	# Paired once, the way the first browser gets in.
	CODE=$(call BrowserService/CreatePairing '{}' "$TOKEN" | field 'd["code"]')
	PAIRED=$(curl -s -X POST -H 'content-type: application/json' -d "{\"code\":\"$CODE\"}" "$BASE/ui/pair")
	FIRST=$(printf '%s' "$PAIRED" | field 'd["token"]')
	[ "$(status ConfigService/GetAgentState '{}' "$FIRST")" = 200 ] || fail "$LABEL: the paired browser cannot call the API"

	# The password, set from that browser.
	[ "$(status BrowserService/SetDashboardPassword '{"newPassword":"short"}' "$FIRST")" = 400 ] || fail "$LABEL: a short password was accepted"
	[ "$(status BrowserService/SetDashboardPassword "{\"newPassword\":\"$PASSWORD\"}" "$FIRST")" = 200 ] || fail "$LABEL: the password could not be set"
	[ "$(status BrowserService/SetDashboardPassword "{\"newPassword\":\"$INITIAL\"}" "$FIRST")" = 400 ] || fail "$LABEL: the initial password was accepted as the new one"
	[ "$(curl -s "$BASE/ui/auth")" = '{"initial":false,"passwordSet":true}' ] || fail "$LABEL: the page is not told the password was changed"
	[ "$(status ConfigService/GetAgentState '{}' "$SETUP")" = 401 ] || fail "$LABEL: the setup session outlived the change"
	[ "$(logincode "$INITIAL")" = 401 ] || fail "$LABEL: the initial password still logs in after a change"
	[ "$(mode "$JINGCLAW_HOME/dashboard-auth.json")" = 600 ] || fail "$LABEL: the password file is mode $(mode "$JINGCLAW_HOME/dashboard-auth.json")"
	[ "$(mode "$JINGCLAW_HOME/dashboard-sessions.json")" = 600 ] || fail "$LABEL: the sessions file is mode $(mode "$JINGCLAW_HOME/dashboard-sessions.json")"
	[ "$(status ConfigService/GetAgentState '{}' "$FIRST")" = 200 ] || fail "$LABEL: setting the password signed out the browser that set it"
	printf 'ok   %s: the password is set from a paired browser, in files only its owner can read\n' "$LABEL"

	# The page's login, as a script would make it.
	WRONG=$(login "not the password at all")
	[ "$(logincode "not the password at all")" = 401 ] || fail "$LABEL: a wrong password was accepted"
	[ "$WRONG" = "$NONE" ] || fail "$LABEL: two wrong passwords are told apart: '$WRONG' and '$NONE'"
	[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' -H 'origin: http://evil.example' -d "{\"password\":\"$PASSWORD\"}" "$BASE/ui/login")" = 403 ] || fail "$LABEL: a cross-origin login was not refused"
	[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: text/plain' -d "{\"password\":\"$PASSWORD\"}" "$BASE/ui/login")" = 415 ] || fail "$LABEL: a login that was not JSON was not refused"
	SECOND_REPLY=$(login "$PASSWORD")
	printf '%s' "$SECOND_REPLY" | field '"expires_at" in d' | grep -q True || fail "$LABEL: the login did not answer like a pairing: $SECOND_REPLY"
	SECOND=$(printf '%s' "$SECOND_REPLY" | field 'd["token"]')
	[ "$(status ConfigService/GetAgentState '{}' "$SECOND")" = 200 ] || fail "$LABEL: the session from a password cannot call the API"
	printf 'ok   %s: the password logs in, a wrong one and a foreign page do not\n' "$LABEL"
	printf '%s\n%s\n' "$PAIRED" "$SECOND_REPLY" >>"$WORK/replies"

	# The restart.
	[ "$(status ConfigService/RestartDaemon '{}' "")" = 401 ] || fail "$LABEL: a restart was asked for without a credential"
	ASKED=$(call ConfigService/RestartDaemon '{}' "$SECOND")
	printf '%s' "$ASKED" | field '"runsInFlight" not in d or d["runsInFlight"] == 0' | grep -q True || fail "$LABEL: an idle daemon says runs are in flight: $ASKED"
	up "$TOKEN"
	kill -0 "$PID" 2>/dev/null || fail "$LABEL: pid $PID is gone; a restart must keep the process"
	[ "$(discovered pid)" = "$PID" ] || fail "$LABEL: the restarted daemon is pid $(discovered pid), not $PID"
	[ "$(discovered base_url)" = "$BASE" ] || fail "$LABEL: the daemon came back on another address"
	grep -q 'restarting' "$WORK/err" || fail "$LABEL: the restart was not said"
	printf 'ok   %s: the restart came back as pid %s on %s\n' "$LABEL" "$PID" "$BASE"

	# What a restart must not lose, and what it must.
	[ "$(status ConfigService/GetAgentState '{}' "$SECOND")" = 200 ] || fail "$LABEL: the session from before the restart is signed out"
	[ "$(status ConfigService/GetAgentState '{}' "$FIRST")" = 200 ] || fail "$LABEL: the paired session from before the restart is signed out"
	[ "$(curl -s "$BASE/ui/auth")" = '{"initial":false,"passwordSet":true}' ] || fail "$LABEL: the password did not survive the restart"
	THIRD=$(login "$PASSWORD" | field 'd["token"]')
	[ "$(status ConfigService/GetAgentState '{}' "$THIRD")" = 200 ] || fail "$LABEL: the password does not log in after the restart"
	[ "$(status ConfigService/GetAgentState '{}' "$TOKEN")" = 401 ] || fail "$LABEL: the old operator credential still works; the new process made its own"
	printf 'ok   %s: the sessions and the password survived the restart\n' "$LABEL"

	# Changing it signs the others out; clearing it too.
	[ "$(status BrowserService/SetDashboardPassword "{\"newPassword\":\"$CHANGED\"}" "$THIRD")" = 403 ] || fail "$LABEL: the password was changed without the current one"
	[ "$(status BrowserService/SetDashboardPassword "{\"newPassword\":\"$CHANGED\",\"currentPassword\":\"$PASSWORD\"}" "$THIRD")" = 200 ] || fail "$LABEL: the password could not be changed"
	[ "$(status ConfigService/GetAgentState '{}' "$SECOND")" = 401 ] || fail "$LABEL: another browser is still in after the password changed"
	[ "$(status ConfigService/GetAgentState '{}' "$THIRD")" = 200 ] || fail "$LABEL: the browser that changed it was signed out"
	[ "$(logincode "$PASSWORD")" = 401 ] || fail "$LABEL: the old password still works"
	FOURTH=$(login "$CHANGED" | field 'd["token"]')
	[ "$(status BrowserService/ClearDashboardPassword "{\"currentPassword\":\"$CHANGED\"}" "$THIRD")" = 200 ] || fail "$LABEL: the password could not be reset"
	[ "$(status ConfigService/GetAgentState '{}' "$FOURTH")" = 401 ] || fail "$LABEL: another browser is still in after the reset"
	[ "$(status ConfigService/GetAgentState '{}' "$THIRD")" = 401 ] || fail "$LABEL: the browser that reset it is still in; a reset signs out everybody"
	[ "$(curl -s "$BASE/ui/auth")" = '{"initial":true,"passwordSet":true}' ] || fail "$LABEL: the password is not initial after a reset"
	[ ! -e "$JINGCLAW_HOME/dashboard-auth.json" ] || fail "$LABEL: the password file is still there"
	[ "$(logincode "$CHANGED")" = 401 ] || fail "$LABEL: a reset password still logs in"
	printf 'ok   %s: changing the password signs the others out, and a reset signs out everybody\n' "$LABEL"

	# From a setup session the change returns a control session to carry on with.
	SETUP_AGAIN=$(login "$INITIAL" | field 'd["token"]')
	DONE=$(call BrowserService/SetDashboardPassword "{\"newPassword\":\"$CHANGED\"}" "$SETUP_AGAIN")
	CONTINUED=$(printf '%s' "$DONE" | field 'd["token"]')
	[ "$(status ConfigService/GetAgentState '{}' "$CONTINUED")" = 200 ] || fail "$LABEL: the session a setup session is answered with cannot call the API: $DONE"
	[ "$(status ConfigService/GetAgentState '{}' "$SETUP_AGAIN")" = 401 ] || fail "$LABEL: the setup session is still in after the change"
	[ "$(logincode "$CHANGED")" = 200 ] || fail "$LABEL: the password set from a setup session does not log in"
	printf '%s\n' "$DONE" >>"$WORK/replies"
	printf 'ok   %s: a change from a setup session continues as a control session\n' "$LABEL"

	nowhere
	printf 'ok   %s: no password is in a log, a file or a reply\n' "$LABEL"
}

# ── the bare command: the daemon, the gateway and (here) no console ────

"$CLI" </dev/null >"$WORK/out" 2>"$WORK/err" &
RUN=$!
exercise "one process"
kill "$RUN"
wait "$RUN" 2>/dev/null || true
RUN=""

# ── the daemon by itself ───────────────────────────────────────────────

rm -f "$JINGCLAW_HOME/dashboard-sessions.json" "$JINGCLAW_HOME/dashboard-auth.json"
"$CLI" daemon </dev/null >"$WORK/out" 2>"$WORK/err" &
RUN=$!
exercise "the daemon alone"

printf '\neverything passed\n'
