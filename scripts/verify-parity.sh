#!/bin/sh
# Proves every client turns the same events into the same screen.
#
# Three clients fold an event log into a conversation, in three languages.
# Nothing stops them drifting apart except a set of examples they are all
# checked against: an event sequence, and the state it must produce.
#
# A disagreement here is not a cosmetic difference. It means somebody watching
# the same session in the console and in the app is being told two different
# things about what the agent did.
set -eu

export JINGCLAW_HOME=none

cd "$(dirname "$0")/.."

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# The reference, and the file the other clients read.
(cd core && go test ./internal/runtime/viewfixture/) >/dev/null 2>&1 ||
	fail "the reference disagrees with its own cases, or the fixtures are stale
$(cd core && go test ./internal/runtime/viewfixture/ 2>&1 | tail -20)"
printf 'ok   go agrees with every case\n'

git diff --quiet -- fixtures/session-view.json 2>/dev/null ||
	fail "fixtures/session-view.json was regenerated and differs; commit it"
printf 'ok   the written fixtures are up to date\n'

# The console's own reducer, against the same file.
if command -v node >/dev/null 2>&1; then
	node fixtures/check-js.mjs >/dev/null 2>&1 ||
		fail "the console's reducer disagrees:
$(node fixtures/check-js.mjs 2>&1 | grep -A 3 FAIL | head -20)"
	printf 'ok   the console agrees with every case\n'
else
	printf 'skipped the console: no node on this machine\n'
fi

# The reducer the fixtures check has to be the one that runs. A shared reducer
# nothing imports is a second implementation with extra steps.
grep -q "from './reduce.js'" core/internal/webui/assets/app.js ||
	fail "the console does not import the shared reducer"
printf 'ok   the console uses the reducer that was checked\n'

# Swift, when there is a client and a toolchain for it.
#
# Checked for Package.swift rather than for the directory: the directory has
# existed since the first milestone with nothing in it, and a check that
# passes against an empty one is worse than no check. Not piped either — the
# exit status of a pipeline is the last command's, so `swift test | tail`
# reports whether tail worked.
if [ -f macos/Package.swift ] && command -v swift >/dev/null 2>&1; then
	if ! (cd macos && swift test >"$TMPDIR/swift-parity.out" 2>&1); then
		fail "the macOS client disagrees:
$(tail -20 "$TMPDIR/swift-parity.out")"
	fi
	printf 'ok   the macOS client agrees with every case\n'
elif [ -f macos/Package.swift ]; then
	printf 'skipped swift: no toolchain on this machine\n'
else
	printf 'skipped swift: no client yet\n'
fi

printf '\nall checks passed\n'
