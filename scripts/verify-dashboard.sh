#!/bin/sh
# Proves a browser can be let in, and can change the settings, without ever
# being handed the credential programs on this machine use.
#
# The seam this checks is the one a unit test cannot: a real daemon, a link a
# person is given, a page fetched with the headers a browser would see, and
# settings edited through the API and read back out of the database.
#
#   the link carries a code, never the operator's credential
#   the code works once, and a page elsewhere cannot reach the pairing
#   a paired browser reaches the control surface, and a gateway still cannot
#   a write changes the lines it was for, refuses a stale or invalid edit,
#     keeps what the operator wrote around it, and keeps the text it replaced
#   what is saved is not what is running until the daemon starts again, and
#     says so, for each of the three documents
#   a document is put back to an earlier version through the same checked
#     save, and text that would not start is never saved
#   the page that is served shows no setting as a file, says nothing is
#     re-read, and puts nothing anyone wrote into the page as markup
#   a daemon with no agent answers the questions the repair screen asks
#   a key put through the page's calls is a mode-600 file holding exactly the
#     value, is never in a reply, a log or the database, is refused when the
#     file changed since the page listed it, and a key anybody can read is put
#     right from the daemon with no agent, which is where it is most often found
#   the page has one password box, bound to nothing, cleared after every save
#   the management pages are served, take another person's words only as text,
#     and nothing on the board's own poll asks for what they show
#   a schedule is made through the page's call, refused with the daemon's own
#     words when it could never run; a memory is listed and forgotten, and
#     forgetting it twice is told apart; a binding takes effect and ends at
#     once, one the saved settings declare is bound again by a restart, and
#     taking one out of the settings does not unbind it
#   a schedule or an approval whose text is Chinese is never cut through a
#     character, so the lists of sessions and approvals still answer; a schedule
#     that could never run is a request to correct and not a daemon fault
#   a key saved when there was no file leaves one that has appeared alone
#   the seventeenth browser gets in, and the one that has been in longest goes
#   what a daemon says about a missing key never repeats the file's name
#   a browser's access ends when the daemon stops
#   no credential is in anything the daemon says
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

# The settings are what a deployment supplies with its first start, and the
# database is where they live from then on. Nothing here is a file the daemon
# reads, and the variables are set again on every start below, as a
# container's are: they must not undo what was edited in between.
CONFIG='# Written by hand, and it should still be here afterwards.
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[server]
addr = "127.0.0.1:7852"
'
export JINGCLAW_CONFIG="$CONFIG"
export JINGCLAW_PERSONA='# who it is'
# $(...) drops the trailing newline, so every text is compared without it.
ORIGINAL=$(printf '%s' "$CONFIG")

start() {
	"$WORK/jingclaw" daemon >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$JINGCLAW_HOME/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
		sleep 0.1
	done
	LOCAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$JINGCLAW_HOME/run/daemon.json")
	GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("gateway_token",""))' "$JINGCLAW_HOME/run/daemon.json")
}
start

BASE="http://127.0.0.1:7852"

jingclaw() { "$WORK/jingclaw" "$@"; }

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
body() { python3 -c 'import json,sys;print(json.dumps(dict(zip(sys.argv[1::2], sys.argv[2::2]))))' "$@"; }

# The saved config text, and the saved versions of a document, newest first as
# "number:source" pairs.
config_text() { call ConfigService/GetConfig "$SESSION" '{}' | field 'd["text"]'; }
versions() {
	call ConfigService/ListDocumentVersions "$SESSION" "{\"document\":\"$1\"}" |
		field '" ".join(v["version"] + ":" + v["source"] for v in d["versions"])'
}

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
# The policy is what stands behind "nothing anyone wrote is read as markup".
POLICY=$(printf '%s' "$HEADERS" | grep -i '^content-security-policy:' | tr -d '\r' | sed 's/^[^:]*: *//')
[ "$POLICY" = "default-src 'none'; script-src 'self' 'unsafe-eval'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'" ] ||
	fail "the policy is not the one the page is built for: $POLICY"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com:7852' "$BASE/ui/")
[ "$STATUS" = 403 ] || fail "a rebound host was given the page: $STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Host: evil.example.com:7852' \
	-d "{\"code\":\"$CODE\"}" "$BASE/ui/pair")
[ "$STATUS" = 403 ] || fail "a rebound host reached the pairing: $STATUS"
printf 'ok   the page carries its policy, and a rebound host reaches neither it nor the pairing\n'

# What the page is made of. Nothing here needs a browser: the files are what
# the daemon serves, and these are the sentences and the one rule they must keep.
curl -s "$BASE/ui/" > "$WORK/served-index.html"
for ASSET in app.js schema.js; do
	curl -s "$BASE/ui/$ASSET" > "$WORK/served-$ASSET"
done
for ASSET in index.html app.js schema.js; do
	[ -s "$WORK/served-$ASSET" ] || fail "$ASSET is not served"
done
# The page names its own icon, so a browser has no reason to ask for one it does not have.
grep -qF 'rel="icon"' "$WORK/served-index.html" || fail "the page names no icon"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/ui/favicon.svg")" = 200 ] || fail "the icon the page names is not served"
# Text written by an agent, a person or a repository is shown with x-text and
# never as markup. The three that are markup show the schema's own sentences.
[ "$(grep -o 'x-html=' "$WORK/served-index.html" | wc -l | tr -d ' ')" = 3 ] || fail "the page puts something other than the schema's own text in as markup"
for SINK in innerHTML outerHTML insertAdjacentHTML document.write ':href' ':src' x-html; do
	if grep -q -- "$SINK" "$WORK/served-app.js"; then fail "app.js contains $SINK"; fi
done
# The settings are not a file, and nothing is re-read between runs.
for RETIRED in 設定檔 從檔案 config.toml PERSONA.md 不需要重啟 重新讀取 下一次執行就會用到; do
	grep -qF -- "$RETIRED" "$WORK/served-index.html" "$WORK/served-app.js" "$WORK/served-schema.js" && fail "the page still says: $RETIRED"
done
# A setting that says where the settings are kept is not offered as one of them.
for BOOTSTRAP in server.data_dir server.runtime_dir; do
	grep -qF -- "$BOOTSTRAP" "$WORK/served-schema.js" && fail "the page offers $BOOTSTRAP"
done
printf 'ok   the served page has no x-html but the schema'"'"'s own, no wording about a settings file, and no setting that says where the settings are\n'

# The cards that wait on a person show what an agent or a chat user wrote only
# through vis, with the arguments in full, oldest first so that one arriving
# does not move the button somebody is about to press; a read that failed is not
# shown as an empty board; the key box holds a save to "there was no file".
for LINE in 'x-text="vis(argsText(p.x.arguments))"' 'x-text="vis(p.x.summary)"' 'x-text="vis(p.x.prompt)"' 'x-text="vis(s.title)"'; do
	grep -qF -- "$LINE" "$WORK/served-index.html" || fail "the board does not show $LINE"
done
grep -qF 'sort((m, n) => m.created - n.created)' "$WORK/served-app.js" || fail "the waiting cards are not oldest first"
grep -qF 'this.toast = vis(t)' "$WORK/served-app.js" || fail "a toast is not shown through vis"
grep -qF 'location.reload()' "$WORK/served-app.js" || fail "a credential that ended leaves the page as it was"
grep -qF 'body.expectAbsent = true' "$WORK/served-app.js" || fail "a key box opened on no file does not hold its save to that"
[ "$(grep -c 'boardKnown' "$WORK/served-index.html")" -ge 3 ] || fail "the board says nothing is there without knowing it was read"
printf 'ok   the board shows what others wrote through vis with the arguments in full, oldest first, and does not call an unread board empty\n'

