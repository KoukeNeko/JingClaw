#!/bin/sh
# Proves the settings reach the running daemon, rather than merely parsing:
# that a first start saves them in the database and writes no file, that wrong
# ones are explained and leave a daemon that can be reached to put them right
# instead of one that is gone, that an address nobody should write is refused,
# and that the two places the settings cannot say are given to the daemon and
# not read out of them.
set -eu

# A deployment of this check's own for each step, so it cannot reach the
# operator's: reading their settings would be bad and writing to their database
# would be worse. Nothing inherited may name one, or supply a document, or
# override a setting: this check is about what the daemon does with what it is
# given, and a variable from the caller is something else it was given.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
CLI="$WORK/jingclaw"
go build -o "$CLI" ./cmd/jingclaw

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

# start NAME RUNDIR runs a daemon for the deployment the environment names,
# with its output in NAME.out and NAME.err, and returns once it has published
# itself in RUNDIR.
start() {
	"$CLI" daemon >"$WORK/$1.out" 2>"$WORK/$1.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$2/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "no discovery file in $2: $(cat "$WORK/$1.err")"
		sleep 0.1
	done
	DISCOVERY="$2/daemon.json"
	BASE=$(discovered base_url)
	TOKEN=$(discovered token)
}
stop() {
	kill "$DAEMON"
	wait "$DAEMON" 2>/dev/null || true
	DAEMON=""
}

# refused runs a daemon and succeeds when it stops with an error without having
# listened. Bounded, because a daemon that did not refuse is one that is
# serving, and waiting for it to end would wait forever.
refused() {
	"$CLI" daemon >"$WORK/refusal" 2>&1 &
	DAEMON=$!
	WAITED=0
	until grep -q '^error:' "$WORK/refusal" || grep -q 'daemon listening' "$WORK/refusal"; do
		WAITED=$((WAITED + 1))
		if [ "$WAITED" -gt 100 ]; then
			stop
			fail "the daemon neither refused nor started: $(cat "$WORK/refusal")"
		fi
		sleep 0.1
	done
	if grep -q 'daemon listening' "$WORK/refusal"; then
		stop
		return 1
	fi
	STATUS=0
	wait "$DAEMON" || STATUS=$?
	DAEMON=""
	[ "$STATUS" -ne 0 ]
}

discovered() {
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$DISCOVERY" "$1"
}

# call METHOD JSON [CREDENTIAL] posts a Connect request as JSON and prints the
# body, and status prints only the code.
call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer ${3:-$TOKEN}" -d "$2" \
		"$BASE/jingclaw.control.v1.$1"
}
status() {
	curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer ${3:-$TOKEN}" -d "$2" "$BASE/jingclaw.control.v1.$1"
}
field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }

# body KEY VALUE... is a JSON object, so that a text with quotes and line breaks
# in it reaches the API as the text it is.
body() { python3 -c 'import json,sys;print(json.dumps(dict(zip(sys.argv[1::2], sys.argv[2::2]))))' "$@"; }

# versions DOCUMENT is what has been saved of a document, newest first, as
# number:source pairs. Nothing at all for a document with no row.
versions() {
	call ConfigService/ListDocumentVersions "{\"document\":\"SETTINGS_DOCUMENT_$1\"}" |
		field '" ".join(v["version"] + ":" + v["source"] for v in d.get("versions", []))'
}

# 1. Nothing configured yet: the daemon saves the example where the settings
#    are kept, and puts no file anywhere.
#
# Nothing may name the deployment here, including anything inherited from a
# caller: this check is about the default, and "none" would say there is no
# deployment at all and so nowhere to keep them.
FRESH="$WORK/fresh-home"
mkdir -p "$FRESH"
REAL_HOME=$HOME
export HOME="$FRESH"
start fresh "$FRESH/.jingclaw/run"

[ -z "$(find "$FRESH" \( -name config.toml -o -name AGENTS.md -o -name PERSONA.md \))" ] ||
	fail "a settings file was created: $(find "$FRESH" -name '*.md' -o -name config.toml)"
