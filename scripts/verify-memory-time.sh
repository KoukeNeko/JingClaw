#!/bin/sh
# Proves memory keeps two timelines.
#
# Record time is when this agent learned something and when it stopped
# believing it. Valid time is when the thing was true in the world. They come
# apart constantly — "the API was v1 until March", learned in June, is a
# correction made in June about a change that happened in March — and a store
# with one timeline has to pick which of those dates to lose.
#
# Nothing here expires by being unused. That mechanism existed and was
# removed: it retired exactly the memories that were not the problem.
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
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[[provider.fake_script]]
text = "Writing that down."
tool = "remember"
args = '{"text":"the deploy freeze is on","valid_until":"2099-01-01"}'

[[provider.fake_script]]
text = "And this one."
tool = "remember"
args = '{"text":"the release branch is cut on Fridays"}'

[[provider.fake_script]]
text = "Noted."

[memory]
enabled = true
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

# 2. Nothing carries an expiry at all. A memory is ended by a correction, by
#    an end date it was given, or by somebody removing it — never by nobody
#    having asked for it.
printf '%s' "$LISTED" | grep -q 'expired' &&
	fail "something expired the moment it was written: $LISTED"
printf 'ok   and nothing expires for being unused\n'

# 3. The two timelines are separate columns, not one.
python3 - "$WORK/data/jingclaw.db" <<'CHECK'
import sqlite3, sys

db = sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True)
rows = db.execute(
    'SELECT text, created_at, valid_from, valid_until, invalidated_at '
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
if freeze[4] is not None or branch[4] is not None:
    fail('something was invalidated at birth')
CHECK
printf 'ok   valid time and record time are separate columns\n'

# 4. A correction records both facts: when this agent stopped carrying the old
#    memory, and when the thing it described stopped being true. Those are
#    rarely the same moment, and losing either loses an answer.
python3 - "$WORK/data/jingclaw.db" <<'CHECK'
import sqlite3, sys, time

db = sqlite3.connect(sys.argv[1])
rows = db.execute(
    "SELECT id FROM memories WHERE text LIKE '%release branch%'").fetchall()
if not rows:
    print('FAIL: the memory to correct is missing', file=sys.stderr)
    raise SystemExit(1)

old_id = rows[0][0]
now = int(time.time()) * 1_000_000_000
day = 86400 * 1_000_000_000

# A coherent history: written a week ago, the world changed two days ago, and
# we found out just now. Three different moments, which is the point.
written = now - 7 * day
changed = now - 2 * day
learned = now - 60 * 1_000_000_000

db.execute("UPDATE memories SET created_at = ?, valid_from = ? WHERE id = ?",
           (written, written, old_id))
db.execute(
    "INSERT INTO memories (id, scope, scope_ref, activation, text, trust, "
    "origin_kind, origin_client, origin_platform, origin_principal, "
    "source_session, source_seq, approved_by, created_at, valid_from) "
    "SELECT 'mem_corrected', scope, scope_ref, activation, "
    "'the release branch is cut on Tuesdays', trust, origin_kind, origin_client, "
    "origin_platform, origin_principal, source_session, source_seq, approved_by, "
    "?, ? FROM memories WHERE id = ?", (learned, changed, old_id))
db.execute(
    "UPDATE memories SET invalidated_at = ?, superseded_by = 'mem_corrected', "
    "valid_until = COALESCE(valid_until, ?) WHERE id = ?",
    (learned, changed, old_id))
db.commit()

row = db.execute(
    "SELECT invalidated_at, valid_until FROM memories WHERE id = ?", (old_id,)).fetchone()
if row[0] != learned:
    print('FAIL: the moment it stopped being carried was not recorded', file=sys.stderr)
    raise SystemExit(1)
if row[1] != changed:
    print('FAIL: the moment it stopped being true was not recorded', file=sys.stderr)
    raise SystemExit(1)
if row[0] == row[1]:
    print('FAIL: the two timelines collapsed into one moment', file=sys.stderr)
    raise SystemExit(1)
CHECK
printf 'ok   a correction records when it stopped being carried\n'
printf 'ok   and separately when it stopped being true\n'

CURRENT=$(A memory list 2>&1)
printf '%s' "$CURRENT" | grep -q 'cut on Fridays' &&
	fail "a corrected memory is still believed: $CURRENT"
printf 'ok   the corrected memory is no longer believed\n'

HISTORY=$(A memory list --history 2>&1)
printf '%s' "$HISTORY" | grep -q 'cut on Fridays' ||
	fail "a corrected memory was deleted rather than kept: $HISTORY"
printf '%s' "$HISTORY" | grep -q 'superseded by mem_corrected' ||
	fail "the correction is not named:
$HISTORY"
printf 'ok   without being deleted, and naming what replaced it\n'

printf '\nPASS: memory keeps both timelines\n'