# A key typed into the page has one place to be: the password box, read once
# when it is sent and cleared whether the call worked or not.
[ "$(grep -o 'type="password"' "$WORK/served-index.html" | wc -l | tr -d ' ')" = 1 ] || fail "the page has more or fewer than one password box"
BOX=$(grep -o '<input[^>]*type="password"[^>]*>' "$WORK/served-index.html")
case "$BOX" in *'autocomplete="off"'*) ;; *) fail "the password box lets the browser fill or keep it: $BOX" ;; esac
case "$BOX" in *x-model*|*:value*|*@input*|*x-ref*) fail "the password box is bound to the page's state: $BOX" ;; esac
[ "$(grep -o 'API.call("CredentialService", "PutCredential"' "$WORK/served-app.js" | wc -l | tr -d ' ')" = 1 ] || fail "a key is saved from more than the one place that clears the box"
grep -qF 'finally { input.value = ""' "$WORK/served-app.js" || fail "the password box is not cleared when the call fails"
for SINK in localStorage sessionStorage console. document.cookie; do
	# As written, not as a pattern: "console." is also the start of "consoles".
	if grep -qF -- "$SINK" "$WORK/served-app.js"; then fail "app.js contains $SINK"; fi
done
printf 'ok   the page has one password box, bound to nothing, cleared after every save, and nothing in it stores or logs\n'

# The skills page shows a repository's words, so it shows them as text, and it
# installs only the tree it showed. verify-skills-web.sh proves the calls it
# makes against a real repository; this is what the page that is served says.
[ "$(grep -o '<pre class="raw"' "$WORK/served-index.html" | wc -l | tr -d ' ')" = 3 ] || fail "the page has more or fewer raw boxes than SKILL.md, a file and git's error"
[ "$(grep -o '<pre class="raw"[^>]*x-text="vis(' "$WORK/served-index.html" | wc -l | tr -d ' ')" = 3 ] || fail "a raw box is not shown through vis, which marks what hides or reorders text"
[ "$(grep -o 'API.call("SkillService", "ActivateSkill"' "$WORK/served-app.js" | wc -l | tr -d ' ')" = 1 ] || fail "a skill is installed from more than the one place"
grep -qF 'treeDigest: rv.treeDigest, replace: replace === true' "$WORK/served-app.js" || fail "an installation does not send the digest that was shown, or replaces without being told"
[ "$(grep -o 'activate(true)' "$WORK/served-index.html" | wc -l | tr -d ' ')" = 1 ] || fail "a replacement can be asked for from more than its confirmation"
SKILL_CALLS=$(grep -o 'API.call("SkillService", "[A-Za-z]*"' "$WORK/served-app.js" | sed 's/.*, "//; s/"//' | sort -u | tr '\n' ' ')
[ "$SKILL_CALLS" = "ActivateSkill DiscardStagedSkill GetStagedSkill ListSkills RemoveSkill StageSkill " ] || fail "the page calls other skill methods than the six: $SKILL_CALLS"
printf 'ok   the skills page shows raw text only through vis, installs from one place with the digest it showed, and calls the six methods\n'

# The management pages show what an agent remembered, which can be what
# somebody on a chat platform said, and bindings, which are ids somebody
# supplied: as text only, deleted only from a confirmation, and not asked for
# by the poll the board runs on its own. Everything they call is a method the
# page was built against.
CALLS_OF() { grep -o "API.call(\"$1\", \"[A-Za-z]*\"" "$WORK/served-app.js" | sed 's/.*, "//; s/"//' | sort -u | tr '\n' ' '; }
[ "$(CALLS_OF MemoryService)" = "ForgetMemory ListMemories " ] || fail "the page calls other memory methods than two: $(CALLS_OF MemoryService)"
[ "$(CALLS_OF ChannelService)" = "DeleteBinding ListBindings UpsertBinding " ] || fail "the page calls other channel methods than three: $(CALLS_OF ChannelService)"
for ONCE in 'API.call("MemoryService", "ForgetMemory"' 'API.call("ChannelService", "DeleteBinding"' 'API.call("ChannelService", "UpsertBinding"' 'API.call("SessionService", "CreateSchedule"'; do
	[ "$(grep -oF "$ONCE" "$WORK/served-app.js" | wc -l | tr -d ' ')" = 1 ] || fail "$ONCE is in more or fewer places than one"
done
for CONFIRMED in 'forgetMem()' 'removeBinding()'; do
	[ "$(grep -oF "@click=\"$CONFIRMED\"" "$WORK/served-index.html" | wc -l | tr -d ' ')" = 1 ] || fail "$CONFIRMED can be reached from more than its confirmation"
done
TICK=$(awk '/^      tick\(\) \{/{p=1} p{print} /^      \},/{if (p) exit}' "$WORK/served-app.js")
case "$TICK" in *Memory*|*Bindings*) fail "the board's poll asks for memories or bindings" ;; esac
[ -n "$TICK" ] || fail "the check could not read the poll"
if grep -E 'x-text="[^"]*\b(m\.text|b\.channel|m\.scopeRef)\b' "$WORK/served-index.html" | grep -v 'vis(' >/dev/null; then fail "the management pages show a memory or a channel without vis"; fi
printf 'ok   the memory and channel pages call five methods, delete only from a confirmation, show what others wrote through vis, and are not on the board'"'"'s poll\n'

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

# Every skill method the page calls is served, to a browser's session and not to a gateway's.
[ "$(status SkillService/ListSkills "$SESSION" '{}')" = 200 ] || fail "a session cannot list skills"
for METHOD in ListSkills StageSkill GetStagedSkill ActivateSkill DiscardStagedSkill RemoveSkill; do
	[ "$(status "SkillService/$METHOD" "$SESSION" '{}')" != 404 ] || fail "the daemon does not serve SkillService/$METHOD, which the page calls"
	[ "$(status "SkillService/$METHOD" "$GATEWAY" '{}')" = 403 ] || fail "the gateway credential reaches SkillService/$METHOD"
done
[ "$(call SkillService/ListSkills "$SESSION" '{}' | field 'len(d.get("installed", [])) + len(d.get("staged", [])) + int(d.get("restartRequired", False))')" = 0 ] ||
	fail "a deployment with no skills lists some, or wants a restart for them"
printf 'ok   the skills the page asks for are served to a session and not to a gateway, and an empty deployment is an empty list\n'

# 6. What is saved, and what is running.
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
HASH=$(printf '%s' "$GOT" | field 'd["hash"]')
[ -n "$HASH" ] || fail "no hash: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = "$ORIGINAL" ] || fail "the saved text is not what the deployment supplied: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["file"]["provider.backend"]')" = fake ] || fail "the saved setting is not shown: $GOT"
[ "$(printf '%s' "$GOT" | field 'int(d["running"]["agent.max_iterations"])')" = 12 ] || fail "the running value is not shown: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["runningHash"]')" = "$HASH" ] || fail "a daemon that has just started is not running what is saved: $GOT"
[ "$(printf '%s' "$GOT" | field 'd.get("pendingRestart", False)')" = False ] || fail "a daemon that has just started says a change is waiting: $GOT"
[ "$(printf '%s' "$GOT" | field 'bool(d.get("path"))')" = False ] || fail "the settings are reported as a file: $GOT"
STATE=$(call ConfigService/GetAgentState "$SESSION" '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = running ] || fail "the daemon does not say it is running an agent: $STATE"
printf 'ok   it shows what is saved next to what is running, and they are the same\n'

# 7. An edit is checked without being saved, and refused when it would not start.
CHECK=$(call ConfigService/CheckConfig "$SESSION" '{"edits":[{"key":"context.compact_at","value":1.5}]}')
[ "$(printf '%s' "$CHECK" | field 'd["problems"][0]["key"]')" = context.compact_at ] || fail "the check missed it: $CHECK"
[ "$(config_text)" = "$ORIGINAL" ] || fail "a check changed the settings"
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"context.compact_at\",\"value\":1.5}]}")
[ "$(printf '%s' "$WROTE" | field 'd.get("written", False)')" = False ] || fail "an invalid edit was written: $WROTE"
[ "$(config_text)" = "$ORIGINAL" ] || fail "a refused write changed the settings"
[ "$(versions SETTINGS_DOCUMENT_CONFIG)" = "1:env" ] || fail "a check or a refused write left a version: $(versions SETTINGS_DOCUMENT_CONFIG)"
printf 'ok   an edit that would not start is checked, and refused, with the settings untouched\n'