grep -q 'created, all defaults' "$WORK/fresh.out" || fail "the daemon does not say it made the settings from the defaults: $(cat "$WORK/fresh.out")"
printf 'ok   saves the settings when there are none, and writes no file\n'

# The header rather than any one setting. Which settings are shown
# uncommented is a presentation decision that changes; this is what identifies
# the text.
"$CLI" daemon --print-config > "$WORK/example"
call ConfigService/GetConfig '{}' | python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["text"])' > "$WORK/stored"
grep -q '^# JingClaw configuration' "$WORK/stored" || fail "what was saved is not the example"
grep -q '^\[gateway\]' "$WORK/stored" || fail "what was saved is missing sections"
cmp -s "$WORK/example" "$WORK/stored" || fail "the saved text is not the example:
$(diff "$WORK/example" "$WORK/stored" | head -20)"
[ "$(versions CONFIG)" = "1:default" ] || fail "the example is not one version of its own: $(versions CONFIG)"
# Who the agent is, and how it works, have no default: an absent document is an
# empty one, and a stub in its place would be text somebody has to delete.
[ -z "$(versions AGENTS)" ] && [ -z "$(versions PERSONA)" ] || fail "a default was saved for the agents or persona documents"
printf 'ok   what it saves is the example, as one version, and the other two documents are left empty\n'

# The settings it shows are at their defaults, so a daemon whose settings
# somebody has read and not edited behaves exactly as one they never opened.
# Without this, the example is a set of choices nobody made.
DIFFER=$(call ConfigService/GetConfig '{}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["file"], "the example sets nothing"
print(" ".join(k for k, v in d["file"].items() if d["running"].get(k) != v))')
[ -z "$DIFFER" ] || fail "saved settings are running as something else: $DIFFER"
printf 'ok   what it saves live is what it would have defaulted to anyway\n'
stop
export HOME=$REAL_HOME

# The rest name their own settings, so they need a deployment to belong to
# rather than the operator's.
export JINGCLAW_HOME="$WORK/wrong"

# 2. A wrong setting is explained, not stack-traced, and does not take the
#    daemon with it.
WRONG='[server]
addr = "0.0.0.0:0"
log_level = "verbose"

[provider]
backend = "openai"
'

# A variable that carries them is refused, and saves nothing. A saved text is
# never replaced by the variable, so one saved from it would outlast the
# variable being put right, and a container has no dashboard to put it right in.
export JINGCLAW_HOME="$WORK/variable"
export JINGCLAW_CONFIG="$WRONG"
refused || fail "a daemon was started on settings from a variable that no start could use: $(cat "$WORK/refusal")"
for EXPECTED in 'server.addr = "0.0.0.0:0"' loopback \
	'server.log_level = "verbose"' 'provider.backend = "openai"' JINGCLAW_CONFIG; do
	grep -q -- "$EXPECTED" "$WORK/refusal" || fail "the refusal never mentions $EXPECTED: $(cat "$WORK/refusal")"
done
export JINGCLAW_CONFIG='[provider]
backend = "fake"
'
start variable "$JINGCLAW_HOME/run"
[ "$(versions CONFIG)" = "1:env" ] || fail "correcting the variable did not make it the first version: $(versions CONFIG)"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] ||
	fail "the corrected variable did not start an agent: $(call ConfigService/GetAgentState '{}')"
stop
unset JINGCLAW_CONFIG
printf 'ok   a variable the daemon could not run is refused and saves nothing, so correcting it is enough\n'

# An older version's file is imported as it is, with no check, because the
# person who has it can put it right where the daemon can be reached.
export JINGCLAW_HOME="$WORK/wrong"
mkdir -p "$JINGCLAW_HOME"
printf '%s' "$WRONG" > "$JINGCLAW_HOME/config.toml"

# Asking a question of settings that would not start is answered with the
# reasons and an exit, as it always was: there is no daemon to keep.
if "$CLI" daemon --print-prompt >"$WORK/out" 2>&1; then
	fail "the prompt was printed from three broken settings"
fi
for EXPECTED in 'server.addr = "0.0.0.0:0"' loopback \
	'server.log_level = "verbose"' 'provider.backend = "openai"' --print-config; do
	grep -q -- "$EXPECTED" "$WORK/out" || fail "the report never mentions $EXPECTED: $(cat "$WORK/out")"
