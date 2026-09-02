#!/bin/sh
# Proves that what a console started dies with the console.
#
# A console that starts a deployment owns it: leaving is supposed to take it
# with you. Typing quit does that today, and so does Ctrl-C. What did not is
# the terminal simply going away — closing the window, or an ssh session
# dropping — because the supervisor listened for interrupt and terminate and
# not for the hangup a terminal sends, and Go's own answer to a hangup is to
# die without running anything deferred.
#
# What that leaves is worse than nothing running: a daemon holding the port
# and the database with no console that can stop it, and a `status` that says
# "running" to whoever looks next.
set -eu

cd "$(dirname "$0")/../core"

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

if [ "$(uname -s)" = "Windows_NT" ] || [ -n "${WINDIR:-}" ]; then
	printf 'skipped: a controlling terminal and its hangup are a Unix arrangement\n'
	exit 0
fi

WORK=$(mktemp -d)
export JINGCLAW_HOME="$WORK"

cleanup() {
	set +e
	pkill -f "$WORK/jingclaw" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

go build -o "$WORK/jingclaw" ./cmd/jingclaw

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"
cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
[server]
addr = "127.0.0.1:7843"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
[gateway]
platform = "telegram"
[gateway.telegram]
account_id = "main"
api_base = "http://127.0.0.1:7844"
EOF

# Drives the supervisor under a pty and then takes the pty away, which is what
# closing a terminal window and losing an ssh session both come down to.
cat > "$WORK/leave.py" <<'DRIVE'
import fcntl, os, pty, select, struct, subprocess, sys, termios, time

work, how = sys.argv[1], sys.argv[2]
binary = work + "/jingclaw"

pid, fd = pty.fork()
if pid == 0:
    os.environ["JINGCLAW_HOME"] = work
    os.execvp(binary, [binary, "--config", work + "/config.toml"])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 100, 0, 0))

seen = bytearray()
deadline = time.time() + 30
while time.time() < deadline:
    ready, _, _ = select.select([fd], [], [], 0.5)
    if ready:
        try:
            seen.extend(os.read(fd, 65536))
        except OSError:
            break
    if b"JingClaw." in seen:
        break

# Let the parts settle before taking the terminal away, or this checks that a
# deployment which had not finished starting does not finish starting.
time.sleep(4)


def running():
    out = subprocess.run(["ps", "-eo", "pid,ppid,command"],
                         capture_output=True, text=True).stdout
    return [line.strip() for line in out.splitlines() if binary in line]


# There has to be something to lose. Every check below passes on an empty
# process table, so without this they would all go green on a deployment that
# had already fallen over for its own reasons.
before = running()
if len(before) < 2:
    print("NOTHING-WAS-RUNNING", len(before))
    raise SystemExit(0)

if how == "close":
    os.close(fd)
elif how == "quit":
    os.write(fd, b"quit\r")
elif how == "kill":
    # Nothing the supervisor runs can help here. Whatever cleans up has to
    # already be running and has to notice on its own.
    os.kill(pid, 9)
elif how == "killgroup":
    # The whole foreground group, which is what a hard close of a terminal
    # and a job-control kill both come to. Whatever cleans up has to be
    # outside that group as well as still running.
    os.killpg(os.getpgid(pid), 9)

time.sleep(6)

for line in running():
    print(line)
DRIVE

printf '%s' "checking that quitting takes it with you... "
LEFT=$(python3 "$WORK/leave.py" "$WORK" quit)
case "$LEFT" in *NOTHING-WAS-RUNNING*) fail "nothing was running to lose: $LEFT" ;; esac
[ -z "$LEFT" ] || fail "quit left these behind:
$LEFT"
printf 'ok\n'

pkill -f "$WORK/jingclaw" 2>/dev/null || true
sleep 1

printf '%s' "checking that closing the terminal takes it with you... "
LEFT=$(python3 "$WORK/leave.py" "$WORK" close)
case "$LEFT" in *NOTHING-WAS-RUNNING*) fail "nothing was running to lose: $LEFT" ;; esac
[ -z "$LEFT" ] || fail "closing the terminal left these behind, orphaned and still holding the port:
$LEFT"
printf 'ok\n'

pkill -f "$WORK/jingclaw" 2>/dev/null || true
sleep 1

printf '%s' "checking that killing the console outright takes it with you... "
LEFT=$(python3 "$WORK/leave.py" "$WORK" kill)
case "$LEFT" in *NOTHING-WAS-RUNNING*) fail "nothing was running to lose: $LEFT" ;; esac
[ -z "$LEFT" ] || fail "the console was killed and these kept running:
$LEFT"
printf 'ok\n'

pkill -f "$WORK/jingclaw" 2>/dev/null || true
sleep 1

printf '%s' "checking that killing the whole foreground group takes it with you... "
LEFT=$(python3 "$WORK/leave.py" "$WORK" killgroup)
case "$LEFT" in *NOTHING-WAS-RUNNING*) fail "nothing was running to lose: $LEFT" ;; esac
[ -z "$LEFT" ] || fail "the console's whole group was killed and these kept running:
$LEFT"
printf 'ok\n'

printf '\nall checks passed\n'