# 8. A real write changes what it was for, leaves the rest as the operator wrote
#    it, comment included, and keeps the text it replaced.
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"agent.max_iterations\",\"value\":7}]}")
[ "$(printf '%s' "$WROTE" | field 'd["written"]')" = True ] || fail "the write was refused: $WROTE"
SAVED=$(config_text)
printf '%s\n' "$SAVED" | grep -q '^max_iterations = 7$' || fail "the setting is not in the saved text: $SAVED"
case "$SAVED" in
	"$ORIGINAL"*) ;;
	*) fail "what the operator wrote was disturbed: $SAVED" ;;
esac
[ "$(versions SETTINGS_DOCUMENT_CONFIG)" = "2:web 1:env" ] || fail "the write is not a second version: $(versions SETTINGS_DOCUMENT_CONFIG)"
OLD=$(call ConfigService/GetDocumentVersion "$SESSION" '{"document":"SETTINGS_DOCUMENT_CONFIG","version":1}' | field 'd["text"]')
[ "$OLD" = "$ORIGINAL" ] || fail "the version it replaced is not the text it replaced: $OLD"
printf 'ok   a write changes what it was for, keeps what was written around it, and keeps the old text\n'

# What it says about itself: saved, and not running yet. The daemon reads its
# settings once, so a value that has just been saved is still the old one.
[ "$(printf '%s' "$WROTE" | field 'd["pendingRestart"]')" = True ] || fail "a write does not say it waits for a restart: $WROTE"
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
[ "$(printf '%s' "$GOT" | field 'int(d["running"]["agent.max_iterations"])')" = 12 ] || fail "a saved value is already running: $GOT"
[ "$(printf '%s' "$GOT" | field 'int(d["file"]["agent.max_iterations"])')" = 7 ] || fail "the saved value is not shown: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["pendingRestart"]')" = True ] || fail "saved settings that are not running are not marked: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["runningHash"] != d["hash"]')" = True ] || fail "the running hash is the saved one: $GOT"
printf 'ok   and it says the change is saved and not running yet\n'

# 9. An edit that began from a version of the text that is gone is refused.
STATUS=$(status ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"agent.max_iterations\",\"value\":9}]}")
[ "$STATUS" = 400 ] || fail "a stale write got $STATUS, want 400 (failed precondition)"
printf '%s\n' "$(config_text)" | grep -q '^max_iterations = 7$' || fail "a stale write changed the settings"
[ "$(versions SETTINGS_DOCUMENT_CONFIG)" = "2:web 1:env" ] || fail "a stale write left a version"
printf 'ok   a write that started from older settings is refused\n'

# 9b. The raw text editor saves whole text through the same check, and never
#     saves text that would not start, or that names where the settings are.
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
HASH=$(printf '%s' "$GOT" | field 'd["hash"]')
SAVED=$(config_text)
for BAD in '[agent' '[agent]
max_iterations = -5' '[server]
data_dir = "/elsewhere"'; do
	REFUSED=$(call ConfigService/ReplaceConfigText "$SESSION" "$(body text "$BAD" hash "$HASH")")
	[ "$(printf '%s' "$REFUSED" | field 'd.get("written", False)')" = False ] || fail "text that would not start was saved: $REFUSED"
	[ "$(printf '%s' "$REFUSED" | field 'len(d["problems"]) > 0')" = True ] || fail "text was refused without a reason: $REFUSED"
done
[ "$(config_text)" = "$SAVED" ] || fail "refused text changed the settings"
[ "$(versions SETTINGS_DOCUMENT_CONFIG)" = "2:web 1:env" ] || fail "refused text left a version: $(versions SETTINGS_DOCUMENT_CONFIG)"
# The page does not offer the two settings that say where the settings are
# kept, and a client that asks for one anyway is told how to give it.
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"server.data_dir\",\"value\":\"/elsewhere\"}]}")
[ "$(printf '%s' "$WROTE" | field 'd.get("written", False)')" = False ] || fail "an edit to server.data_dir was written: $WROTE"
[ "$(printf '%s' "$WROTE" | field 'd["problems"][0]["key"]')" = server.data_dir ] || fail "an edit to server.data_dir was not refused by name: $WROTE"
printf '%s' "$WROTE" | grep -q 'JINGCLAW_SERVER_DATA_DIR' || fail "the refusal does not say how server.data_dir is given: $WROTE"
printf 'ok   text that would not start, or that says where the settings are, is refused and never saved\n'

# 10. The persona is read and written with the same care. It is a document in
#     the database like the config, and no more a file.
GOT=$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_PERSONA"}')
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = '# who it is' ] || fail "the persona was not read: $GOT"
PH=$(printf '%s' "$GOT" | field 'd["hash"]')
PUT=$(call ConfigService/PutInstructionFile "$SESSION" "{\"file\":\"INSTRUCTION_FILE_PERSONA\",\"text\":\"Be brief.\\n\",\"hash\":\"$PH\"}")
[ "$(printf '%s' "$PUT" | field 'd["pendingRestart"]')" = True ] || fail "a persona write does not say it waits for a restart: $PUT"
GOT=$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_PERSONA"}')
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = 'Be brief.' ] || fail "the persona was not written: $GOT"
# What the page marks as saved and not running: the hash it started from is not
# the hash that is saved, and the agents document, which nobody touched, is not marked.
[ "$(printf '%s' "$GOT" | field 'd["pendingRestart"]')" = True ] || fail "a saved persona is not marked as not running yet: $GOT"
[ "$(printf '%s' "$GOT" | field 'd["runningHash"] != d["hash"]')" = True ] || fail "the persona's running hash is the saved one: $GOT"
[ "$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_AGENTS"}' | field 'd.get("pendingRestart", False)')" = False ] ||
	fail "a document nobody saved is marked as waiting for a restart"
[ "$(versions SETTINGS_DOCUMENT_PERSONA)" = "2:web 1:env" ] || fail "the persona has no second version: $(versions SETTINGS_DOCUMENT_PERSONA)"
OLD=$(call ConfigService/GetDocumentVersion "$SESSION" '{"document":"SETTINGS_DOCUMENT_PERSONA","version":1}' | field 'd["text"]')
[ "$OLD" = '# who it is' ] || fail "the persona it replaced was not kept: $OLD"
STATUS=$(status ConfigService/PutInstructionFile "$SESSION" "{\"file\":\"INSTRUCTION_FILE_PERSONA\",\"text\":\"stale\",\"hash\":\"$PH\"}")
[ "$STATUS" = 400 ] || fail "a stale persona write got $STATUS, want 400"
[ "$(printf '%s' "$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_PERSONA"}')" | field 'd["text"]')" = 'Be brief.' ] ||
	fail "a stale persona write changed the persona"
