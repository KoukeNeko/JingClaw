#!/bin/sh
# Proves the panel always gives the terminal back.
#
# A panel that exits badly leaves alternate screen on and the cursor hidden,
# and the person it happens to has no way to fix it except closing the window.
# That is not a cosmetic failure — it is the tool making the machine worse on
# its way out, and it happens exactly when something has already gone wrong.
#
# Four ways out are checked, because they leave through different doors:
# being told to stop, being interrupted, being suspended and resumed, and
# failing outright. Each runs under a real PTY, and each reads the bytes on
# the wire rather than asking a library whether it tidied up.
set -eu

cd "$(dirname "$0")/../core"

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# The tests re-exec this test binary to get a panel under a PTY, so they need
# to build. A compile failure here is a failure, not a skip: unlike a kernel
# feature, there is no machine where this legitimately cannot run.
go vet ./internal/tui/ >/tmp/tui-vet.log 2>&1 ||
	fail "the panel does not build:
$(tail -20 /tmp/tui-vet.log)"

go test -buildvcs=false ./internal/tui/ -count=1 -timeout 120s -v \
	>/tmp/tui-lifecycle.log 2>&1 ||
	fail "the panel did not give the terminal back:
$(tail -30 /tmp/tui-lifecycle.log)"

for WAY in \
	'WhenItIsToldToStop:told to stop' \
	'OnInterrupt:interrupted' \
	'AfterBeingSuspended:suspended and resumed' \
	'WhenItCrashes:crashing'; do
	NAME=${WAY%%:*}
	SAID=${WAY#*:}
	# The pass line, not the name. A check that only looked for the name
	# would go on passing after somebody skipped the test, which is the
	# state it exists to notice.
	grep -q -- "--- PASS: TestItGivesTheTerminalBack$NAME" /tmp/tui-lifecycle.log ||
		fail "nothing proved the terminal comes back after being $SAID"
	printf 'ok   the terminal comes back after %s\n' "$SAID"
done

printf '\nall checks passed\n'
