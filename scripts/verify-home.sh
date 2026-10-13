#!/bin/sh
# Proves where a deployment lives, and that standing somewhere else does not
# move it.
#
# The failure this guards against happened three times in one day. Two
# directories both looked like real deployments; which one answered depended on
# where the daemon was started from; and the settings somebody edited were not
# the settings that ran. Nothing crashed, and nothing said anything was wrong.
#
# So the check is not "does it start". It is: the same deployment, addressed
# the same way, from three different directories, resolves to the same paths.
#
# The settings are in the database, which is part of the same question: a
# deployment that quietly opened a different database would come up with
# different settings and nothing to say so. So the other half is the upgrade,
# where the first start has to find the files an older version kept them in and
# must not start over somewhere else.
set -eu

# Nothing inherited may name a deployment, supply a document or move a place:
# this check is about what is resolved when none of that is said.
for VARIABLE in $(env | sed -n 's/^\(JINGCLAW_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$VARIABLE"
done

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

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

# start NAME RUNDIR runs a daemon with its output in NAME.out and NAME.err, and
# returns once it has published itself in RUNDIR.
start() {
	"$WORK/jingclaw" daemon >"$WORK/$1.out" 2>"$WORK/$1.err" &
	DAEMON=$!
	WAITED=0
	while [ ! -f "$2/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start:
$(cat "$WORK/$1.err")"
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
	python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$DISCOVERY" "$1"
}
call() {
	curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $TOKEN" -d "$2" \
		"$BASE/jingclaw.control.v1.$1"
}
field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))' "$1"; }

# versions DOCUMENT is what has been saved of a document, newest first, as
# number:source pairs.
versions() {
	call ConfigService/ListDocumentVersions "{\"document\":\"SETTINGS_DOCUMENT_$1\"}" |
		field '" ".join(v["version"] + ":" + v["source"] for v in d.get("versions", []))'
}

# mode_of FILE is the permission bits of a file, as octal.
mode_of() { python3 -c 'import os,stat,sys;print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))' "$1"; }

mkdir -p "$WORK/home" "$WORK/project/deep/nested" "$WORK/elsewhere"

# Isolated by moving the user's home, not by naming the deployment.
#
# JINGCLAW_HOME would answer the question this file is asking: it wins over
# everything, so a resolver that still searched the working directory would
# never be consulted and the decoy below would prove nothing. Moving HOME
# leaves the default path in play, which is the path that has to be right.
export HOME="$(cd "$WORK/home" && pwd -P)"

# The physical path, because on macOS the temp directory is reached through a
# symlink and the daemon reports where things actually are.
HOME_DIR="$HOME/.jingclaw"

cd "$WORK/project"
"$WORK/jingclaw" daemon --init >"$WORK/init.out" 2>&1 || fail "--init failed:
$(cat "$WORK/init.out")"

for PLACE in workspace data run; do
	[ -d "$HOME_DIR/$PLACE" ] || fail "--init did not create $PLACE"
done
printf 'ok   --init creates the directory and what goes in it\n'

# The settings are not files. A file named config.toml, or the two that say who
# the agent is, would be one nothing reads, and in the workspace it would be one
# the agent could edit while doing a job and mistake for the thing it was
# meant to be.
no_settings_files() {
	FOUND=$(find "$1" \( -name config.toml -o -name AGENTS.md -o -name PERSONA.md \))
	[ -z "$FOUND" ] || fail "$2 left a settings file nothing reads: $FOUND"
}
no_settings_files "$HOME_DIR" "--init"
printf 'ok   and no settings file, which would be one nothing reads\n'

# It went where the deployment is, not where somebody was standing.
[ -e "$WORK/project/.jingclaw" ] &&
	fail "--init made a directory in the working directory"
printf 'ok   and not in the directory it was run from\n'

# Creating over an existing one is refused: a "create" that adopts whatever was
# there is how a fresh deployment ends up on another one's database.
if "$WORK/jingclaw" daemon --init >/dev/null 2>&1; then
	fail "a second --init was allowed over an existing directory"
fi
printf 'ok   it refuses to create one over another\n'

