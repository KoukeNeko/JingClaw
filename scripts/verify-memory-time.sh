#!/bin/sh
# Proves memory keeps two timelines and retires what nobody wants.
#
# Record time is when this agent learned something and when it stopped
# believing it. Valid time is when the thing was true in the world. They come
# apart constantly, and a store with one has to pick which date to lose.
#
# And separately: a fact nobody has wanted in months is probably not wrong, it
# is probably noise — but retiring it must not look like a correction, because
# "somebody replaced this" and "nobody wanted this" lead somewhere different.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"
cat > "$WORK/config.toml" <<EOF
[model]
provider = "fake"
model = "fake-echo"
fake_delay = "0s"

[[model.fake_script]]
text = "Writing that down."
tool = "remember"
args = '{"text":"the deploy freeze is on","valid_until":"2099-01-01"}'

[[model.fake_script]]
text = "And this one."
tool = "remember"
args = '{"text":"the release branch is cut on Fridays"}'

[[model.fake_script]]
text = "Noted."

[memory]
enabled = true
retrieval_ttl = "720h"
[workspace]
root = "$WORK/ws"
[server]
addr = "127.0.0.1:7830"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

A() { "$WORK/agent" --config "$WORK/config.toml" "$@"; }

SESSION=$(A session create | tr -d '\r\n')
A send "$SESSION" "remember these two things" >/dev/null
sleep 2

LISTED=$(A memory list 2>&1)
printf '%s' "$LISTED" | grep -q 'deploy freeze' ||
	fail "nothing was remembered: $LISTED"
printf 'ok   two memories are written\n'

# 1. A fact with a known end says so, and it is not the same field as being
#    corrected.
printf '%s' "$LISTED" | grep -q 'in force through 2099-01-01' ||
	fail "the validity window is not shown: $LISTED"
printf 'ok   one carries the date it stops being true\n'

# 2. A retrieval memory carries an expiry; nothing is expired yet.
printf '%s' "$LISTED" | grep -q 'expired' &&
	fail "something expired the moment it was written: $LISTED"
printf 'ok   and nothing has expired yet\n'

# 3. The two timelines are separate columns, not one.
python3 - "$WORK/data/jingclaw.db" <<'CHECK'
import sqlite3, sys

db = sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True)
rows = db.execute(
    'SELECT text, created_at, valid_from, valid_until, expires_at, invalidated_at '
    'FROM memories ORDER BY created_at').fetchall()


def fail(why):
    print('FAIL: ' + why, file=sys.stderr)
    raise SystemExit(1)


if len(rows) != 2:
    fail('%d memories stored, want 2' % len(rows))

freeze = next((r for r in rows if 'freeze' in r[0]), None)
branch = next((r for r in rows if 'release branch' in r[0]), None)
if freeze is None or branch is None:
    fail('the memories are not the ones that were written: %r' % (rows,))

# Valid time is recorded, and separately from record time.
if freeze[3] is None:
    fail('a fact with a stated end has no valid_until')
if branch[3] is not None:
    fail('a fact with no stated end was given one')
if freeze[2] == 0:
    fail('valid_from was not recorded')

# Record time says nothing has been retracted.
if freeze[5] is not None or branch[5] is not None:
    fail('something was invalidated at birth')

# Retrieval memories carry an expiry; it is record hygiene, not truth.
for row in (freeze, branch):
    if row[4] is None:
        fail('a retrieval memory has no expiry: %r' % (row[0],))
    if row[4] <= row[1]:
        fail('an expiry is not in the future: %r' % (row[0],))
CHECK
printf 'ok   valid time and record time are separate columns\n'

# 4. Reaching an expiry retires without deleting, and is told apart from a
#    correction. Moved forward by rewriting the row, which is what a month of
#    waiting would otherwise cost this check.
python3 - "$WORK/data/jingclaw.db" <<'AGE'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("UPDATE memories SET expires_at = 1 WHERE text LIKE '%release branch%'")
db.commit()
AGE

kill "$DAEMON"
wait "$DAEMON" 2>/dev/null || true
"$WORK/agentd" --config "$WORK/config.toml" >>"$WORK/daemon.out" 2>>"$WORK/daemon.err" &
DAEMON=$!
WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not come back"
	sleep 0.1
done

grep -q '"msg":"memories expired"' "$WORK/daemon.err" ||
	fail "the daemon did not sweep what had expired:
$(grep -i memor "$WORK/daemon.err" | tail -3)"
printf 'ok   what nobody wanted is retired at startup\n'

CURRENT=$(A memory list 2>&1)
printf '%s' "$CURRENT" | grep -q 'release branch' &&
	fail "an expired memory is still believed: $CURRENT"
printf 'ok   and stops being believed\n'

HISTORY=$(A memory list --history 2>&1)
printf '%s' "$HISTORY" | grep -q 'release branch' ||
	fail "an expired memory was deleted rather than retired: $HISTORY"
printf 'ok   without being deleted\n'

# 5. Retired and corrected must not read the same. One of them is worth
#    looking into and the other is housekeeping.
printf '%s' "$HISTORY" | grep -q 'expired, unused' ||
	fail "an expired memory is not told apart from a corrected one:
$HISTORY"
printf '%s' "$HISTORY" | grep -q 'superseded by' &&
	fail "an expired memory is reported as superseded by something"
printf 'ok   and is told apart from one that was corrected\n'

printf '\nPASS: memory keeps both timelines, and retires what nobody wants\n'