printf 'ok   the persona can be read and written, and a stale write to it is refused\n'

# None of the three is a file, whatever an older version did.
for NAMED in config.toml AGENTS.md PERSONA.md; do
	[ -e "$WORK/$NAMED" ] && fail "$NAMED exists: the settings were also written to a file"
done
printf 'ok   and none of it is written to a file\n'

# 10b. A key put through the calls the page makes. The page lists the slots,
#      saves into one, and sends back the modification time it listed.
CANARY="CANARY-q9Zr7TxK2mVb8Lw3Nd5YpHc4Gs1Xz7Q"
mode() { python3 -c 'import os,sys;print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$1"; }
cred() {
	call CredentialService/ListCredentials "$SESSION" '{}' | python3 -c '
import json, sys
slots = {c.get("slot"): c for c in json.load(sys.stdin)["credentials"]}
print(slots["CREDENTIAL_SLOT_" + sys.argv[1]].get(sys.argv[2], ""))' "$1" "$2"
}

# The page puts controls under the settings the schema calls secretfile, and
# finds a slot by the setting that names its file: the two have to be the same
# eight.
call CredentialService/ListCredentials "$SESSION" '{}' > "$WORK/credentials.json"
python3 - "$WORK/served-schema.js" "$WORK/credentials.json" <<'PY' || fail "the schema's file-name settings are not the daemon's credential slots"
import json, sys
text = open(sys.argv[1], encoding="utf-8").read().split("window.SCHEMA = ", 1)[1].strip().rstrip(";")
offered = {f["key"] for sec in json.loads(text)["sections"] for blk in sec["blocks"] for f in blk["fields"] if f.get("kind") == "secretfile"}
slots = {c["fileSetting"] for c in json.load(open(sys.argv[2]))["credentials"]}
assert len(slots) == 8, sorted(slots)
assert offered == slots, sorted(offered ^ slots)
PY
printf 'ok   the settings the page puts key controls under are the eight slots the daemon has\n'

# The gateway reads the discord token, and the default platform is discord, so
# the slot is required and a start with the file is not what is running.
KEY="$WORK/discord.token"
[ ! -e "$KEY" ] || fail "a token file is there before one was saved"
[ "$(cred GATEWAY_DISCORD need)" = CREDENTIAL_NEED_REQUIRED ] || fail "the default platform's token is not required"
[ "$(cred GATEWAY_DISCORD process)" = CREDENTIAL_PROCESS_GATEWAY ] || fail "the token is not said to be read by the gateway"
[ "$(cred GATEWAY_DISCORD fileState)" = CREDENTIAL_FILE_STATE_ABSENT ] || fail "a file is reported where there is none"
PUT=$(call CredentialService/PutCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value "$CANARY")")
case "$PUT" in *"$CANARY"*) fail "the reply to a save repeats the value: $PUT" ;; esac
[ "$(cat "$KEY")" = "$CANARY" ] || fail "the file does not hold exactly the value"
[ "$(mode "$KEY")" = 600 ] || fail "the file is mode $(mode "$KEY"), not 600"
[ "$(printf '%s' "$PUT" | field 'd["credential"]["restartNeeded"]')" = True ] || fail "a key the next start will read is not said to wait for a restart: $PUT"
[ -z "$(cred GATEWAY_DISCORD envInUse)" ] || fail "a variable is said to decide where there is none"
printf 'ok   a key saved from the page is a mode-600 file holding exactly the value, and a restart is what puts it to use\n'

# A slot nothing reads is saved without asking for a restart.
call CredentialService/PutCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI value "$CANARY")" > /dev/null
[ -z "$(cred PROVIDER_GEMINI restartNeeded)" ] || fail "a key nothing reads is said to wait for a restart"
[ "$(cred PROVIDER_GEMINI need)" = CREDENTIAL_NEED_UNUSED ] || fail "the fake backend is said to need a gemini key"
call CredentialService/DeleteCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)" > /dev/null
[ ! -e "$WORK/gemini.key" ] || fail "a delete left the file"

# The save expects the modification time the page listed, as the string it was
# given. One that has been cut short, as a JavaScript Date would, is another
# time, and so is an old one.
WHEN=$(cred GATEWAY_DISCORD modifiedAt)
[ -n "$WHEN" ] || fail "a saved file has no modification time"
STALE=$(call CredentialService/PutCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value "$CANARY-stale" expectModifiedAt "2001-02-03T04:05:06Z")")
[ "$(printf '%s' "$STALE" | field 'd.get("code", "")')" = failed_precondition ] || fail "a save that expected an old time was accepted: $STALE"
SHORT=$(printf '%s' "$WHEN" | python3 -c 'import re,sys;print(re.sub(r"(\.\d{3})\d+Z$", r"\1Z", sys.stdin.read().strip()))')
if [ "$SHORT" != "$WHEN" ]; then
	CUT=$(call CredentialService/PutCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value "$CANARY-cut" expectModifiedAt "$SHORT")")
	[ "$(printf '%s' "$CUT" | field 'd.get("code", "")')" = failed_precondition ] || fail "a time cut to milliseconds was accepted, so a page that parsed it would be refused or would not be: $CUT"
fi
[ "$(cat "$KEY")" = "$CANARY" ] || fail "a refused save changed the file"
AGREED=$(call CredentialService/PutCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value "$CANARY-again" expectModifiedAt "$WHEN")")
[ "$(printf '%s' "$AGREED" | field 'd["credential"]["fileState"]')" = CREDENTIAL_FILE_STATE_PRESENT ] || fail "the time as it was listed was refused: $AGREED"
[ "$(cat "$KEY")" = "$CANARY-again" ] || fail "an accepted save did not change the file"
printf 'ok   a save expects the time it was listed with, as the string it was given: an older one, or one cut short, is refused\n'

# A key anybody could read is blocked with its mode, and putting it right needs no value.
chmod 644 "$KEY"
[ "$(cred GATEWAY_DISCORD source)" = CREDENTIAL_SOURCE_BLOCKED ] || fail "a file anybody could read is not blocked"
[ "$(cred GATEWAY_DISCORD fileState)" = CREDENTIAL_FILE_STATE_WRONG_MODE ] || fail "the state is not the mode"
[ "$(cred GATEWAY_DISCORD fileDetail)" = "is mode 0644" ] || fail "the mode is not said: $(cred GATEWAY_DISCORD fileDetail)"
call CredentialService/RestrictCredentialFile "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD)" > /dev/null
[ "$(mode "$KEY")" = 600 ] && [ "$(cat "$KEY")" = "$CANARY-again" ] || fail "restricting did not make it 600, or changed what is in it"
call CredentialService/DeleteCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD)" > /dev/null
[ ! -e "$KEY" ] || fail "a delete left the file"
[ "$(cred GATEWAY_DISCORD fileState)" = CREDENTIAL_FILE_STATE_ABSENT ] || fail "a deleted file is still reported"
# A box opened when there was no file saves only while there is still none: a
# file that appeared in between is somebody's, and there is no backup.
NEW=$(call CredentialService/PutCredential "$SESSION" "{\"slot\":\"CREDENTIAL_SLOT_GATEWAY_DISCORD\",\"value\":\"$CANARY-first\",\"expectAbsent\":true}")
[ "$(printf '%s' "$NEW" | field 'd["credential"]["fileState"]')" = CREDENTIAL_FILE_STATE_PRESENT ] || fail "a save that expected no file was refused when there was none: $NEW"
LATE=$(call CredentialService/PutCredential "$SESSION" "{\"slot\":\"CREDENTIAL_SLOT_GATEWAY_DISCORD\",\"value\":\"$CANARY-second\",\"expectAbsent\":true}")
[ "$(printf '%s' "$LATE" | field 'd.get("code", "")')" = failed_precondition ] || fail "a save that expected no file replaced one that is there: $LATE"
[ "$(cat "$KEY")" = "$CANARY-first" ] || fail "a refused save changed the file"
call CredentialService/DeleteCredential "$SESSION" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD)" > /dev/null
[ ! -e "$KEY" ] || fail "a delete left the file"
for METHOD in ListCredentials PutCredential DeleteCredential RestrictCredentialFile; do
	[ "$(status "CredentialService/$METHOD" "$GATEWAY" "$(body slot CREDENTIAL_SLOT_GATEWAY_DISCORD value x)")" = 403 ] || fail "the gateway's credential reaches $METHOD"