# The check this file exists for. Directories that look exactly like
# deployments sit above two of the three places we start from, which is the
# arrangement that used to decide which one answered. Each holds settings that
# would stop an agent from running if anything read them, so a daemon that
# did read them, or imported them, is one that cannot be talked to below.
for DECOY in "$WORK/project" "$WORK/project/deep"; do
	mkdir -p "$DECOY/.jingclaw/data" "$DECOY/.jingclaw/run" "$DECOY/.jingclaw/workspace"
	printf '[provider]\nbackend = "not a provider"\n' > "$DECOY/.jingclaw/config.toml"
done

EXPECTED=$(cd "$WORK/project" && "$WORK/jingclaw" daemon --print-paths 2>&1) ||
	fail "--print-paths failed: $EXPECTED"

for FROM in "$WORK/project/deep/nested" "$WORK/elsewhere"; do
	ACTUAL=$(cd "$FROM" && "$WORK/jingclaw" daemon --print-paths 2>&1) ||
		fail "--print-paths failed in $FROM: $ACTUAL"
	[ "$ACTUAL" = "$EXPECTED" ] || fail "starting in $FROM resolved differently:
$ACTUAL

against, from the first directory:
$EXPECTED"
done
printf 'ok   three starting directories, one set of paths\n'

# Every path is inside the deployment, including the workspace: "whatever you
# happened to be standing in" is not a workspace. The settings are said to be in
# the database, which is not a path, and there is no longer a file to name.
cd "$WORK/elsewhere"
REPORTED=$("$WORK/jingclaw" daemon --print-paths 2>&1)
printf '%s\n' "$REPORTED" | while read -r NAME VALUE; do
	[ "$NAME" = settings ] && continue
	case "$VALUE" in
	"$HOME_DIR" | "$HOME_DIR"/*) ;;
	*) printf 'FAIL: %s %s is outside the deployment\n' "$NAME" "$VALUE" >&2; exit 1 ;;
	esac
done || exit 1
printf '%s\n' "$REPORTED" | grep -q '^settings  *in the database' ||
	fail "--print-paths does not say where the settings are: $REPORTED"
printf '%s\n' "$REPORTED" | grep -q '^config ' &&
	fail "--print-paths still names a settings file: $REPORTED"
printf 'ok   and every one of them is inside it\n'

# The daemon agrees with what it printed, and a client elsewhere finds it.
cd "$WORK/project/deep/nested"
start home "$HOME_DIR/run"

LINE=$(grep -o '"msg":"jingclaw daemon listening"[^}]*' "$WORK/home.err" || true)
[ -n "$LINE" ] || fail "the daemon never reported listening"

for FIELD in database discovery workspace; do
	VALUE=$(echo "$LINE" | tr ',' '\n' | grep "\"$FIELD\"" | cut -d'"' -f4)
	[ -n "$VALUE" ] || fail "the daemon did not report $FIELD"
	case "$VALUE" in
	"$HOME_DIR"/*) ;;
	*) fail "$FIELD resolved to $VALUE, outside the deployment" ;;
	esac
done
echo "$LINE" | grep -q '"config_file"' && fail "the daemon still reports a settings file"
echo "$LINE" | tr ',' '\n' | grep '"settings"' | grep -q 'database' ||
	fail "the daemon does not say the settings are in the database"
printf 'ok   the running daemon resolved everything inside it\n'

cd "$WORK/elsewhere"
SESSION=$("$WORK/jingclaw" session create 2>&1 | tr -d '\r\n')
case "$SESSION" in
ses_*) ;;
*) fail "a client started elsewhere could not find the daemon: $SESSION" ;;
esac
printf 'ok   a client started anywhere finds the same daemon\n'

# It started from the settings it made itself, not from the decoys above it.
[ "$(versions CONFIG)" = "1:default" ] || fail "the settings are not the ones it made: $(versions CONFIG)"
no_settings_files "$HOME_DIR" "starting"
printf 'ok   and from the settings it made, not from any beside it\n'

# The database holds the settings, which can hold credentials a server needs,
# next to every conversation. The files SQLite keeps beside it hold the same,
# and exist only while it runs.
for DATABASE in "$HOME_DIR/data/jingclaw.db" "$HOME_DIR/data/jingclaw.db-wal" "$HOME_DIR/data/jingclaw.db-shm"; do
	[ -e "$DATABASE" ] || continue
	[ "$(mode_of "$DATABASE")" = 0o600 ] || fail "$DATABASE is readable by somebody else: $(mode_of "$DATABASE")"
done
printf 'ok   the database is for its owner alone\n'
stop

# A machine that has never had one. Everything the daemon needs has to be
# there afterwards, because there was nothing to prepare it: the workspace
# stopped being a setting an operator points at something existing, so
# starting is what creates it.
UNTOUCHED=$(mktemp -d)
HOME="$UNTOUCHED" "$WORK/jingclaw" daemon >"$WORK/fresh.out" 2>"$WORK/fresh.err" &
FRESH_DAEMON=$!

WAITED=0
while [ ! -f "$UNTOUCHED/.jingclaw/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	if [ "$WAITED" -gt 150 ]; then
		kill "$FRESH_DAEMON" 2>/dev/null
		fail "a machine with no deployment could not start one: $(cat "$WORK/fresh.err")"
	fi
	sleep 0.1
done

for MADE in workspace data/jingclaw.db run; do
	[ -e "$UNTOUCHED/.jingclaw/$MADE" ] ||
		fail "starting on a fresh machine did not make $MADE"
done
no_settings_files "$UNTOUCHED" "starting on a fresh machine"
kill "$FRESH_DAEMON" 2>/dev/null
wait "$FRESH_DAEMON" 2>/dev/null || true

# And nothing outside it. A deployment that scattered anything is one that
# cannot be moved or backed up by copying a directory.
OUTSIDE=$(find "$UNTOUCHED" -maxdepth 1 -mindepth 1 ! -name ".jingclaw" | head -3)
[ -z "$OUTSIDE" ] || fail "it also made things outside the deployment: $OUTSIDE"

rm -rf "$UNTOUCHED"
printf 'ok   a machine that never had one gets everything, and nothing else\n'

# The upgrade. A deployment an older version made keeps its settings in three
# files, and the first start of this one has to bring them in: starting over
# with the defaults beside a deployment that has its own would be settings that
# were edited and are not the settings that ran. From here on a deployment is
# named, which is how a script says "this one" and is the case that still wins.
export JINGCLAW_HOME="$WORK/upgrade"
mkdir -p "$JINGCLAW_HOME/workspace" "$WORK/original"
cat > "$JINGCLAW_HOME/config.toml" <<'EOF'
# The settings an older version kept in a file.
[provider]
backend = "fake"
fake_delay = "0s"

[agent]
max_iterations = 5
EOF
printf '# How this project works\n\nIt is a check.\n' > "$JINGCLAW_HOME/AGENTS.md"
printf '# Who you are\n\n設定檔測試\n' > "$JINGCLAW_HOME/PERSONA.md"
printf 'an old backup\n' > "$JINGCLAW_HOME/config.toml.bak.1"
cp "$JINGCLAW_HOME/config.toml" "$JINGCLAW_HOME/AGENTS.md" "$JINGCLAW_HOME/PERSONA.md" \
	"$JINGCLAW_HOME/config.toml.bak.1" "$WORK/original/"

start upgrade "$JINGCLAW_HOME/run"

grep -q 'imported from config.toml' "$WORK/upgrade.out" || fail "the startup line does not say the settings were imported: $(cat "$WORK/upgrade.out")"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] || fail "a daemon started on imported settings is not running an agent"
[ "$(call ConfigService/GetConfig '{}' | field 'int(d["running"]["agent.max_iterations"])')" = 5 ] || fail "the imported settings are not what is running"
call ConfigService/GetConfig '{}' | python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["text"])' > "$WORK/imported"
cmp -s "$WORK/imported" "$WORK/original/config.toml" || fail "the imported settings are not the file, byte for byte:
$(diff "$WORK/original/config.toml" "$WORK/imported" | head)"
for FILE in AGENTS PERSONA; do
	call ConfigService/GetInstructionFile "{\"file\":\"INSTRUCTION_FILE_$FILE\"}" |
		python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["text"])' > "$WORK/imported"
	cmp -s "$WORK/imported" "$WORK/original/$FILE.md" || fail "the imported $FILE.md is not the file, byte for byte"
done
for DOCUMENT in CONFIG AGENTS PERSONA; do
	[ "$(versions $DOCUMENT)" = "1:file" ] || fail "$DOCUMENT is not one version imported from a file: $(versions $DOCUMENT)"
done
printf 'ok   the first start after an upgrade imports the three files it finds\n'

# Set aside and not deleted: the operator's own files, under a name that
# nothing mistakes for settings. A backup is not ours to touch.
for NAMED in config.toml AGENTS.md PERSONA.md; do
	[ -e "$JINGCLAW_HOME/$NAMED" ] && fail "$NAMED is still where it was, to be mistaken for the settings"
	cmp -s "$JINGCLAW_HOME/$NAMED.imported" "$WORK/original/$NAMED" || fail "$NAMED.imported is not the file it was"
done
cmp -s "$JINGCLAW_HOME/config.toml.bak.1" "$WORK/original/config.toml.bak.1" || fail "a backup was touched"
printf 'ok   and sets each file aside as <name>.imported, without deleting it or touching a backup\n'
stop

# A second start reads nothing from the directory: with the settings in the
# database, a file that turns up beside them is not the settings, whatever it
# says, and is not renamed away either, since renaming it would hide that it
# was there.
printf '[provider]\nbackend = "not a provider"\n' > "$JINGCLAW_HOME/config.toml"
printf 'a stray persona\n' > "$JINGCLAW_HOME/PERSONA.md"
start again "$JINGCLAW_HOME/run"

grep -q 'database (3 documents)' "$WORK/again.out" || fail "a later start does not say where the settings are: $(cat "$WORK/again.out")"
[ "$(call ConfigService/GetAgentState '{}' | field 'd["mode"]')" = running ] || fail "a file beside the settings was read: $(call ConfigService/GetAgentState '{}')"
for DOCUMENT in CONFIG AGENTS PERSONA; do
	[ "$(versions $DOCUMENT)" = "1:file" ] || fail "a later start changed $DOCUMENT: $(versions $DOCUMENT)"
done
[ "$(cat "$JINGCLAW_HOME/config.toml")" = '[provider]
backend = "not a provider"' ] || fail "a file that was not imported was changed"
[ "$(cat "$JINGCLAW_HOME/PERSONA.md")" = 'a stray persona' ] || fail "a file that was not imported was changed"
grep -q 'is not what was imported' "$WORK/again.err" || fail "a file left beside the settings was not mentioned"
printf 'ok   a second start reads nothing from the directory, and says so about a file that turned up\n'
stop

# The check that exists for a deployment whose files said where its database
# was. The settings cannot say that any more, so an import that carried on
# would open the default database, empty, and the sessions in the real one
# would be somewhere nothing looks.
for PLACE in server.data_dir:JINGCLAW_SERVER_DATA_DIR:data-elsewhere server.runtime_dir:JINGCLAW_SERVER_RUNTIME_DIR:run-elsewhere; do
	KEY=${PLACE%%:*}
	REST=${PLACE#*:}
	VARIABLE=${REST%%:*}
	ELSEWHERE="$WORK/${REST#*:}"

	export JINGCLAW_HOME="$WORK/guard-$KEY"
	mkdir -p "$JINGCLAW_HOME/workspace"
	printf '[provider]\nbackend = "fake"\n\n[server]\n%s = "%s"\n' "${KEY#server.}" "$ELSEWHERE" > "$JINGCLAW_HOME/config.toml"

	refused || fail "a deployment whose file said $KEY was started somewhere else: $(cat "$WORK/refusal")"
	grep -q -- "$VARIABLE=$ELSEWHERE" "$WORK/refusal" || fail "the refusal does not say how to give $KEY: $(cat "$WORK/refusal")"
	[ -f "$JINGCLAW_HOME/config.toml" ] || fail "a refused start moved the file"
	[ -e "$JINGCLAW_HOME/config.toml.imported" ] && fail "a refused start set the file aside as imported"
	[ -e "$JINGCLAW_HOME/run/daemon.json" ] && fail "a refused start published itself"

	# And the way through it, which is saying so.
	export "$VARIABLE=$ELSEWHERE"
	case "$KEY" in
	server.data_dir) start given "$JINGCLAW_HOME/run" ;;
	*) start given "$ELSEWHERE" ;;
	esac
	grep -q 'imported from config.toml' "$WORK/given.out" || fail "$KEY given as $VARIABLE did not import the file: $(cat "$WORK/given.out")"
	case "$KEY" in
	server.data_dir) [ -f "$ELSEWHERE/jingclaw.db" ] || fail "the database is not in $ELSEWHERE" ;;
	*) [ -f "$ELSEWHERE/daemon.json" ] || fail "the discovery file is not in $ELSEWHERE" ;;
	esac
	stop
	unset "$VARIABLE"
	printf 'ok   a file that says %s is refused until it is given as %s\n' "$KEY" "$VARIABLE"
done

# The same check for a configuration a variable carries. The variable is saved
# the way a file is, and a line in it for where the database is is ignored the
# same way, so a deployment that keeps its data on a volume and its configuration
# in the service's variables would otherwise open an empty database and say it
# had made its settings from the variable.
for PLACE in server.data_dir:JINGCLAW_SERVER_DATA_DIR:data-by-variable server.runtime_dir:JINGCLAW_SERVER_RUNTIME_DIR:run-by-variable; do
	KEY=${PLACE%%:*}
	REST=${PLACE#*:}
	VARIABLE=${REST%%:*}
	ELSEWHERE="$WORK/${REST#*:}"

	export JINGCLAW_HOME="$WORK/variable-guard-$KEY"
	export JINGCLAW_CONFIG="$(printf '[provider]\nbackend = "fake"\n\n[server]\n%s = "%s"\n' "${KEY#server.}" "$ELSEWHERE")"

	refused || fail "a deployment whose variable said $KEY was started somewhere else: $(cat "$WORK/refusal")"
	grep -q -- "$VARIABLE=$ELSEWHERE" "$WORK/refusal" || fail "the refusal does not say how to give $KEY: $(cat "$WORK/refusal")"
	grep -q JINGCLAW_CONFIG "$WORK/refusal" || fail "the refusal does not say which variable: $(cat "$WORK/refusal")"
	[ -e "$JINGCLAW_HOME/run/daemon.json" ] && fail "a refused start published itself"

	export "$VARIABLE=$ELSEWHERE"
	case "$KEY" in
	server.data_dir) start given "$JINGCLAW_HOME/run" ;;
	*) start given "$ELSEWHERE" ;;
	esac
	grep -q 'created from JINGCLAW_CONFIG' "$WORK/given.out" || fail "$KEY given as $VARIABLE did not save the variable: $(cat "$WORK/given.out")"
	stop
	unset "$VARIABLE" JINGCLAW_CONFIG
	printf 'ok   a variable that says %s is refused until it is given as %s\n' "$KEY" "$VARIABLE"
done

# A trial run with a database of its own is the isolation databasePath offers.
# The files it imports are the real deployment's, so it leaves them where they
# are, and the deployment's own start is the one that takes them.
export JINGCLAW_HOME="$WORK/trial"
mkdir -p "$JINGCLAW_HOME/workspace"
printf '[provider]\nbackend = "fake"\n' > "$JINGCLAW_HOME/config.toml"
printf 'my rules\n' > "$JINGCLAW_HOME/AGENTS.md"
"$WORK/jingclaw" daemon --data-dir "$WORK/trial-scratch" --print-prompt >"$WORK/trial-prompt.out" 2>"$WORK/trial-prompt.err" ||
	fail "the trial run failed: $(cat "$WORK/trial-prompt.err")"
grep -q 'my rules' "$WORK/trial-prompt.out" || fail "the trial did not answer from the deployment's files"
for FILE in config.toml AGENTS.md; do
	[ -f "$JINGCLAW_HOME/$FILE" ] && [ ! -e "$JINGCLAW_HOME/$FILE.imported" ] || fail "a trial run took $FILE into a database nobody opens again"
done
start trial "$JINGCLAW_HOME/run"
grep -q 'imported from config.toml' "$WORK/trial.out" || fail "the deployment's own start did not import the files: $(cat "$WORK/trial.out")"
[ -f "$JINGCLAW_HOME/AGENTS.md.imported" ] && [ ! -e "$JINGCLAW_HOME/AGENTS.md" ] || fail "the deployment's own start did not set the file aside"
[ "$(versions AGENTS)" = "1:file" ] || fail "the instructions are not in the deployment's database: $(versions AGENTS)"
stop
printf 'ok   a trial run in a database of its own leaves the deployment'"'"'s files for its own start\n'

printf 'PASS: one deployment, wherever it is started from\n'
