#!/bin/sh
# Proves a deployment can bring its settings in when a variable is the only way
# in.
#
# Three of the things JingClaw reads are documents — the settings, the persona,
# the standing instructions — and a platform whose inputs are a volume and an
# environment has no way to put them anywhere before the first start. The unit
# tests cover the saving. What they cannot see is whether the daemon calls it,
# whether it calls it before the example is saved (an example landing first
# would make every variable permanently ignored, and quietly), whether a
# document survives the round trip through an environment with its line breaks
# intact, and whether what it saved is what the agent was then built from.
set -eu

# Nothing inherited may supply a document or override a setting: this check is
# about what a variable does when it is the only way in, and a variable from
# the caller is another one.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Under set -e a failing kill would end this
	# function where it stands and leave the work directory behind.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# start NAME runs a daemon for the deployment JINGCLAW_HOME names, with its
# output in NAME.out and NAME.err, and returns once it has published itself.
start() {
	"$WORK/jingclaw" daemon >"$WORK/$1.out" 2>"$WORK/$1.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$JINGCLAW_HOME/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/$1.err")"
		sleep 0.1
	done
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
	"$WORK/jingclaw" daemon >"$WORK/refusal" 2>&1 &
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
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$JINGCLAW_HOME/run/daemon.json" "$1"
}
call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" -d "$2" \
		"$BASE/jingclaw.control.v1.$1"
}
field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }

# saved DOCUMENT prints the text of config, agents or persona exactly as it is
# kept, and same DOCUMENT TEXT says whether it is TEXT to the byte.
saved() {
	case "$1" in
	config) call ConfigService/GetConfig '{}' ;;
	agents) call ConfigService/GetInstructionFile '{"file":"INSTRUCTION_FILE_AGENTS"}' ;;
	persona) call ConfigService/GetInstructionFile '{"file":"INSTRUCTION_FILE_PERSONA"}' ;;
	esac | python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin).get("text", ""))'
}
same() {
	printf '%s' "$2" > "$WORK/expected"
	saved "$1" > "$WORK/actual"
	cmp -s "$WORK/expected" "$WORK/actual"
}

# versions DOCUMENT is what has been saved of a document, newest first, as
# number:source pairs. Nothing at all for a document with no row.
versions() {
	call ConfigService/ListDocumentVersions "{\"document\":\"SETTINGS_DOCUMENT_$1\"}" |
		field '" ".join(v["version"] + ":" + v["source"] for v in d.get("versions", []))'
}

# put DOCUMENT TEXT saves a document through the API, from the version it is at.
put() {
	HASH=$(call ConfigService/GetInstructionFile "{\"file\":\"INSTRUCTION_FILE_$1\"}" | field 'd.get("hash", "")')
	call ConfigService/PutInstructionFile "$(python3 -c 'import json,sys;print(json.dumps({"file": "INSTRUCTION_FILE_" + sys.argv[1], "text": sys.argv[2], "hash": sys.argv[3]}))' "$1" "$2" "$HASH")" >/dev/null
}

no_settings_files() {
	FOUND=$(find "$JINGCLAW_HOME" \( -name config.toml -o -name AGENTS.md -o -name PERSONA.md \))
	[ -z "$FOUND" ] || fail "$1 wrote a file nothing reads: $FOUND"
}

PERSONA='# Who you are

Answer briefly.'

export JINGCLAW_HOME="$WORK/deployment"
export JINGCLAW_CONFIG='[agent]
max_iterations = 4

[provider]
backend = "fake"
fake_model = "fake-echo"
'
export JINGCLAW_PERSONA="base64:$(printf '%s' "$PERSONA" | base64 | tr -d '\n')"
export JINGCLAW_AGENTS='# How this project works

It is a check.'

# What the agent is told is made from the saved documents. A variable that never
# reached them would leave this prompt as the built-in one.
JINGCLAW_HOME="$WORK/asked" "$WORK/jingclaw" daemon --print-prompt >"$WORK/prompt" 2>/dev/null ||
	fail "the prompt could not be printed from settings a variable supplied"
grep -q 'Answer briefly\.' "$WORK/prompt" || fail "the persona never reached the prompt"
grep -q 'It is a check\.' "$WORK/prompt" || fail "the instructions never reached the prompt"
printf 'ok   what a variable supplies is what the agent is told\n'

start first

# The variables seed the database, and nothing is written for anyone to edit:
# the files are not read, so one would be a setting that does nothing.
no_settings_files "starting from variables"

# The whole point of the encoding: a document arrives as a document.
same persona "$PERSONA" || fail "the persona lost something on the way through the environment: $(saved persona)"
same agents "$JINGCLAW_AGENTS" || fail "the instructions lost something on the way through the environment: $(saved agents)"

# The configuration is the one the deployment supplied, rather than the
# example, and it is the one running.
same config "$JINGCLAW_CONFIG" || fail "the settings are the example rather than the ones the deployment supplied: $(saved config)"
[ "$(call ConfigService/GetConfig '{}' | field 'int(d["running"]["agent.max_iterations"])')" = 4 ] ||
	fail "the supplied settings are saved and not what is running"
