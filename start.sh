#!/bin/sh
# Build this checkout and run it.
#
# For working on JingClaw, as against using it: it rebuilds first, so what
# starts is what is in the working tree rather than whatever was installed
# last. An installed copy is `go install ./core/cmd/jingclaw` and then
# `jingclaw`, which needs no script.
#
# Arguments are passed through, so this is also how to run a subcommand
# against the code you are editing:
#
#   ./start.sh                 the console, with everything running under it
#   ./start.sh status          is one already running
#   ./start.sh stop            stop it
#   ./start.sh service install keep it running without a terminal
set -eu

cd "$(dirname "$0")"

# Built rather than `go run`, and to a stable path. The supervisor starts the
# daemon and the gateway from its own file, so all three are this build; `go
# run` works, but its executable lives in a temporary directory that only
# exists while it does, which makes `ps` harder to read than it needs to be.
BIN="core/bin/jingclaw"
mkdir -p "$(dirname "$BIN")"

printf 'building… '
(cd core && go build -o "bin/jingclaw" ./cmd/jingclaw)
printf 'done\n'

exec "./$BIN" "$@"