done
canary_nowhere() {
	grep -qF -- "CANARY-q9Zr7TxK2mVb8Lw3Nd5YpHc4Gs1Xz7Q" "$WORK/daemon.out" "$WORK/daemon.err" 2>/dev/null && fail "a key was written to the daemon's output"
	if [ -d "$JINGCLAW_HOME/data" ] && grep -rqF -- "CANARY-q9Zr7TxK2mVb8Lw3Nd5YpHc4Gs1Xz7Q" "$JINGCLAW_HOME/data"; then fail "a key is in the database"; fi
	return 0
}
canary_nowhere
case "$(call CredentialService/ListCredentials "$SESSION" '{}')" in *"CANARY"*) fail "the listing holds the key" ;; esac
printf 'ok   a key anybody can read is blocked with its mode and put right with no value; delete removes it; and the key is in no log, no reply and not the database\n'

# 11. Nothing the daemon says, or says back, holds a credential. Its output is
#     kept by whatever runs it, and a log that carries one outlives the daemon.
nothing_leaks() {
	ANSWERS="$(call ConfigService/GetConfig "$LOCAL" '{}')
$(call ConfigService/GetAgentState "$LOCAL" '{}')
$(call ConfigService/GetInstructionFile "$LOCAL" '{"file":"INSTRUCTION_FILE_PERSONA"}')"
	# A refusal says nothing, so it would pass for any credential. These are
	# the answers, and the check has to have read them.
	case "$ANSWERS" in
		*'"runningHash"'*'"mode"'*'"text"'*) ;;
		*) fail "the settings API did not answer: $ANSWERS" ;;
	esac
	for SECRET in "$@"; do
		grep -qF -- "$SECRET" "$WORK/daemon.out" "$WORK/daemon.err" &&
			fail "a credential was written to the daemon's output"
		case "$ANSWERS" in
			*"$SECRET"*) fail "a credential was returned by the settings API" ;;
		esac
	done
	printf 'ok   no credential is in the daemon'"'"'s output or in what the settings API returns\n'
}
nothing_leaks "$LOCAL" "$GATEWAY" "$CODE" "$SESSION"

# 12. A browser's access ends when the daemon stops. Nothing of it is kept.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
rm -f "$WORK/run/daemon.json"
start
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 401 ] || fail "a session outlived the daemon"
printf 'ok   a session does not outlive the daemon\n'

# 13. The restart is what puts a saved change into effect, and the variables
#     that arrive again with it do not undo what was saved.
SESSION=$LOCAL
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
[ "$(printf '%s' "$GOT" | field 'int(d["running"]["agent.max_iterations"])')" = 7 ] || fail "a saved setting did not take effect on restart: $GOT"
[ "$(printf '%s' "$GOT" | field 'd.get("pendingRestart", False)')" = False ] || fail "the restart still says a change is waiting: $GOT"
printf '%s\n' "$(config_text)" | grep -q '^max_iterations = 7$' || fail "the variable put the supplied settings back over the saved ones"
GOT=$(call ConfigService/GetInstructionFile "$SESSION" '{"file":"INSTRUCTION_FILE_PERSONA"}')
[ "$(printf '%s' "$GOT" | field 'd["text"]')" = 'Be brief.' ] || fail "the variable put the supplied persona back over the saved one: $GOT"
[ "$(printf '%s' "$GOT" | field 'd.get("pendingRestart", False)')" = False ] || fail "the persona is still waiting after a restart: $GOT"
printf 'ok   a restart runs what was saved, and the variables do not undo it\n'

nothing_leaks "$LOCAL" "$GATEWAY"

# 14. An earlier version is put back by saving its text again, through the same
#     check, with the hash of what is saved now. The page's restore does exactly
#     this. What is saved is compared with what is running by hash, so putting
#     back the text that is running is no longer waiting for anything.
version_text() {
	call ConfigService/GetDocumentVersion "$SESSION" "{\"document\":\"$1\",\"version\":$2}" |
		python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["text"])' > "$WORK/version.txt"
}
replace_body() {
	python3 -c 'import json,sys;print(json.dumps({"text":open(sys.argv[1],encoding="utf-8").read(),"hash":sys.argv[2]}))' "$WORK/version.txt" "$1"
}
HASH=$(call ConfigService/GetConfig "$SESSION" '{}' | field 'd["hash"]')
version_text SETTINGS_DOCUMENT_CONFIG 1
RESTORED=$(call ConfigService/ReplaceConfigText "$SESSION" "$(replace_body "$HASH")")
[ "$(printf '%s' "$RESTORED" | field 'd["written"]')" = True ] || fail "the first version was not restored: $RESTORED"
[ "$(printf '%s' "$RESTORED" | field 'd["pendingRestart"]')" = True ] || fail "a restored version that is not running is not marked: $RESTORED"
[ "$(printf '%s' "$RESTORED" | field 'd["version"]')" = 3 ] || fail "a restore is not a new version: $RESTORED"
[ "$(config_text)" = "$ORIGINAL" ] || fail "the restored text is not the first version's: $(config_text)"
[ "$(versions SETTINGS_DOCUMENT_CONFIG)" = "3:web 2:web 1:env" ] || fail "a restore dropped or rewrote a version: $(versions SETTINGS_DOCUMENT_CONFIG)"
HASH=$(printf '%s' "$RESTORED" | field 'd["hash"]')
version_text SETTINGS_DOCUMENT_CONFIG 2
BACK=$(call ConfigService/ReplaceConfigText "$SESSION" "$(replace_body "$HASH")")
[ "$(printf '%s' "$BACK" | field 'd.get("pendingRestart", False)')" = False ] || fail "putting back what is running still waits for a restart: $BACK"
printf '%s\n' "$(config_text)" | grep -q '^max_iterations = 7$' || fail "the running version was not put back"
printf 'ok   an earlier version is restored through the checked save, and what is saved is compared with what is running by hash\n'

