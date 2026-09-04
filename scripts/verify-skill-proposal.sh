#!/bin/sh
# Proves an agent can propose a skill and that a person still decides.
#
# The whole point of the split is that staging is cheap and activating is not.
# So the checks are about the seam between them: a staged skill steers nothing
# and is not installed; activating it stops for an approval rather than running;
# and only a person's yes puts the instructions in front of the model.
#
# It drives the real installer against a repository served over git://, because
# what a unit test cannot see is whether the two tools, the permission engine,
# and the approval a person answers actually line up end to end.
set -eu

cd "$(dirname "$0")/../core"

command -v git >/dev/null 2>&1 || {
	printf 'skipped: no git here, and this fetches a skill from one\n'
	exit 0
}

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK/home"
SKILLS="$JINGCLAW_HOME/skills"

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
GITD=""
cleanup() {
	# Best effort from here. Under set -e a failing kill would end this
	# function where it stands and leave a daemon or a git server holding a
	# port the next run wants.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$GITD" ] && kill "$GITD" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

git_quiet() {
	git -c init.defaultBranch=main \
		-c user.name=t -c user.email=t@example.com "$@"
}

# A repository holding a skill, served over git:// — the tools refuse a local
# path on purpose, because a source has to be one an install can be repeated
# from.
SRC="$WORK/src"
mkdir -p "$SRC/release"
cat > "$SRC/release/SKILL.md" <<'SKILL'
---
name: proposed
description: A skill the agent fetched and a person approved.
---

Do the proposed thing carefully.
SKILL
git_quiet init --quiet "$SRC" >/dev/null 2>&1 || {
	printf 'skipped: git is not usable here\n'
	exit 0
}
git_quiet -C "$SRC" add -A
git_quiet -C "$SRC" commit --quiet -m "a skill"
COMMIT=$(git_quiet -C "$SRC" rev-parse HEAD)

SERVE="$WORK/serve"
mkdir -p "$SERVE"
git_quiet clone --bare --quiet "$SRC" "$SERVE/repo.git"
# Fetching one commit by its hash is what the installer does; the server has to
# allow it. The commit is a ref tip, so reachable-only is enough.
git_quiet -C "$SERVE/repo.git" config uploadpack.allowReachableSHA1InWant true

PORT=$((20000 + $$ % 20000))
git daemon --reuseaddr --export-all --listen=127.0.0.1 --port="$PORT" \
	--base-path="$SERVE" "$SERVE" >/dev/null 2>&1 &
GITD=$!

SOURCE="git:git://127.0.0.1:$PORT/repo.git#$COMMIT:release"

WAITED=0
until git ls-remote "git://127.0.0.1:$PORT/repo.git" >/dev/null 2>&1; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && {
		printf 'skipped: git daemon would not serve on 127.0.0.1:%s\n' "$PORT"
		exit 0
	}
	sleep 0.1
done

# The model, scripted: fetch the skill, then install it, then say it is done.
# The second call is the one that has to stop for a person.
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Fetching the skill."
tool = "skill_stage"
args = '{"source":"$SOURCE"}'

[[provider.fake_script]]
text = "Installing it."
tool = "skill_activate"
args = '{"name":"proposed"}'

[[provider.fake_script]]
text = "Installed."

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

BASE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["base_url"])' "$WORK/run/daemon.json")
TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$WORK/run/daemon.json")
DB="$WORK/data/jingclaw.db"

call() {
	curl -s -X POST -H 'content-type: application/json' \
		-H "authorization: Bearer $TOKEN" -d "$2" \
		"$BASE/jingclaw.control.v1.SessionService/$1"
}

query() { sqlite3 -readonly "$DB" "$1"; }

wait_for() {
	WAITED=0
	until [ "$(query "$1")" = "1" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 300 ] && fail "$2: $(tail -8 "$WORK/daemon.err")"
		sleep 0.1
	done
}

SESSION=$(call CreateSession '{"title":"proposing a skill"}' |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
call SendTurn "{\"sessionId\":\"$SESSION\",\"text\":\"install the proposed skill\"}" >/dev/null

# 1. Staging fetched it, and it is not installed.
wait_for "select 1 from events where kind='tool.completed'
	and json_extract(payload,'\$.name')='skill_stage' limit 1;" \
	"the skill was never staged"

[ -f "$SKILLS/.staged/proposed/SKILL.md" ] ||
	fail "staging did not leave the skill where it waits for approval"
[ ! -e "$SKILLS/proposed" ] ||
	fail "staging installed the skill outright, without anybody deciding"
printf 'ok   staging fetches the skill and does not install it\n'

# 2. Activating stops for a person, and shows them the real thing.
wait_for "select 1 from events where kind='approval.requested'
	and json_extract(payload,'\$.tool_name')='skill_activate' limit 1;" \
	"activating did not ask anybody"

[ ! -e "$SKILLS/proposed" ] ||
	fail "the skill was installed before the approval was answered"

PREVIEW=$(query "select json_extract(payload,'\$.preview') from events
	where kind='approval.requested'
	and json_extract(payload,'\$.tool_name')='skill_activate' limit 1;")
printf '%s' "$PREVIEW" | grep -q "$COMMIT" ||
	fail "the approval does not show the exact commit being installed"
printf '%s' "$PREVIEW" | grep -q "standing instructions" ||
	fail "the approval does not say the skill becomes standing instructions"
printf 'ok   activating stops for a person and shows the commit and the reach\n'

# 3. A person's yes is what installs it.
APPROVAL=$(query "select json_extract(payload,'\$.approval_id') from events
	where kind='approval.requested'
	and json_extract(payload,'\$.tool_name')='skill_activate' limit 1;")
call DecideApproval \
	"{\"approvalId\":\"$APPROVAL\",\"decision\":\"APPROVAL_DECISION_ALLOW\"}" >/dev/null

WAITED=0
while [ ! -f "$SKILLS/proposed/SKILL.md" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "approving did not install the skill: $(tail -8 "$WORK/daemon.err")"
	sleep 0.1
done
[ ! -e "$SKILLS/.staged/proposed" ] ||
	fail "the skill was installed but left behind in staging"

"$WORK/jingclaw" skills list 2>/dev/null | grep -q "proposed" ||
	fail "the installed skill is not in the catalogue"
printf 'ok   approving it installs it, and the catalogue then has it\n'

printf '\nall checks passed\n'
