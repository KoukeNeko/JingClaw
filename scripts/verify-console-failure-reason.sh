#!/bin/sh
# Proves the console no longer omits why a run failed: the whole reason is
# printed on failure, `why` reprints it, and the failed line names the kind.
set -eu

cd "$(dirname "$0")/../core"

printf 'vetting the console packages... '
go vet ./internal/console/... ./internal/cli/console/... >/dev/null
printf 'ok\n'

printf 'checking the failed line names its kind... '
go test -run 'TestAFailedRunNamesWhatWentWrong|TestARunningLineHasNoFailureKind' \
	./internal/console/... >/dev/null
printf 'passed\n'

printf 'checking the reason is printed in full and reachable by why... '
go test -run 'TestAFailureIsPrintedInFull|TestWhyReprintsTheLastFailure|TestWhyWithNoFailureSaysSo|TestWhyOnAReasonlessFailure|TestWhyRunsThroughTheCommandPath' \
	./internal/cli/console/... >/dev/null
printf 'passed\n'