# 15. A daemon with no agent answers what the repair screen asks of it: why
#     (fixed at the start), what the saved settings say now, and a fix that
#     takes effect only after a restart.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""
export JINGCLAW_HOME="$WORK/repair"
mkdir -p "$JINGCLAW_HOME"
unset JINGCLAW_CONFIG JINGCLAW_PERSONA
printf '[agent]\nmax_iterations = -5\n\n[server]\naddr = "127.0.0.1:7852"\n' > "$JINGCLAW_HOME/config.toml"
start
SESSION=$LOCAL
STATE=$(call ConfigService/GetAgentState "$SESSION" '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] || fail "a daemon that cannot use its settings is not in repair mode: $STATE"
[ "$(printf '%s' "$STATE" | field 'd["problems"][0]["key"]')" = agent.max_iterations ] || fail "the problem is not named by its key: $STATE"
[ "$(printf '%s' "$STATE" | field 'd["problems"][0]["value"]')" = -5 ] || fail "the problem does not say the value: $STATE"
for FIELD in why fix; do
	[ "$(printf '%s' "$STATE" | field "len(d['problems'][0]['$FIELD']) > 0")" = True ] || fail "the problem has no $FIELD: $STATE"
done
GOT=$(call ConfigService/GetConfig "$SESSION" '{}')
HASH=$(printf '%s' "$GOT" | field 'd["hash"]')
[ "$(printf '%s' "$GOT" | field '"max_iterations = -5" in d["text"]')" = True ] || fail "the saved text is not handed back: $GOT"
[ "$(printf '%s' "$GOT" | field 'd.get("runningHash", "")')" = "" ] || fail "a daemon with no agent says something is running: $GOT"
# What the page asks to say whether the saved settings pass now: a check of the
# text as saved, with nothing edited.
CHECK=$(call ConfigService/CheckConfig "$SESSION" '{"edits":[]}')
[ "$(printf '%s' "$CHECK" | field 'd["problems"][0]["key"]')" = agent.max_iterations ] || fail "a check of the saved text misses the problem: $CHECK"
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{\"hash\":\"$HASH\",\"edits\":[{\"key\":\"agent.max_iterations\",\"value\":5}]}")
[ "$(printf '%s' "$WROTE" | field 'd["written"]')" = True ] || fail "the fix was refused: $WROTE"
CHECK=$(call ConfigService/CheckConfig "$SESSION" '{"edits":[]}')
[ "$(printf '%s' "$CHECK" | field 'len(d.get("problems", []))')" = 0 ] || fail "a fixed setting is still a problem: $CHECK"
STATE=$(call ConfigService/GetAgentState "$SESSION" '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] && [ "$(printf '%s' "$STATE" | field 'd["problems"][0]["key"]')" = agent.max_iterations ] ||
	fail "a fix changed what the daemon says about its start: $STATE"
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 404 ] || fail "a daemon with no agent answers for sessions"
[ "$(status SkillService/ListSkills "$SESSION" '{}')" = 404 ] || fail "a daemon with no agent answers for skills, and the page would offer them"
printf 'ok   with no agent it says why, shows the saved text, checks it as saved, and a fix waits for a restart\n'

# 16. The daemon with no agent serves the credentials to a browser, because a
#     key anybody can read is the commonest reason for there being none. The
#     page lists them, puts a wrong mode right, and polls without a session RPC.
LINK=$(jingclaw dashboard --print 2>/dev/null)
CODE=${LINK#*#pair=}
SESSION=$(curl -s -X POST -d "{\"code\":\"$CODE\"}" "$BASE/ui/pair" | field 'd["token"]')
[ -n "$SESSION" ] || fail "a browser could not be let in to a daemon with no agent"
printf '%s\n' "$CANARY" > "$JINGCLAW_HOME/gemini.key"
chmod 644 "$JINGCLAW_HOME/gemini.key"
[ "$(status CredentialService/ListCredentials "$SESSION" '{}')" = 200 ] || fail "a daemon with no agent does not list the credentials to a browser"
[ -z "$GATEWAY" ] || fail "a daemon with no agent has a gateway credential"
[ "$(status CredentialService/ListCredentials not-a-credential '{}')" = 401 ] || fail "a daemon with no agent lists credentials to a made-up credential"
[ "$(cred PROVIDER_GEMINI source)" = CREDENTIAL_SOURCE_BLOCKED ] || fail "a key anybody can read is not blocked in a daemon with no agent"
[ "$(cred PROVIDER_GEMINI fileDetail)" = "is mode 0644" ] || fail "the mode is not said in a daemon with no agent"
FIXED=$(call CredentialService/RestrictCredentialFile "$SESSION" "$(body slot CREDENTIAL_SLOT_PROVIDER_GEMINI)")
[ "$(printf '%s' "$FIXED" | field 'd["credential"]["source"]')" = CREDENTIAL_SOURCE_FILE ] || fail "restricting did not unblock the key: $FIXED"
[ "$(mode "$JINGCLAW_HOME/gemini.key")" = 600 ] || fail "the key is mode $(mode "$JINGCLAW_HOME/gemini.key")"
[ "$(call ConfigService/GetAgentState "$SESSION" '{}' | field 'd["mode"]')" = repair ] || fail "a repair took effect without a restart"
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 404 ] || fail "a daemon with no agent answers for sessions"
canary_nowhere
printf 'ok   with no agent it still lists the credentials to a browser, and a key anybody can read is put right from there\n'

# 17. The pages that manage what lives in the database: schedules, memory and
#     bindings. A deployment of its own, with memory on and one channel in the
#     saved settings, and a model scripted to ask to remember two things.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""
export JINGCLAW_HOME="$WORK/manage"
mkdir -p "$JINGCLAW_HOME"
export JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Noting the first."
tool = "remember"
args = "{\"text\":\"deploy only after make test\",\"activation\":\"standing\"}"

[[provider.fake_script]]
text = "Noting the second."
tool = "remember"
args = "{\"text\":\"backups are in /srv/backup\"}"

[[provider.fake_script]]
text = "Noted."

[memory]
enabled = true

[gateway]
platform = "discord"

[[gateway.discord.channels]]
channel_ids = ["111111111111111111"]
tenant_id = "222222222222222222"
workspace_id = "default"
users = ["333333333333333333"]
approvers = ["333333333333333333"]

[server]
addr = "127.0.0.1:7852"
'
start
pair() {
	LINK=$(jingclaw dashboard --print 2>/dev/null)
	SESSION=$(curl -s -X POST -d "{\"code\":\"${LINK#*#pair=}\"}" "$BASE/ui/pair" | field 'd["token"]')
	[ -n "$SESSION" ] || fail "a browser could not be let in"
}
pair
META='"meta":{"clientId":"web","requestId":"verify"}'

# 17a. A schedule: made through the page's call, and refused in the daemon's own
#      words when it could never run. The page shows those words and adds none.
for BAD in '0 25 * * *:hour' 'every day:fields' '0 9 30 2 *:ever come'; do
	EXPR=${BAD%%:*}; WORDS=${BAD#*:}
	REPLY=$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"$EXPR\",\"prompt\":\"x\"}")
	printf '%s' "$REPLY" | grep -q "$WORDS" || fail "an expression that cannot run ($EXPR) is not refused with words naming why: $REPLY"
	printf '%s' "$REPLY" | grep -q '"schedule"' && fail "an expression that cannot run was made: $REPLY"
done
REPLY=$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"0 9 * * *\",\"zone\":\"Mars/Phobos\",\"prompt\":\"x\"}")
printf '%s' "$REPLY" | grep -q 'zone' || fail "a zone this machine does not have is not refused: $REPLY"
printf '%s' "$REPLY" | grep -q 'sch_' && fail "the refusal names an id nobody has seen: $REPLY"
# A request to correct, and so a 400 with that code: the page logs a failure
# of the daemon for anything else.
[ "$(status SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"0 25 * * *\",\"prompt\":\"x\"}")" = 400 ] || fail "an expression that could never run is not a request to correct"
[ "$(printf '%s' "$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"0 25 * * *\",\"prompt\":\"x\"}")" | field 'd["code"]')" = invalid_argument ] || fail "an expression that could never run is not invalid_argument"
[ "$(call SessionService/ListSchedules "$SESSION" '{}' | field 'len(d.get("schedules", []))')" = 0 ] || fail "a refused schedule was kept"
MADE=$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"0 9 * * 1-5\",\"zone\":\"Asia/Taipei\",\"prompt\":\"what changed yesterday\",\"missedPolicy\":\"skip\"}")
[ "$(printf '%s' "$MADE" | field 'd["schedule"]["expression"] + "|" + d["schedule"]["zone"] + "|" + d["schedule"]["missedPolicy"]')" = "0 9 * * 1-5|Asia/Taipei|skip" ] || fail "a schedule was not made as asked: $MADE"
[ -n "$(printf '%s' "$MADE" | field 'd["schedule"].get("nextAt", "")')" ] || fail "a new schedule has no next time to say: $MADE"
MADE=$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"@hourly\",\"prompt\":\"is anything failing\"}")
[ "$(printf '%s' "$MADE" | field 'd["schedule"].get("zone", "") + d["schedule"].get("missedPolicy", "")')" = "" ] || fail "an empty zone and policy were not kept empty: $MADE"
[ "$(call SessionService/ListSchedules "$SESSION" '{}' | field 'len(d["schedules"])')" = 2 ] || fail "the two schedules are not listed"
# The session a schedule makes is named after what it asks. A Chinese prompt
# longer than the title is cut through a character by a cut in bytes, and every
# listing of sessions then fails, for the board and the command line alike.
LONG='整理昨天 workspace 裡改了什麼，並且把結果整理成一段摘要，放進報告裡，再寄給所有需要知道的人，最後記得確認有沒有寄成功'
LONG_JSON=$(printf '%s' "$LONG" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')
CJK=$(call SessionService/CreateSchedule "$SESSION" "{$META,\"expression\":\"0 9 * * *\",\"prompt\":$LONG_JSON}")
[ "$(printf '%s' "$CJK" | field 'bool(d["schedule"]["id"])')" = True ] || fail "a Chinese schedule was not made: $CJK"
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 200 ] || fail "a Chinese schedule prompt broke the listing of sessions"
call SessionService/ListSessions "$SESSION" '{}' | python3 -c '
import json, sys
titles = [s["title"] for s in json.load(sys.stdin)["sessions"] if s["title"].startswith("0 9 * * *")]
assert titles, "no session made by the schedule"
for t in titles:
    assert "\ufffd" not in t and t.endswith("…"), t' || fail "the title of a schedule's session is not a whole cut of its prompt"
