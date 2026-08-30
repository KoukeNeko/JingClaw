#!/bin/sh
# Proves the reference reducer still agrees with its recorded cases.
#
# This file used to check three clients in three languages against one set of
# examples, because nothing else stopped them drifting apart. Two of those
# clients are gone: the console and the macOS app were removed when the
# terminal became the only client.
#
# The fixtures stay. They are the recorded behaviour of the session view — an
# event sequence and the screen it must produce — and the TUI will be the next
# thing checked against them. Deleting them because there is momentarily only
# one reader would mean writing them again from the implementation, which is
# how a fixture stops being evidence and becomes a copy of the code.
set -eu

# No deployment at all: this check starts nothing and only runs tests, and
# saying so keeps it from reading the operator's settings by accident.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/.."

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

(cd core && go test ./internal/runtime/viewfixture/) >/dev/null 2>&1 ||
	fail "the reference disagrees with its own cases, or the fixtures are stale
$(cd core && go test ./internal/runtime/viewfixture/ 2>&1 | tail -20)"
printf 'ok   go agrees with every case\n'

git diff --quiet -- fixtures/session-view.json 2>/dev/null ||
	fail "fixtures/session-view.json was regenerated and differs; commit it"
printf 'ok   the written fixtures are up to date\n'

printf '\nall checks passed\n'