done
[ -f "$JINGCLAW_HOME/config.toml" ] || fail "a question set the file aside"
printf 'ok   names every wrong setting, not just the first\n'

# The text is imported now, as it is: what follows is a daemon started on
# settings that are in the database and will not do.
start wrong "$JINGCLAW_HOME/run"
[ ! -f "$JINGCLAW_HOME/config.toml" ] && [ -f "$JINGCLAW_HOME/config.toml.imported" ] ||
	fail "the start after the question did not set the imported file aside"

STATE=$(call ConfigService/GetAgentState '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] || fail "stored settings that will not start did not leave a daemon in repair mode: $STATE"
[ "$(printf '%s' "$STATE" | field 'sorted(p["key"] for p in d["problems"])')" = "['provider.backend', 'server.addr', 'server.log_level']" ] ||
	fail "the daemon does not say what is wrong with every one: $STATE"
[ "$(printf '%s' "$STATE" | field 'set(p["source"] for p in d["problems"])')" = "{'settings'}" ] || fail "the problems are not attributed to the settings: $STATE"
for EXPECTED in 'server.addr = "0.0.0.0:0"' loopback 'provider.backend = "openai"' --print-config; do
	grep -q -- "$EXPECTED" "$WORK/wrong.err" || fail "the daemon's report never mentions $EXPECTED: $(cat "$WORK/wrong.err")"
done
[ "$(discovered mode)" = repair ] || fail "the discovery file does not say the daemon is in repair mode: $(cat "$DISCOVERY")"
[ -z "$(discovered gateway_token)" ] || fail "a daemon with nothing to ingest was given a gateway credential"
printf 'ok   stored settings that will not start leave a daemon that says why, instead of none\n'

# It is the same door and fewer rooms behind it, not a second way in.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' -d '{}' \
	"$BASE/jingclaw.control.v1.ConfigService/GetAgentState")
[ "$STATUS" = 401 ] || fail "a repair daemon answered without a credential: $STATUS"
[ "$(status ConfigService/GetAgentState '{}' not-a-credential)" = 401 ] || fail "a repair daemon accepted a made-up credential"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' -X POST \
	-H 'content-type: application/json' -H "authorization: Bearer $TOKEN" -d '{}' \
	"$BASE/jingclaw.control.v1.ConfigService/GetAgentState")
[ "$STATUS" = 403 ] || fail "a rebound host reached a repair daemon: $STATUS"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' "$BASE/ui/")" = 403 ] || fail "a rebound host was given the page by a repair daemon"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/ui/")" = 200 ] || fail "the page does not load from a repair daemon"
curl -s -D - -o /dev/null "$BASE/ui/" | grep -qi '^content-security-policy:' || fail "the page from a repair daemon has no policy"
printf 'ok   and it is the same door: credential, host check and page\n'

# Nothing that runs an agent is there to answer, whoever asks. A refusal would
# say the daemon has sessions; not found says it has none.
for METHOD in SessionService/ListSessions MemoryService/ListMemories ChannelService/ListBindings \
	ArtifactService/StatArtifact GatewayIngressService/GetGatewaySettings; do
	[ "$(status "$METHOD" '{}')" = 404 ] || fail "a repair daemon answers $METHOD: $(status "$METHOD" '{}')"
done
printf 'ok   and nothing that runs an agent is behind it, even for the operator'"'"'s credential\n'

# A browser gets in the way it always does, and sees why there is no agent.
LINK=$(env JINGCLAW_NO_BROWSER=1 "$CLI" dashboard --print 2>"$WORK/said")
grep -q 'server.addr' "$WORK/said" || fail "the dashboard command does not say why there is no agent: $(cat "$WORK/said")"
CODE=${LINK#*#pair=}
SESSION=$(curl -s -X POST -d "{\"code\":\"$CODE\"}" "$BASE/ui/pair" | field 'd["token"]')
[ -n "$SESSION" ] || fail "a repair daemon would not pair a browser: $LINK"
[ "$(status ConfigService/GetAgentState '{}' "$SESSION")" = 200 ] || fail "a paired browser cannot ask a repair daemon what is wrong"
[ "$(status SessionService/ListSessions '{}' "$SESSION")" = 404 ] || fail "a paired browser reached sessions on a repair daemon"
printf 'ok   a browser pairs with it, and can ask what is wrong\n'

# 3. The settings are put right where the daemon can be reached, and the next
#    start runs.
GOT=$(call ConfigService/GetConfig '{}')
HASH=$(printf '%s' "$GOT" | field 'd["hash"]')
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = "$(printf '%s' "$WRONG")" ] || fail "the daemon does not hand back the text it cannot start on: $GOT"

