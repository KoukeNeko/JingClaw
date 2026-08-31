#!/bin/sh
# Proves a run nobody asked for cannot do what nobody agreed to.
#
# A schedule fires while its owner is asleep. Everything below is about that
# one sentence: the run acts as the schedule and not as the person who set it
# up, it is refused the things a person would have been asked about rather
# than parking to wait for them, and one occasion becomes one run however many
# times the daemon is restarted.
#
# The schedule here is scripted to reach for a write, because a check that
# scheduled a well-behaved question would prove only that a well-behaved
# question is harmless.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

export JINGCLAW_HOME="$WORK"
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

# Reaches for a write, then answers. A scheduled run must be refused the
# first and still produce the second.
[[provider.fake_script]]
text = "Writing it down."
tool = "write_file"
args = '{"path":"taken.md","content":"should have been refused"}'

[[provider.fake_script]]
text = "I could not write that."

[server]
addr = "127.0.0.1:7821"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

start_daemon() {
	"$WORK/jingclaw" daemon --config "$WORK/config.toml" >>"$WORK/daemon.out" 2>>"$WORK/daemon.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(tail -5 "$WORK/daemon.err")"
		sleep 0.1
	done
}

stop_daemon() {
	set +e
	[ -n "$DAEMON" ] && { kill "$DAEMON" 2>/dev/null; wait "$DAEMON" 2>/dev/null; }
	set -e
	DAEMON=""
	rm -f "$WORK/run/daemon.json"
}

start_daemon

# 1. What somebody types is checked while they are still looking at it.
"$WORK/jingclaw" schedule add "0 9 30 2 *" "never" --config "$WORK/config.toml" >/dev/null 2>&1 &&
	fail "a schedule that can never run was accepted"
"$WORK/jingclaw" schedule add "0 25 * * *" "bad hour" --config "$WORK/config.toml" 2>"$WORK/err.txt" &&
	fail "an impossible hour was accepted"
grep -q 'hour' "$WORK/err.txt" ||
	fail "the error does not say which field is wrong: $(cat "$WORK/err.txt")"
printf 'ok   an expression that cannot run is refused, by field\n'

# 2. A schedule due every minute fires, and says it was the schedule acting.
SCHEDULE=$("$WORK/jingclaw" schedule add "* * * * *" "what changed" \
	--config "$WORK/config.toml" 2>/dev/null)
[ -n "$SCHEDULE" ] || fail "nothing was created"

WAITED=0
while : ; do
	RUNS=$(python3 - "$WORK/data/jingclaw.db" <<'CHECK'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
print(db.execute("select count(*) from runs where origin like '%schedule%'").fetchone()[0])
CHECK
)
	[ "$RUNS" -ge 1 ] && break
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "the schedule never fired: $(tail -5 "$WORK/daemon.err")"
	sleep 0.2
done
printf 'ok   a schedule that is due starts a run\n'

# 3. Everything else is in the database.
python3 - "$WORK/data/jingclaw.db" "$SCHEDULE" <<'CHECK' || exit 1
import json, sqlite3, sys, time

db = sqlite3.connect(sys.argv[1])
db.row_factory = sqlite3.Row
schedule_id = sys.argv[2]


def fail(said):
    print(f"FAIL: {said}", file=sys.stderr)
    raise SystemExit(1)


runs = db.execute("select id, origin, session_id, status from runs").fetchall()
scheduled = [r for r in runs if json.loads(r["origin"]).get("kind") == "schedule"]
if len(scheduled) != 1:
    fail(f"want one scheduled run, got {len(scheduled)}")

run = scheduled[0]
origin = json.loads(run["origin"])

# The schedule is who acts. Creating one is delegation; running one is not
# the person who set it up still acting while they are asleep.
if origin.get("client_id") != schedule_id:
    fail(f"the run does not act as the schedule: {origin}")
if origin.get("principal"):
    fail(f"the run carries a person as its principal: {origin}")
