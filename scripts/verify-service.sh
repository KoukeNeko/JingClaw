#!/bin/sh
# Proves the plist a service install would write runs a copy of this executable
# kept under the deployment — never this file where it stands — and names this
# deployment, and a PATH with the tools on it.
#
# What this cannot prove is that launchd then runs it. Loading a job would
# change the login session of whoever runs this check — starting a second
# daemon on their own database, or replacing a service they already had — and
# a check that does that is worse than a gap. The loading itself is two
# launchctl calls and has been done by hand.
#
# Nor can it prove the PATH is the one a terminal has rather than the one a
# login shell alone would give: the difference is whichever directories a
# particular machine adds in .zshrc, and there is nothing portable to assert.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

# A deployment of this check's own, so it cannot reach the operator's: reading
# their settings would be bad and writing to their database would be worse.
# Where the agent may read and write is this directory's workspace, which is
# why the check has to have one rather than simply having none.
export JINGCLAW_HOME="$WORK"
cleanup() {
	set +e
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# A plist is launchd's, and launchd is macOS's. Elsewhere there is nothing to
# write one for, and the command says so rather than writing one nothing
# reads. Skipped with a reason: a check that quietly passed on a machine that
# could not run it is worse than one that says it did not.
if [ "$(uname -s)" != "Darwin" ]; then
	printf 'skipped: launchd is macOS, and a plist is what launchd reads\n'
	exit 0
fi

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DEPLOYMENT="$WORK/.jingclaw"
mkdir -p "$DEPLOYMENT"

# --print is the whole reason this check can exist: it produces exactly what
# install would write, and writes it nowhere.
JINGCLAW_HOME="$DEPLOYMENT" "$WORK/jingclaw" service install --print >"$WORK/plist" 2>"$WORK/err" ||
	fail "printing the plist failed: $(cat "$WORK/err")"

plutil -lint "$WORK/plist" >/dev/null 2>&1 ||
	fail "launchd could not parse it: $(plutil -lint "$WORK/plist" 2>&1)"
printf 'ok   the plist is one launchd can parse\n'

# The service runs a copy under the deployment, not this file where it stands.
# launchd cannot open a program inside a folder macOS protects, and a checkout
# is usually in one: the service hangs in the loader before main, reported as
# running, with nothing written anywhere. Naming this file was the defect.
grep -q "<string>$DEPLOYMENT/bin/jingclaw</string>" "$WORK/plist" ||
	fail "it does not run the copy under the deployment: $(cat "$WORK/plist")"
grep -q "<string>$WORK/jingclaw</string>" "$WORK/plist" &&
	fail "it still runs the executable where it stands, which launchd cannot open from a protected folder"
printf 'ok   it runs a copy kept under the deployment, not this file\n'

# Printing is a dry run. A dry run that leaves a copy behind is an install.
[ -e "$DEPLOYMENT/bin/jingclaw" ] &&
	fail "printing the plist copied the program into the deployment"
printf 'ok   and printing it copied nothing\n'

grep -q "<string>$DEPLOYMENT</string>" "$WORK/plist" ||
	fail "it does not name this deployment: $(cat "$WORK/plist")"
printf 'ok   it names the deployment it was printed for\n'

# A service does not inherit the shell's environment. Without PATH written in,
# every tool the agent runs by name is missing, and only once nobody is
# watching.
PATH_IN_PLIST=$(python3 - "$WORK/plist" <<'READ'
import plistlib, sys

with open(sys.argv[1], "rb") as file:
    print(plistlib.load(file)["EnvironmentVariables"].get("PATH", ""))
READ
)
[ -n "$PATH_IN_PLIST" ] || fail "no PATH was written, so the service would find no tools"

for TOOL in git python3; do
	FOUND=""
	for DIR in $(echo "$PATH_IN_PLIST" | tr ':' ' '); do
		[ -x "$DIR/$TOOL" ] && FOUND="$DIR" && break
	done
	[ -n "$FOUND" ] || fail "$TOOL is not on the PATH the service would run with: $PATH_IN_PLIST"
done
printf 'ok   the PATH it records has the tools the agent runs\n'

# The session directories of whatever started this are not a place that will
# still exist tomorrow, and a service that found a tool there today would lose
# it without saying so.
case "$PATH_IN_PLIST" in
*local-agent-mode-sessions*) fail "the PATH carries directories of the process that installed it" ;;
esac
printf 'ok   and not the directories of whatever ran the install\n'

python3 - "$WORK/plist" <<'CHECK' || exit 1
import plistlib, sys

with open(sys.argv[1], "rb") as file:
    job = plistlib.load(file)

for key in ("RunAtLoad", "KeepAlive"):
    if job.get(key) is not True:
        print(f"FAIL: {key} is {job.get(key)!r}, so the service would not come back", file=sys.stderr)
        raise SystemExit(1)

for key in ("StandardOutPath", "StandardErrorPath"):
    if not job.get(key, "").endswith((".out", ".err")):
        print(f"FAIL: {key} is {job.get(key)!r}", file=sys.stderr)
        raise SystemExit(1)
CHECK
printf 'ok   it restarts by itself and writes what it says down\n'

printf '\nall checks passed\n'
