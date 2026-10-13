#!/bin/sh
# Proves a credential can be put where the agent finds it and can never be read
# back.
#
# The unit tests cover the rules. What they cannot see is the daemon: whether
# the service is mounted behind the credential check, whether the file that
# lands on a real disk is what the rules say (owner-only, exactly the value,
# nothing beside it), whether a value reaches a log, the database or a reply on
# the way through a real process, and whether the daemon that exists to repair a
# key with the wrong mode serves the call that repairs it.
#
# Three deployments, one daemon at a time on one port:
#   1. a running daemon: saving, refusing, overriding, repairing, deleting
#   2. a daemon that cannot start because an older version's settings name a
#      file outside the deployment: it refuses to write there, and still serves
#   3. a daemon that cannot start because a key can be read by anybody: the call
#      that puts it right, and the start after
set -eu

# Nothing inherited may name a deployment, supply a document, or hold a
# credential: this check is about where credentials come from, and one from the
# caller would be another source.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done
unset GEMINI_API_KEY GOOGLE_API_KEY ANTHROPIC_API_KEY BRAVE_SEARCH_TOKEN DISCORD_BOT_TOKEN TELEGRAM_BOT_TOKEN

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
CLI="$WORK/jingclaw"
go build -o "$CLI" ./cmd/jingclaw

PORT=7853
BASE="http://127.0.0.1:$PORT"

# Values nothing else would say. The second is what the environment is given.
CANARY="q9Zr-7TxK2mVb8Lw3Nd5YpHc4Gs1-Xz7Q"
ENVCANARY="Wb4Tn8Hc2Zr6Kd9Pm3Lv5Xs7Qg1-Vk3R"

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# daemon is not stopped and the work directory is not removed.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

discovered() {
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$JINGCLAW_HOME/run/daemon.json" "$1"
}