for DOCUMENT in CONFIG AGENTS PERSONA; do
	[ "$(versions $DOCUMENT)" = "1:env" ] || fail "$DOCUMENT is not one version from the environment: $(versions $DOCUMENT)"
done
[ "$(call ConfigService/GetInstructionFile '{"file":"INSTRUCTION_FILE_PERSONA"}' | field 'd.get("pendingRestart", False)')" = False ] ||
	fail "the persona the agent was built from is not the one saved"

grep -q 'created from JINGCLAW_CONFIG' "$WORK/first.out" ||
	fail "the startup line calls a supplied configuration a default"
printf 'ok   the three documents arrive in the database, intact, and are what runs\n'

# The variables arrive again on every restart. Overwriting would discard
# whatever was edited, on the restart after somebody edited it. Clearing a
# document is an edit, so it is not given its text back either.
put PERSONA '# Edited in the dashboard
'
put AGENTS ''
[ "$(saved agents)" = "" ] || fail "the instructions were not cleared"
stop
start second
same persona '# Edited in the dashboard
' || fail "a restart overwrote a document somebody had edited: $(saved persona)"
[ "$(saved agents)" = "" ] || fail "a restart put back instructions somebody had cleared: $(saved agents)"
same config "$JINGCLAW_CONFIG" || fail "a restart changed the settings"
[ "$(versions PERSONA)" = "2:web 1:env" ] || fail "a restart added a version to the persona: $(versions PERSONA)"
[ "$(versions AGENTS)" = "2:web 1:env" ] || fail "a restart added a version to the instructions: $(versions AGENTS)"
grep -q 'database (3 documents)' "$WORK/second.out" || fail "a later start does not say where the settings are: $(cat "$WORK/second.out")"
grep -q 'created from' "$WORK/second.out" && fail "a later start claims to have created the settings"
no_settings_files "restarting"
printf 'ok   a restart does not undo an edit, or give back what was cleared\n'
stop

# A value that announces an encoding and then does not decode is line noise,
# and line noise saved as a persona is a deployment nobody looks at again. A
# configuration that is not TOML would be refused later, by a daemon that had
# already been given somewhere to keep it.
for REFUSAL in 'PERSONA=base64:not base64 at all !!!|does not decode' 'CONFIG=[provider|does not parse as TOML'; do
	(
		export JINGCLAW_HOME="$WORK/refused"
		export "JINGCLAW_${REFUSAL%%|*}"
		refused
	) || fail "a value that could not be read started the daemon anyway: ${REFUSAL%%|*}: $(cat "$WORK/refusal")"
	grep -q -- "${REFUSAL#*|}" "$WORK/refusal" || fail "the refusal does not say what was wrong with the value: $(cat "$WORK/refusal")"
	[ -e "$WORK/refused" ] && fail "a refused value still left a deployment behind"
done
printf 'ok   a value that cannot be read is refused, with nothing left behind\n'

# A deployment that brings nothing still gets settings. They were created only
# by --init, so every container and every service ran with none, and the
# reason for making them rather than documenting them reached only the people
# who already knew about the flag.
unset JINGCLAW_CONFIG JINGCLAW_PERSONA JINGCLAW_AGENTS
export JINGCLAW_HOME="$WORK/plain"
start plain
[ "$(versions CONFIG)" = "1:default" ] || fail "a plain start saved no settings: $(versions CONFIG)"
grep -q 'created, all defaults' "$WORK/plain.out" || fail "a plain start does not say its settings are the defaults: $(cat "$WORK/plain.out")"
[ -z "$(versions AGENTS)$(versions PERSONA)" ] || fail "a plain start saved text nobody wrote"
no_settings_files "a plain start"

# What a redeployment looks like: the container is replaced, the volume is
# not. The settings are already there and the new one must read them rather
# than put the defaults back over them.
put PERSONA '# Edited between deployments
'
stop
start redeployed
same persona '# Edited between deployments
' || fail "a second start replaced the persona that was in the volume"
[ "$(versions CONFIG)" = "1:default" ] || fail "a second start saved the defaults again: $(versions CONFIG)"
printf 'ok   a plain start has settings to edit, and a redeployment keeps them\n'
stop

# A deployment an older version made has its own files, and they come first:
# those were the settings it ran with, and a variable that arrives with the
# new version must not replace them.
export JINGCLAW_HOME="$WORK/both"
mkdir -p "$JINGCLAW_HOME"
printf '[agent]\nmax_iterations = 6\n\n[provider]\nbackend = "fake"\n' > "$JINGCLAW_HOME/config.toml"
export JINGCLAW_CONFIG='[agent]
max_iterations = 4

[provider]
backend = "fake"
'
start both
[ "$(versions CONFIG)" = "1:file" ] || fail "a variable took the place of a file that was there: $(versions CONFIG)"
[ "$(call ConfigService/GetConfig '{}' | field 'int(d["running"]["agent.max_iterations"])')" = 6 ] || fail "the variable's settings are running, not the file's"
printf 'ok   a settings file that is there wins over a variable\n'
stop

printf 'PASS: the documents a deployment brings arrive, once, intact\n'