jingclaw session list >/dev/null || fail "the command line cannot list sessions after a Chinese schedule"
call SessionService/DeleteSchedule "$SESSION" "{$META,\"scheduleId\":\"$(printf '%s' "$CJK" | field 'd["schedule"]["id"]')\"}" >/dev/null
printf 'ok   a schedule is made as asked with its next time, and one that could never run is refused with the daemon'"'"'s own words and not kept\n'

# 17b. Memory: two things the model asked to remember and a person allowed.
SID=$(call SessionService/CreateSession "$SESSION" "{$META,\"title\":\"seed\"}" | field 'd["session"]["id"]')
call SessionService/SendTurn "$SESSION" "{$META,\"sessionId\":\"$SID\",\"text\":\"note these\"}" >/dev/null
WAITED=0
while [ "$(call MemoryService/ListMemories "$SESSION" '{}' | field 'len(d.get("memories", []))')" -lt 2 ]; do
	for A in $(call SessionService/ListApprovals "$SESSION" "{\"sessionId\":\"$SID\"}" | field '" ".join(a["id"] for a in d.get("approvals", []) if a["status"] == "APPROVAL_STATUS_PENDING")'); do
		call SessionService/DecideApproval "$SESSION" "{$META,\"approvalId\":\"$A\",\"remember\":\"REMEMBER_SCOPE_ONCE\",\"decision\":\"APPROVAL_DECISION_ALLOW\"}" >/dev/null
	done
	WAITED=$((WAITED + 1)); [ "$WAITED" -gt 200 ] && fail "the model's two memories were never written"
	sleep 0.1
done
LISTED=$(call MemoryService/ListMemories "$SESSION" '{"includeInvalidated":true,"limit":200}')
[ "$(printf '%s' "$LISTED" | field 'len(d["memories"])')" = 2 ] || fail "memories are not listed as the page asks: $LISTED"
for FIELD in id text activation scope scopeRef trust sourceSessionId approvedBy createdAt; do
	[ "$(printf '%s' "$LISTED" | field "all(m.get('$FIELD') for m in d['memories'])")" = True ] || fail "a memory has no $FIELD, which the page shows: $LISTED"