print("ok   and the run acts as the schedule, not as whoever created it")

# Wait for it to settle, so the tool result below is there.
for _ in range(300):
    status = db.execute("select status from runs where id = ?", (run["id"],)).fetchone()[0]
    if status in ("completed", "failed", "cancelled", "awaiting_approval"):
        break
    time.sleep(0.1)

if status == "awaiting_approval":
    fail("a scheduled run parked waiting for a person who is not there")
print("ok   and never parks waiting for somebody who is not there")

calls = [
    json.loads(row["payload"])
    for row in db.execute(
        "select payload from events where run_id = ? and kind = 'tool.completed'",
        (run["id"],),
    )
]
writes = [c for c in calls if c.get("name") == "write_file"]
if not writes:
    fail(f"the write never reached the runtime, so this proves nothing: {calls}")
if not writes[0].get("is_error"):
    fail(f"a scheduled run wrote a file: {writes[0]}")
print("ok   and is refused what a person would have been asked about")

waiting = db.execute("select count(*) as n from approvals where run_id = ?", (run["id"],)).fetchone()
if waiting["n"]:
    fail(f"a scheduled run asked somebody for permission {waiting['n']} times")
print("ok   without asking anybody, there being nobody to ask")

# One occasion, one row, and it names the run it became.
firings = db.execute(
    "select * from schedule_firings where schedule_id = ?", (schedule_id,)
).fetchall()
if len(firings) != 1:
    fail(f"want one firing, got {len(firings)}: {[dict(f) for f in firings]}")
if firings[0]["run_id"] != run["id"]:
    fail(f"the firing does not name its run: {dict(firings[0])}")

# When it was due and when anybody noticed are different facts.
if firings[0]["due_at"] > firings[0]["observed_at"]:
    fail(f"it was observed before it was due: {dict(firings[0])}")
print("ok   the occasion is recorded once, and names the run it became")
CHECK

[ -f "$WORK/workspace/taken.md" ] && fail "the file was written after all"
printf 'ok   and nothing was written\n'

# 4. Restarting must not run the same occasion again.
BEFORE=$(python3 -c "
import sqlite3,sys
db=sqlite3.connect('$WORK/data/jingclaw.db')
print(db.execute(\"select count(*) from runs where origin like '%schedule%'\").fetchone()[0])")

stop_daemon
start_daemon
sleep 2

AFTER=$(python3 -c "
import sqlite3,sys
db=sqlite3.connect('$WORK/data/jingclaw.db')
print(db.execute(\"select count(*) from runs where origin like '%schedule%'\").fetchone()[0])")

[ "$BEFORE" = "$AFTER" ] ||
	fail "restarting ran the same occasion again: $BEFORE became $AFTER"
printf 'ok   restarting does not run an occasion that was already accounted for\n'

# 5. Pausing and listing.
"$WORK/jingclaw" schedule pause "$SCHEDULE" --config "$WORK/config.toml" >/dev/null 2>&1 ||
	fail "pausing failed"

LISTED=$("$WORK/jingclaw" schedule list --config "$WORK/config.toml" 2>&1)
printf '%s' "$LISTED" | grep -q "$SCHEDULE" ||
	fail "a paused schedule vanished from the listing: $LISTED"
printf '%s' "$LISTED" | grep -q 'paused' ||
	fail "the listing does not say it is paused: $LISTED"
printf 'ok   a paused schedule is still listed, and says so\n'

"$WORK/jingclaw" schedule remove "$SCHEDULE" --config "$WORK/config.toml" >/dev/null 2>&1 ||
	fail "removing it failed"
LISTED=$("$WORK/jingclaw" schedule list --config "$WORK/config.toml" 2>&1)
printf '%s' "$LISTED" | grep -q "$SCHEDULE" &&
	fail "it survived removal: $LISTED"
printf 'ok   and removing one forgets it\n'

printf '\nall checks passed\n'