for BAD in '[provider]
backend = "nope"
' '[provider
backend' '[server]
data_dir = "/elsewhere"

[provider]
backend = "fake"
'; do
	REFUSED=$(call ConfigService/ReplaceConfigText "$(body text "$BAD" hash "$HASH")")
	[ "$(printf '%s' "$REFUSED" | field 'd.get("written", False)')" = False ] || fail "text that would not start was saved: $REFUSED"
	[ "$(printf '%s' "$REFUSED" | field 'len(d["problems"]) > 0')" = True ] || fail "text was refused without a reason: $REFUSED"
done
[ "$(versions CONFIG)" = "1:file" ] || fail "text that was refused left a version: $(versions CONFIG)"
printf 'ok   text that would not start is refused with its reasons, and never saved\n'

# The text that names where the database is was one of those refusals, and the
# same goes for an edit: the settings are kept in the database, so they cannot
# be where it is.
for KEY in server.data_dir server.runtime_dir; do
	CHECK=$(call ConfigService/CheckConfig "{\"edits\":[{\"key\":\"$KEY\",\"value\":\"/elsewhere\"}]}")
	[ "$(printf '%s' "$CHECK" | field 'd["problems"][0]["key"]')" = "$KEY" ] || fail "an edit to $KEY was not refused: $CHECK"
	printf '%s' "$CHECK" | grep -q 'JINGCLAW_SERVER_' || fail "the refusal does not say how $KEY is given: $CHECK"
done
printf 'ok   and so is an edit to a place the settings cannot say\n'

FIXED='# Put right in repair mode.
[agent]
name = "設定檔測試"
max_iterations = 3

[provider]
backend = "fake"
fake_delay = "1ms"
'
SAVED=$(call ConfigService/ReplaceConfigText "$(body text "$FIXED" hash "$HASH")")
[ "$(printf '%s' "$SAVED" | field 'd["written"]')" = True ] || fail "text that would start was refused: $SAVED"
[ "$(printf '%s' "$SAVED" | field 'd["pendingRestart"]')" = True ] || fail "a repair does not say it waits for a restart: $SAVED"
[ "$(printf '%s' "$SAVED" | field 'd["hash"]')" = "$(printf '%s' "$FIXED" | python3 -c 'import hashlib,sys;print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')" ] ||
	fail "the saved text is not the text that was sent: $SAVED"
[ "$(versions CONFIG)" = "2:web 1:file" ] || fail "the repair is not a second version: $(versions CONFIG)"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = repair ] || fail "a repair took effect without a restart"
printf 'ok   text that would start is saved, whole, and waits for a restart\n'

