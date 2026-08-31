#!/bin/sh
# Proves the reference reducer still agrees with its recorded cases.
#
# This file used to check three clients in three languages against one set of
# examples, because nothing else stopped them drifting apart. Two of those
# clients are gone: the console and the macOS app were removed when the
# terminal became the only client.
#
# The fixtures stay, and the panel now reads them. They are the recorded
# behaviour of the session view — an event sequence and the screen it must
# produce — and two implementations are checked against them: the reference,
# and the reducer the panel folds live events with.
#
# Two in one language is worth less than two in three, and it is not nothing.
# The panel reads the written file rather than the Go builder, so a case the
# reference stopped producing is a case the panel stops being checked on, and
# both are checked against expectations written by hand from what a reader
# should see rather than from what either one computes.
set -eu

# No deployment at all: this check starts nothing and only runs tests, and
# saying so keeps it from reading the operator's settings by accident.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/.."

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

(cd core && go test ./internal/runtime/viewfixture/) >/dev/null 2>&1 ||
	fail "the reference disagrees with its own cases, or the fixtures are stale
$(cd core && go test ./internal/runtime/viewfixture/ 2>&1 | tail -20)"
printf 'ok   the reference agrees with every case\n'

(cd core && go test ./internal/tui/ -run Recorded -count=1 -v) >/tmp/panel-parity.log 2>&1 ||
	fail "the panel draws something other than the recorded cases
$(tail -30 /tmp/panel-parity.log)"

# The pass line rather than the exit status. A skipped test exits zero, and a
# check that accepted that would go on reporting agreement with cases nobody
# is running.
grep -q -- "--- PASS: TestThePanelAgreesWithTheRecordedCases" /tmp/panel-parity.log ||
	fail "nothing checked the panel against the recorded cases:
$(tail -20 /tmp/panel-parity.log)"
printf 'ok   and the panel draws them the same way\n'

git diff --quiet -- fixtures/session-view.json 2>/dev/null ||
	fail "fixtures/session-view.json was regenerated and differs; commit it"
printf 'ok   the written fixtures are up to date\n'

printf '\nall checks passed\n'