done
[ "$(printf '%s' "$LISTED" | field 'all(m["origin"]["kind"] == "RUN_ORIGIN_KIND_LOCAL_CLIENT" for m in d["memories"])')" = True ] || fail "a memory does not say where it came from: $LISTED"
[ "$(status MemoryService/ListMemories "$GATEWAY" '{}')" = 403 ] || fail "the gateway credential reaches the memories"
GONE=$(printf '%s' "$LISTED" | field '[m["id"] for m in d["memories"] if "backups" in m["text"]][0]')
[ "$(status MemoryService/ForgetMemory "$SESSION" "{$META,\"id\":\"$GONE\"}")" = 200 ] || fail "a memory was not forgotten"
[ "$(call MemoryService/ListMemories "$SESSION" '{"includeInvalidated":true}' | field 'len(d["memories"])')" = 1 ] || fail "a forgotten memory is still listed, or more went with it"
# The page tells "already gone" from a failure by this code.
[ "$(status MemoryService/ForgetMemory "$SESSION" "{$META,\"id\":\"$GONE\"}")" = 404 ] || fail "forgetting what is already gone is not told apart as not found"
printf 'ok   memories are listed with where they came from, forgotten one at a time, and forgetting twice is told apart\n'

# 17c. Bindings. The saved settings declare one channel.
N=0
deliver() {
	N=$((N + 1))
	status GatewayIngressService/DeliverInbound "$GATEWAY" "{\"message\":{\"platform\":\"discord\",\"accountId\":\"main\",\"tenantId\":\"222222222222222222\",\"channelId\":\"$1\",\"platformMessageId\":\"m$N\",\"idempotencyKey\":\"k$N\",\"principalId\":\"333333333333333333\",\"text\":\"hello\",\"trigger\":\"MESSAGE_TRIGGER_MENTION\"}}"
}
bound() { call ChannelService/ListBindings "$SESSION" '{}' | field "sorted(b['channelId'] for b in d.get('bindings', []))"; }
[ "$(bound)" = "['111111111111111111']" ] || fail "the channel in the saved settings was not bound at start: $(bound)"
[ "$(status ChannelService/ListBindings "$GATEWAY" '{}')" = 403 ] || fail "the gateway credential reaches the bindings"
[ "$(deliver 888888888888888888)" != 200 ] || fail "a message from an unbound channel was taken"
UPSERT=$(call ChannelService/UpsertBinding "$SESSION" "{$META,\"binding\":{\"platform\":\"discord\",\"accountId\":\"main\",\"tenantId\":\"222222222222222222\",\"channelId\":\"888888888888888888\",\"workspaceId\":\"default\",\"permissionProfile\":\"gateway\"}}")
[ "$(printf '%s' "$UPSERT" | field 'd["binding"]["channelId"]')" = 888888888888888888 ] || fail "a binding was not added: $UPSERT"
# Naming nobody comes back as nothing named, which is what the page reads as
# whoever the platform lets in.
[ "$(call ChannelService/ListBindings "$SESSION" '{}' | field '[b.get("allowedPrincipals", []) + b.get("allowedClaims", []) for b in d["bindings"] if b["channelId"] == "888888888888888888"][0]')" = "[]" ] || fail "a binding made with nobody named lists somebody"
[ "$(deliver 888888888888888888)" = 200 ] || fail "a binding took effect only after a restart"
BID=$(call ChannelService/ListBindings "$SESSION" '{}' | field '[b["id"] for b in d["bindings"] if b["channelId"] == "888888888888888888"][0]')
[ "$(status ChannelService/DeleteBinding "$SESSION" "{$META,\"bindingId\":\"$BID\"}")" = 200 ] || fail "a binding was not removed"
[ "$(deliver 888888888888888888)" != 200 ] || fail "a removed binding still took a message"
# Removing what is already gone is not an error, so a page that is a little
# behind another one says nothing wrong by having asked.
[ "$(status ChannelService/DeleteBinding "$SESSION" "{$META,\"bindingId\":\"$BID\"}")" = 200 ] || fail "removing a binding that is already gone is an error"
printf 'ok   a binding takes effect and ends at once, with no restart, and removing it again changes nothing\n'

# What the page says about the settings: the channel they declare is written
# again at every start, a binding added elsewhere is not touched, and taking
# the channel out of the settings does not unbind it.
DECLARED=$(call ChannelService/ListBindings "$SESSION" '{}' | field '[b["id"] for b in d["bindings"] if b["channelId"] == "111111111111111111"][0]')
call ChannelService/UpsertBinding "$SESSION" "{$META,\"binding\":{\"platform\":\"discord\",\"accountId\":\"main\",\"tenantId\":\"222222222222222222\",\"channelId\":\"999999999999999999\",\"workspaceId\":\"default\"}}" >/dev/null
[ "$(status ChannelService/DeleteBinding "$SESSION" "{$META,\"bindingId\":\"$DECLARED\"}")" = 200 ] || fail "the declared binding was not removed"
[ "$(bound)" = "['999999999999999999']" ] || fail "the removal did not leave only the one added here: $(bound)"
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""; rm -f "$JINGCLAW_HOME/run/daemon.json"
start; pair
[ "$(bound)" = "['111111111111111111', '999999999999999999']" ] || fail "a restart did not bind the declared channel again, or dropped the one added here: $(bound)"
HASH=$(call ConfigService/GetConfig "$SESSION" '{}' | field 'd["hash"]')
WROTE=$(call ConfigService/WriteConfig "$SESSION" "{$META,\"hash\":\"$HASH\",\"edits\":[{\"key\":\"gateway.discord.channels\",\"remove\":true}]}")
[ "$(printf '%s' "$WROTE" | field 'd["written"]')" = True ] || fail "the channel was not taken out of the settings: $WROTE"
if call ConfigService/GetConfig "$SESSION" '{}' | field 'd["text"]' | grep -q '^channel_ids'; then fail "the settings still name the channel"; fi
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""; rm -f "$JINGCLAW_HOME/run/daemon.json"
start; pair
[ "$(bound)" = "['111111111111111111', '999999999999999999']" ] || fail "taking the channel out of the settings unbound it: $(bound)"
printf 'ok   a binding the settings declare is bound again by a restart, one added here stays, and taking it out of the settings does not unbind it\n'

# 18. An approval is a protobuf message, and its summary must be text. The
#     summary keeps only the start of each argument, and a cut in bytes through
#     Chinese leaves a summary nobody can list: the run waits on an approval no
#     page, and no command, can show. The arguments are listed whole.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""
export JINGCLAW_HOME="$WORK/cjk"
mkdir -p "$JINGCLAW_HOME"
export JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Writing it."
tool = "write_file"
args = "{\"path\":\"notes.md\",\"content\":\"a這是一份很長的中文說明文件，裡面有許多字，用來看摘要會被切在哪裡。再多一些字讓它超過限制，而且要超過六十個字才會被切，所以最後這句話只在完整的參數裡。\"}"

[server]
addr = "127.0.0.1:7852"
'
start; pair
SID=$(call SessionService/CreateSession "$SESSION" "{$META,\"title\":\"中文\"}" | field 'd["session"]["id"]')
call SessionService/SendTurn "$SESSION" "{$META,\"sessionId\":\"$SID\",\"text\":\"go\"}" >/dev/null
WAITED=0
while [ "$(call SessionService/ListApprovals "$SESSION" "{\"sessionId\":\"$SID\"}" | field 'len(d.get("approvals", []))' 2>/dev/null || echo 0)" = 0 ]; do
	WAITED=$((WAITED + 1)); [ "$WAITED" -gt 200 ] && fail "the approval cannot be listed: $(call SessionService/ListApprovals "$SESSION" "{\"sessionId\":\"$SID\"}")"
	sleep 0.1
done
call SessionService/ListApprovals "$SESSION" "{\"sessionId\":\"$SID\"}" | python3 -c '
import json, sys
a = json.load(sys.stdin)["approvals"][0]
assert "\ufffd" not in a["summary"] and "…" in a["summary"], a["summary"]
assert "最後這句話只在完整的參數裡" in a["arguments"], a["arguments"]' || fail "the summary of an approval is cut through a character, or the arguments are not whole"
printf 'ok   an approval whose arguments are Chinese is listed, its summary cut at a whole character and its arguments whole\n'

# 19. Sixteen browsers is a bound on what the daemon holds, not on who may use
#     the page: nothing signs a browser out, so each tab ever opened keeps a
#     place for twelve hours. The next one gets in and the longest-standing one
#     goes, instead of the page saying a link was used when it was not.
FIRST=$SESSION
for N in $(seq 1 17); do pair; done
[ "$(status SessionService/ListSessions "$FIRST" '{}')" = 401 ] || fail "the browser that had been in longest is still in after seventeen more"
[ "$(status SessionService/ListSessions "$SESSION" '{}')" = 200 ] || fail "the newest browser was refused"
printf 'ok   a seventeenth browser gets in, and the one that has been in longest is the one that goes\n'

# 20. The reason there is no agent for want of a key is shown on the page and
#     kept in the log. It names the setting and not the file: the name of the
#     file is typed, and the commonest mistake with it is to paste the key.
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true; DAEMON=""
export JINGCLAW_HOME="$WORK/nokey"
mkdir -p "$JINGCLAW_HOME"
unset GEMINI_API_KEY GOOGLE_API_KEY
PASTED="AIzaSyD-FAKEKEY0123456789abcdefghijk_123456"
export JINGCLAW_CONFIG="[provider]
backend = \"gemini\"

[provider.gemini]
api_key_file = \"$PASTED\"

[server]
addr = \"127.0.0.1:7852\"
"
start
STATE=$(call ConfigService/GetAgentState "$LOCAL" '{}')
[ "$(printf '%s' "$STATE" | field 'd["mode"]')" = repair ] || fail "a daemon with no key is not in repair mode: $STATE"
printf '%s' "$STATE" | grep -q 'provider.gemini.api_key_file' || fail "the reason does not name the setting: $STATE"
case "$STATE" in *"$PASTED"*) fail "the reason repeats the name of the file: $STATE" ;; esac
grep -qF -- "$PASTED" "$WORK/daemon.out" "$WORK/daemon.err" && fail "the name of the file is in the daemon's output"
printf 'ok   a missing key is explained by the setting that names its file, never by the name itself\n'

printf '\nall dashboard checks passed\n'