# start NAME runs a daemon for the deployment JINGCLAW_HOME names, with its
# output in NAME.out and NAME.err, and returns once it has published itself.
start() {
	"$CLI" daemon >"$WORK/$1.out" 2>"$WORK/$1.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$JINGCLAW_HOME/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/$1.err")"
		sleep 0.1
	done
	TOKEN=$(discovered token)
	GATEWAY=$(discovered gateway_token)
}
stop() {
	kill "$DAEMON"
	wait "$DAEMON" 2>/dev/null || true
	DAEMON=""
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

# body KEY VALUE... is a JSON object, so that a value with quotes and line
# breaks in it reaches the API as the value it is.
body() { python3 -c 'import json,sys;print(json.dumps(dict(zip(sys.argv[1::2], sys.argv[2::2]))))' "$@"; }

# credential SLOT FIELD prints one field of one slot as ListCredentials reports
# it, and nothing for a field that is not set.
credential() {
	call CredentialService/ListCredentials '{}' | python3 -c '
import json, sys
slots = {c.get("slot"): c for c in json.load(sys.stdin)["credentials"]}
print(slots["CREDENTIAL_SLOT_" + sys.argv[1]].get(sys.argv[2], ""))' "$1" "$2"
}

# put SLOT VALUE saves a credential and prints the reply.
put() { call CredentialService/PutCredential "$(body slot "CREDENTIAL_SLOT_$1" value "$2")"; }
errorcode() { field 'd.get("code", "")'; }

mode() { python3 -c 'import os,sys;print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$1"; }

# nothing_left_beside DIR says nothing is in the deployment directory that a
# save could have left: a backup, or a temporary file.
nothing_left_beside() {
	LEFT=$(ls -A "$1" | grep -E '\.bak|\.tmp|^\.credential|~$' || true)
	[ -z "$LEFT" ] || fail "a save left something beside the file: $LEFT"
}

# nowhere says neither value is in any daemon's output, nor in the database that
# holds the settings and the log.
nowhere() {
	for WHERE in "$WORK"/*.out "$WORK"/*.err "$JINGCLAW_HOME"/data/*; do
		[ -f "$WHERE" ] || continue
		for VALUE in "$CANARY" "$ENVCANARY"; do
			if grep -qF -- "$VALUE" "$WHERE"; then
				fail "a credential is in $WHERE"
			fi
		done
	done
}

# ── 1. a running daemon ────────────────────────────────────────────────

export JINGCLAW_HOME="$WORK/running"
mkdir -p "$JINGCLAW_HOME"
chmod 700 "$JINGCLAW_HOME"
# Outside the deployment, and nothing here may touch it.
printf 'not yours\n' >"$WORK/outside.key"
chmod 644 "$WORK/outside.key"

export JINGCLAW_CONFIG="[provider]
backend = \"fake\"
fake_delay = \"0s\"

[server]
addr = \"127.0.0.1:$PORT\"
"
start running

[ "$(status CredentialService/ListCredentials '{}' not-a-credential)" = 401 ] || fail "the credentials answered a made-up credential"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' -d '{}' "$BASE/jingclaw.control.v1.CredentialService/ListCredentials")" = 401 ] ||
	fail "the credentials answered with no credential"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' -X POST -H 'content-type: application/json' \
	-H "authorization: Bearer $TOKEN" -d '{}' "$BASE/jingclaw.control.v1.CredentialService/ListCredentials")" = 403 ] ||
	fail "a rebound host reached the credentials"
printf 'ok   the credentials are behind the credential check and the host check\n'

# The settings are the example, so nothing selects a provider that needs a key.
[ "$(credential PROVIDER_GEMINI fileName)" = gemini.key ] || fail "the file is not the one the settings name"
[ "$(credential PROVIDER_GEMINI fileState)" = CREDENTIAL_FILE_STATE_ABSENT ] || fail "a file is reported where there is none"
[ "$(credential PROVIDER_GEMINI need)" = CREDENTIAL_NEED_UNUSED ] || fail "a key nothing reads is not reported as unused"
[ "$(credential PROVIDER_GEMINI process)" = CREDENTIAL_PROCESS_DAEMON ] || fail "the provider key is not read by the daemon"
[ "$(credential GATEWAY_DISCORD process)" = CREDENTIAL_PROCESS_GATEWAY ] || fail "the bot token is not read by the gateway"
[ "$(credential GATEWAY_DISCORD need)" = CREDENTIAL_NEED_REQUIRED ] || fail "the default platform's token is not required"
printf 'ok   every slot says which setting names its file, who reads it, and whether the settings make it matter\n'

# 2. Saving.
REPLY=$(put PROVIDER_GEMINI "$CANARY")
case "$REPLY" in
	*"$CANARY"*) fail "the reply to a save repeats the value: $REPLY" ;;
esac
[ "$(printf '%s' "$REPLY" | field 'd["credential"]["fileState"]')" = CREDENTIAL_FILE_STATE_PRESENT ] || fail "a save is not reported as present: $REPLY"
[ "$(printf '%s' "$REPLY" | field '"modifiedAt" in d["credential"]')" = True ] || fail "a save does not say when the file was written: $REPLY"
KEY="$JINGCLAW_HOME/gemini.key"
[ "$(mode "$KEY")" = 600 ] || fail "the file is mode $(mode "$KEY"), not 600"
[ "$(cat "$KEY")" = "$CANARY" ] || fail "the file does not hold the value"
[ "$(wc -c <"$KEY" | tr -d ' ')" = "${#CANARY}" ] || fail "the file holds more or less than the value"
nothing_left_beside "$JINGCLAW_HOME"
printf 'ok   a saved key is a mode-600 file holding exactly the value, with no backup and no temporary file beside it\n'

# The value in a request is trimmed like the file would be read: a trailing line
# break is not part of a key.
put PROVIDER_GEMINI "$CANARY
" >/dev/null
[ "$(wc -c <"$KEY" | tr -d ' ')" = "${#CANARY}" ] || fail "a trailing line break was written"
printf 'ok   and a trailing line break is not part of it\n'

# 3. Nothing it says is the value, or a part of it, or a measure of it.
LISTED=$(call CredentialService/ListCredentials '{}')
printf '%s' "$LISTED" | python3 -c '
import base64, hashlib, json, sys
canary = sys.argv[1]
text = sys.stdin.read()
for form in (canary, canary[:4], canary[-4:], base64.b64encode(canary.encode()).decode(),
             canary.encode().hex(), hashlib.sha256(canary.encode()).hexdigest(), "*" * len(canary)):
    assert form not in text, "the listing holds " + form
allowed = {"slot", "process", "need", "source", "fileSetting", "fileName", "envSetting", "envNames",
           "envInUse", "fileState", "fileDetail", "modifiedAt", "restartNeeded"}
for one in json.loads(text)["credentials"]:
    extra = set(one) - allowed
    assert not extra, "the listing carries " + ", ".join(sorted(extra))
' "$CANARY" || fail "the listing says more than the state of a slot: $LISTED"
nowhere
printf 'ok   what it says is a state and never the value, a part of it, a hash or a size\n'

# 4. Refused values leave the file as it was, and do not repeat what was sent.
for BAD in "$CANARY
$CANARY" ""; do
	REFUSED=$(put PROVIDER_GEMINI "$BAD")
	[ "$(printf '%s' "$REFUSED" | errorcode)" = invalid_argument ] || fail "a value that cannot be a key was accepted: $REFUSED"
	case "$REFUSED" in
		*"$CANARY"*) fail "the refusal repeats the value: $REFUSED" ;;
	esac
done
[ "$(cat "$KEY")" = "$CANARY" ] || fail "a refused value changed the file"
nothing_left_beside "$JINGCLAW_HOME"
printf 'ok   a value that cannot be a key is refused without repeating it, and the file is as it was\n'

# 5. compare-and-set is on the time the file was last modified.
put PROVIDER_GEMINI new >/dev/null
STALE=$(call CredentialService/PutCredential "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI value "stale" expectModifiedAt "2001-02-03T04:05:06Z")")
[ "$(printf '%s' "$STALE" | errorcode)" = failed_precondition ] || fail "a save that expected another version was accepted: $STALE"
[ "$(cat "$KEY")" = "new" ] || fail "a refused save changed the file"
WHEN=$(credential PROVIDER_GEMINI modifiedAt)
[ -n "$WHEN" ] || fail "the file has no modification time"
AGREED=$(call CredentialService/PutCredential "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI value "$CANARY" expectModifiedAt "$WHEN")")
[ "$(printf '%s' "$AGREED" | field 'd["credential"]["fileState"]')" = CREDENTIAL_FILE_STATE_PRESENT ] || fail "a save that expected this version was refused: $AGREED"
[ "$(cat "$KEY")" = "$CANARY" ] || fail "an accepted save did not change the file"
printf 'ok   a save that expects another version of the file is refused, and one that expects this one is not\n'

# 6. A link at the file's name is somebody's arrangement. Nothing is written
#    through it or over it, and nothing at the other end changes.
ln -s "$WORK/outside.key" "$JINGCLAW_HOME/discord.token"
for METHOD in "PutCredential $(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value "$CANARY")" \
	"DeleteCredential $(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD)" \
	"RestrictCredentialFile $(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD)"; do
	REFUSED=$(call "CredentialService/${METHOD%% *}" "${METHOD#* }")
	[ "$(printf '%s' "$REFUSED" | errorcode)" = failed_precondition ] || fail "${METHOD%% *} went through a link: $REFUSED"
	case "$REFUSED" in
		*"exists and is not a credential file"*) ;;
		*) fail "${METHOD%% *} does not say why: $REFUSED" ;;
	esac
done
[ -L "$JINGCLAW_HOME/discord.token" ] || fail "the link was replaced"
[ "$(cat "$WORK/outside.key")" = "not yours" ] || fail "something was written through the link"
[ "$(mode "$WORK/outside.key")" = 644 ] || fail "the mode of what the link points at was changed"
rm "$JINGCLAW_HOME/discord.token"
printf 'ok   a link at the file is refused for a save, a delete and a restrict, and what it points at is untouched\n'

# 7. A name that leaves the deployment cannot be saved as a setting, so it
#    cannot be written to either. The settings are the only place a name comes
#    from.
HASH=$(call ConfigService/GetConfig '{}' | field 'd["hash"]')
for NAME in ../outside.key /etc/passwd sub/dir.key config.toml; do
	CHECKED=$(call ConfigService/CheckConfig "$(python3 -c '
import json, sys
print(json.dumps({"edits": [{"key": "provider.gemini.api_key_file", "value": sys.argv[1]}]}))' "$NAME")")
	[ "$(printf '%s' "$CHECKED" | field 'd["problems"][0]["key"]')" = provider.gemini.api_key_file ] || fail "$NAME was accepted as a credential file: $CHECKED"
	printf '%s' "$CHECKED" | grep -q 'letters, digits' || fail "$NAME is refused without saying what is allowed: $CHECKED"
done
[ "$(call ConfigService/GetConfig '{}' | field 'd["hash"]')" = "$HASH" ] || fail "checking a name changed the settings"
printf 'ok   a file name that leaves the deployment, or is the settings file, is refused in the settings with what is allowed\n'

# 8. A file anybody could read is blocked, and putting it right needs no value.
chmod 644 "$KEY"
[ "$(credential PROVIDER_GEMINI source)" = CREDENTIAL_SOURCE_BLOCKED ] || fail "a file anybody could read is not blocked"
[ "$(credential PROVIDER_GEMINI fileState)" = CREDENTIAL_FILE_STATE_WRONG_MODE ] || fail "the file's state is not its mode"
[ "$(credential PROVIDER_GEMINI fileDetail)" = "is mode 0644" ] || fail "the mode is not said: $(credential PROVIDER_GEMINI fileDetail)"
RESTRICTED=$(call CredentialService/RestrictCredentialFile "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)")
[ "$(printf '%s' "$RESTRICTED" | field 'd["credential"]["source"]')" = CREDENTIAL_SOURCE_FILE ] || fail "restricting did not unblock it: $RESTRICTED"
[ "$(mode "$KEY")" = 600 ] || fail "the file is mode $(mode "$KEY") after it was restricted"
[ "$(cat "$KEY")" = "$CANARY" ] || fail "restricting changed the file"
printf 'ok   a file anybody could read is blocked with its mode, and restricting it needs no value and changes nothing in it\n'

# And saving it again is the other way to put it right.
chmod 644 "$KEY"
put PROVIDER_GEMINI "$CANARY" >/dev/null
[ "$(mode "$KEY")" = 600 ] || fail "saving a key over one anybody could read left it that way"
printf 'ok   and so does saving it again\n'

# 9. The gateway's credential reaches the ingress and nothing here.
for METHOD in ListCredentials PutCredential DeleteCredential RestrictCredentialFile; do
	[ "$(status "CredentialService/$METHOD" "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI value x)" "$GATEWAY")" = 403 ] ||
		fail "the gateway's credential reaches $METHOD"
done
[ "$(cat "$KEY")" = "$CANARY" ] || fail "the gateway's credential changed a key"
printf 'ok   the gateway credential is refused all four\n'

# 10. Deleting removes the file, and only that file.
printf 'kept\n' >"$JINGCLAW_HOME/telegram.token"
chmod 600 "$JINGCLAW_HOME/telegram.token"
DELETED=$(call CredentialService/DeleteCredential "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)")
[ "$(printf '%s' "$DELETED" | field 'd["credential"]["fileState"]')" = CREDENTIAL_FILE_STATE_ABSENT ] || fail "a delete is not reported: $DELETED"
[ ! -e "$KEY" ] || fail "the file is still there"
[ -f "$JINGCLAW_HOME/telegram.token" ] || fail "deleting one key removed another"
[ -z "$(call CredentialService/DeleteCredential "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)" | field 'd.get("code", "")')" ] || fail "deleting what is not there is an error"
nothing_left_beside "$JINGCLAW_HOME"
nowhere
printf 'ok   a delete removes that slot'"'"'s file and no other, and deleting nothing is not an error\n'

# 11. A variable is what is read when it is set, and it is reported by name.
put PROVIDER_GEMINI "$CANARY" >/dev/null
stop
export GEMINI_API_KEY="$ENVCANARY"
start running-with-variable
[ "$(credential PROVIDER_GEMINI source)" = CREDENTIAL_SOURCE_ENVIRONMENT ] || fail "a variable that is set is not what decides"
[ "$(credential PROVIDER_GEMINI envInUse)" = GEMINI_API_KEY ] || fail "the variable is not named"
[ "$(credential PROVIDER_GEMINI fileState)" = CREDENTIAL_FILE_STATE_PRESENT ] || fail "the file beside it is not reported"
[ -z "$(credential PROVIDER_GEMINI restartNeeded)" ] || fail "a file the variable overrides is said to need a restart"
call CredentialService/ListCredentials '{}' | grep -qF -- "$ENVCANARY" && fail "the listing holds the variable's value"
nowhere
stop
unset GEMINI_API_KEY
printf 'ok   a variable that is set decides, and is named and never shown, with the file beside it still reported\n'

# ── 2. a daemon whose settings name a file outside the deployment ──────

# What an older version's settings could say, and what it did with it: read a
# mode-600 file outside the deployment and send what was in it to a provider as
# a key. Imported as they are, they now fail the check, and what is left is a
# daemon that can be reached to correct them.
unset JINGCLAW_CONFIG
export JINGCLAW_HOME="$WORK/escaping"
mkdir -p "$JINGCLAW_HOME"
chmod 700 "$JINGCLAW_HOME"
printf 'not yours\n' >"$WORK/outside.key"
chmod 600 "$WORK/outside.key"
cat >"$JINGCLAW_HOME/config.toml" <<EOF
[provider]
backend = "fake"

[provider.gemini]
api_key_file = "../outside.key"

[server]
addr = "127.0.0.1:$PORT"
EOF
start escaping

STATE=$(call ConfigService/GetAgentState '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] || fail "settings naming a file outside the deployment started an agent: $STATE"
[ "$(printf '%s' "$STATE" | field 'd["problems"][0]["key"]')" = provider.gemini.api_key_file ] || fail "the daemon does not say which setting: $STATE"
printf '%s' "$STATE" | grep -q 'letters, digits' || fail "the daemon does not say what is allowed: $STATE"
[ "$(credential PROVIDER_GEMINI fileState)" = CREDENTIAL_FILE_STATE_INVALID_NAME ] || fail "the name is not reported as not allowed"
[ "$(credential PROVIDER_GEMINI source)" = CREDENTIAL_SOURCE_BLOCKED ] || fail "a name that is not allowed is not blocked"
[ -z "$(credential PROVIDER_GEMINI fileName)" ] || fail "a name that is not allowed is repeated"
printf 'ok   settings that name a file outside the deployment leave a daemon that says so, and serves the credentials\n'

for METHOD in "PutCredential $(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI value "$CANARY")" \
	"DeleteCredential $(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)" \
	"RestrictCredentialFile $(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)"; do
	REFUSED=$(call "CredentialService/${METHOD%% *}" "${METHOD#* }")
	[ "$(printf '%s' "$REFUSED" | errorcode)" = failed_precondition ] || fail "${METHOD%% *} reached outside the deployment: $REFUSED"
done
[ "$(cat "$WORK/outside.key")" = "not yours" ] || fail "something was written outside the deployment"
[ "$(mode "$WORK/outside.key")" = 600 ] || fail "the mode of a file outside the deployment was changed"
[ "$(ls -A "$JINGCLAW_HOME" | grep -cE 'gemini|outside' || true)" = 0 ] || fail "a file was made in the deployment: $(ls -A "$JINGCLAW_HOME")"
printf 'ok   and nothing is written, removed or restricted outside the deployment\n'

# Corrected where the daemon can be reached, the same daemon takes the key.
GOT=$(call ConfigService/GetConfig '{}')
FIXED='[provider]
backend = "fake"
'
SAVED=$(call ConfigService/ReplaceConfigText "$(body text "$FIXED" hash "$(printf '%s' "$GOT" | field 'd["hash"]')")")
[ "$(printf '%s' "$SAVED" | field 'd["written"]')" = True ] || fail "the correction was not saved: $SAVED"
put PROVIDER_GEMINI "$CANARY" >/dev/null
[ "$(cat "$JINGCLAW_HOME/gemini.key")" = "$CANARY" ] || fail "a repair daemon would not take a key once the name was corrected"
[ "$(mode "$JINGCLAW_HOME/gemini.key")" = 600 ] || fail "the key is mode $(mode "$JINGCLAW_HOME/gemini.key")"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = repair ] || fail "a repair took effect without a restart"
nowhere
stop
printf 'ok   corrected, the repair daemon takes the key, and is still a repair daemon until it is restarted\n'

# ── 3. a daemon kept from starting by a key anybody could read ─────────

# The search key stands in for any provider's: it is read when the agent is
# built, and building does not need a network.
export JINGCLAW_HOME="$WORK/exposed"
mkdir -p "$JINGCLAW_HOME"
chmod 700 "$JINGCLAW_HOME"
printf '%s\n' "$CANARY" >"$JINGCLAW_HOME/brave-search.token"
chmod 644 "$JINGCLAW_HOME/brave-search.token"
export JINGCLAW_CONFIG="[provider]
backend = \"fake\"
fake_delay = \"0s\"

[web]
enabled = true
backend = \"none\"

[web.search]
backend = \"brave\"

[server]
addr = \"127.0.0.1:$PORT\"
"
start exposed

[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = repair ] || fail "a key anybody could read did not keep the daemon from starting"
[ "$(credential WEB_SEARCH need)" = CREDENTIAL_NEED_REQUIRED ] || fail "the search key is not required of a daemon that searches"
[ "$(credential WEB_SEARCH source)" = CREDENTIAL_SOURCE_BLOCKED ] || fail "the key that kept it from starting is not blocked"
[ "$(credential WEB_SEARCH fileState)" = CREDENTIAL_FILE_STATE_WRONG_MODE ] || fail "the key's state is not its mode"
call ConfigService/GetAgentState '{}' | grep -qF -- "$CANARY" && fail "the reason there is no agent holds the key"
printf 'ok   a key anybody could read leaves a repair daemon that says which key and why\n'

RESTRICTED=$(call CredentialService/RestrictCredentialFile "$(body slot CREDENTIAL_SLOT_WEB_SEARCH)")
[ "$(printf '%s' "$RESTRICTED" | field 'd["credential"]["source"]')" = CREDENTIAL_SOURCE_FILE ] || fail "restricting did not unblock the key: $RESTRICTED"
[ "$(printf '%s' "$RESTRICTED" | field 'd["credential"]["restartNeeded"]')" = True ] || fail "putting the key right does not say a restart is needed: $RESTRICTED"
[ "$(mode "$JINGCLAW_HOME/brave-search.token")" = 600 ] || fail "the key is mode $(mode "$JINGCLAW_HOME/brave-search.token")"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = repair ] || fail "a repair took effect without a restart"
nowhere
stop

start exposed-again
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] || fail "the restart after the repair did not run an agent: $(call ConfigService/GetAgentState '{}')"
[ "$(credential WEB_SEARCH source)" = CREDENTIAL_SOURCE_FILE ] || fail "the key that was put right is not what is used"
[ -z "$(credential WEB_SEARCH restartNeeded)" ] || fail "a key written before the daemon began is said to need a restart"
nowhere
printf 'ok   restricting it from the repair daemon needs no value, and the start after it runs an agent on that key\n'
stop

printf '\nall checks passed\n'