# A variable that carries the broken text arrives with every start, and must not
# put it back over the repair.
export JINGCLAW_CONFIG="$WRONG"
stop
start fixed "$JINGCLAW_HOME/run"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] || fail "the daemon did not run after the settings were put right: $(call ConfigService/GetAgentState '{}')"
GOT=$(call ConfigService/GetConfig '{}')
[ "$(printf '%s' "$GOT" | field 'int(d["running"]["agent.max_iterations"])')" = 3 ] || fail "the repaired settings are not what is running: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = "$(printf '%s' "$FIXED")" ] || fail "the text did not come back as it was sent: $GOT"
[ "$(versions CONFIG)" = "2:web 1:file" ] || fail "a start changed the saved settings: $(versions CONFIG)"
[ "$(discovered mode)" = "" ] || fail "a daemon with an agent says it is in repair mode"
[ -n "$(discovered gateway_token)" ] || fail "a daemon with an agent has no gateway credential"
SESSION=$("$CLI" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session id"
"$CLI" send "$SESSION" "hello" >/dev/null
printf 'ok   the next start runs the repaired settings, serves a run, and is not undone by the variable\n'
stop
unset JINGCLAW_CONFIG

# 4. The two places the settings cannot say are given by the environment. A
#    text that names them anyway, as an older deployment's does, is kept as it
#    was written and not obeyed.
export JINGCLAW_HOME="$WORK/deployment"
export JINGCLAW_SERVER_RUNTIME_DIR="$WORK/run-by-environment"
export JINGCLAW_SERVER_DATA_DIR="$WORK/data-by-environment"
export JINGCLAW_CONFIG="[agent]
name = \"設定檔測試\"
max_iterations = 3

[provider]
backend = \"fake\"
fake_delay = \"1ms\"

[server]
runtime_dir = \"$WORK/run-by-text\"
data_dir = \"$WORK/data-by-text\"
"
start placed "$JINGCLAW_SERVER_RUNTIME_DIR"

[ -f "$WORK/data-by-environment/jingclaw.db" ] || fail "the database is not where the environment put it"
for NOT in run-by-text data-by-text; do
	[ -e "$WORK/$NOT" ] && fail "the settings moved the daemon to $NOT"
done
grep -q 'run-by-text' "$WORK/placed.err" && fail "the daemon announced the place the settings named"
call ConfigService/GetConfig '{}' | field 'd["text"]' | grep -q "run-by-text" || fail "the text was not kept as it was written"
printf 'ok   publishes discovery and keeps its database where the environment says, whatever the text says\n'

# The client finds it the same way, and so does nobody who was not told.
SESSION=$("$CLI" session create | tr -d '\r\n')
[ -n "$SESSION" ] || fail "no session id"
"$CLI" send "$SESSION" "hello" >/dev/null
if env -u JINGCLAW_SERVER_RUNTIME_DIR "$CLI" session create >/dev/null 2>&1; then
	fail "a client that was not told where to look found the daemon"
fi
printf 'ok   serves a run through that location, to a client that was told it\n'

# And so does what finds a daemon without asking it anything: it is told in the
# one variable, so a status that says "not running" about a daemon that is, or a
# stop that leaves it running, is a place the two disagree.
"$CLI" status | grep -q '^running' || fail "status does not find a daemon that was told where to publish itself: $("$CLI" status 2>&1)"
printf 'ok   and so does the command that reports on it\n'

# An address nobody should write is refused wherever it is typed. So is an edit
# to a place the environment has given, with the variable that gave it.
CHECK=$(call ConfigService/CheckConfig '{"edits":[{"key":"server.addr","value":"0.0.0.0:9977"}]}')
[ "$(printf '%s' "$CHECK" | field 'd["problems"][0]["key"]')" = server.addr ] || fail "the check missed an address anybody can reach: $CHECK"
HASH=$(call ConfigService/GetConfig '{}' | field 'd["hash"]')
WROTE=$(call ConfigService/WriteConfig "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"server.addr\",\"value\":\"0.0.0.0:9977\"}]}")
[ "$(printf '%s' "$WROTE" | field 'd.get("written", False)')" = False ] || fail "an address anybody can reach was saved: $WROTE"
for KEY in server.data_dir:JINGCLAW_SERVER_DATA_DIR server.runtime_dir:JINGCLAW_SERVER_RUNTIME_DIR; do
	REFUSED=$(call ConfigService/WriteConfig "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"${KEY%%:*}\",\"value\":\"/elsewhere\"}]}")
	case "$REFUSED" in
		*"${KEY%%:*} is set by ${KEY#*:}"*) ;;
		*) fail "an edit to ${KEY%%:*} was not refused by the variable that gave it: $REFUSED" ;;
	esac
done
[ "$(call ConfigService/GetConfig '{}' | field 'd["hash"]')" = "$HASH" ] || fail "a refused edit changed the settings"
[ "$(versions CONFIG)" = "1:env" ] || fail "a refused edit left a version"
printf 'ok   an address nobody should write is refused, and so is an edit to a place that was given, with the text untouched\n'

printf '\nall checks passed\n'
