#!/bin/sh
# Proves a deployment can bring its files in when a variable is the only way in.
#
# Three of the things JingClaw reads are files in a directory — the settings,
# the persona, the standing instructions — and a platform whose inputs are a
# volume and an environment has no way to put a file in a directory before the
# first start. The unit tests cover the writing. What they cannot see is
# whether the daemon calls it, whether it calls it before the example file is
# written (an example landing first would make every variable permanently
# ignored, and quietly), and whether a document survives the round trip
# through an environment with its line breaks intact.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

cleanup() {
	# Best effort from here. Under set -e a failing kill would end this
	# function where it stands and leave the work directory behind.
	set +e
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# Long enough to write the files and open the database, short enough that a
# daemon that never comes up is a failed check rather than a hung one.
start_briefly() {
	"$WORK/jingclaw" daemon >"$WORK/log" 2>&1 &
	sleep 5
	kill $! 2>/dev/null
	wait 2>/dev/null
	return 0
}

PERSONA='# Who you are

Answer briefly.'

HOME_DIR="$WORK/deployment"
export JINGCLAW_HOME="$HOME_DIR"
export JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"
'
export JINGCLAW_PERSONA="base64:$(printf '%s' "$PERSONA" | base64 | tr -d '\n')"
export JINGCLAW_AGENTS='# How this project works

It is a check.'

start_briefly

for name in config.toml PERSONA.md AGENTS.md; do
	[ -f "$HOME_DIR/$name" ] || fail "$name was not written from the environment"
done

# The whole point of the encoding: a document arrives as a document.
[ "$(cat "$HOME_DIR/PERSONA.md")" = "$PERSONA" ] ||
	fail "the persona lost something on the way through the environment"

grep -q 'fake-echo' "$HOME_DIR/config.toml" ||
	fail "config.toml is the example rather than the one the deployment supplied"

grep -q 'created from JINGCLAW_CONFIG' "$WORK/log" ||
	fail "the startup line calls a supplied configuration a default"

# The variables arrive again on every restart. Overwriting would discard
# whatever was edited in the volume, on the restart after somebody edited it.
printf '# Edited in the volume\n' > "$HOME_DIR/PERSONA.md"
start_briefly
[ "$(cat "$HOME_DIR/PERSONA.md")" = "# Edited in the volume" ] ||
	fail "a restart overwrote a file somebody had edited"

# A value that announces an encoding and then does not decode is line noise,
# and line noise written into a persona is a deployment nobody looks at again.
JINGCLAW_HOME="$WORK/refused" \
	JINGCLAW_PERSONA='base64:not base64 at all !!!' \
	"$WORK/jingclaw" daemon >"$WORK/refusal" 2>&1 &&
	fail "an undecodable value started the daemon anyway"

grep -q 'does not decode' "$WORK/refusal" ||
	fail "the refusal does not say what was wrong with the value"

[ -e "$WORK/refused" ] && fail "a refused value still left a deployment behind"

printf 'PASS: the files a deployment brings arrive, once, intact\n'
