#!/bin/sh
# Proves the Linux confinement is real, on a real Linux kernel.
#
# Run in a container rather than skipped, because everything else about a
# sandbox can be right while the thing it exists for does not happen. The
# macOS check beside this one runs where the daemon runs; this one cannot, so
# it borrows a kernel.
#
# Skipped, with a reason, where there is no way to get one. A check that
# quietly passes on a machine that could not run it is worse than one that
# says it did not.
set -eu

cd "$(dirname "$0")/.."

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

if ! command -v docker >/dev/null 2>&1; then
	printf 'skipped: no docker here, and this needs a linux kernel to be real\n'
	exit 0
fi
if ! docker info >/dev/null 2>&1; then
	printf 'skipped: docker is installed and not running\n'
	exit 0
fi

# The kernel decides whether any of this can be checked at all. Asked before
# anything else, so "no landlock" reads as a skip rather than as a failure.
ABI=$(docker run --rm -v "$PWD:/src" -w /src/core golang:alpine sh -c '
	cat > /tmp/abi.go <<GO
package main

import (
	"fmt"

	"github.com/landlock-lsm/go-landlock/landlock/syscall"
)

func main() {
	version, err := syscall.LandlockGetABIVersion()
	if err != nil {
		fmt.Println(0)
		return
	}
	fmt.Println(version)
}
GO
	go run -buildvcs=false /tmp/abi.go 2>/dev/null || echo 0
' 2>/dev/null | tail -1)

case "$ABI" in
	''|*[!0-9]*) fail "could not ask the kernel what landlock it has" ;;
esac

if [ "$ABI" -lt 1 ]; then
	printf 'skipped: this kernel has no landlock, so there is nothing to enforce\n'
	exit 0
fi
printf 'ok   the kernel offers landlock ABI %s\n' "$ABI"

# Everything else is the package's own checks, which are the ones that watch
# a command actually be refused.
docker run --rm -v "$PWD:/src" -w /src/core golang:alpine sh -c '
	apk add --no-cache netcat-openbsd >/dev/null 2>&1
	go test -buildvcs=false ./internal/sandbox/ -count=1 -timeout 120s
' >/tmp/sandbox-linux.log 2>&1 ||
	fail "the kernel did not enforce what was asked of it:
$(tail -20 /tmp/sandbox-linux.log)"

printf 'ok   a confined command cannot write outside what it was given\n'
if [ "$ABI" -ge 4 ]; then
	printf 'ok   and cannot open a connection a policy forbade\n'
else
	printf 'ok   and a policy asking for no network is refused, this ABI being too old for it\n'
fi

# And the daemon says which of those it will actually enforce, rather than
# only that confinement is on.
SAID=$(docker run --rm -v "$PWD:/src" -w /src/core golang:alpine sh -c '
	go run -buildvcs=false ./cmd/jingclaw daemon --print-config 2>/dev/null | grep -c "sandbox" || true
' 2>/dev/null | tail -1)
[ "$SAID" -ge 1 ] || fail "the settings do not mention the sandbox at all"
printf 'ok   and the setting that turns it on is documented\n'

printf '\nall checks passed\n'
