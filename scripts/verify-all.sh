#!/bin/sh
# Runs every end-to-end check.
#
# These exist because six of the defects this project has had were in assembly
# seams rather than in logic — the daemon that never wired the projector, the
# codec that did not know an event, the bot that connected and then ignored
# everything. None of those were visible to a unit test, and all of them were
# visible within seconds of actually running the thing.
set -eu

cd "$(dirname "$0")"

FAILED=0
for CHECK in verify-config.sh verify-console.sh verify-compaction.sh \
	verify-mcp.sh verify-artifacts.sh; do
	printf '\n=== %s ===\n' "$CHECK"
	if ! "./$CHECK"; then
		FAILED=$((FAILED + 1))
	fi
done

printf '\n'
if [ "$FAILED" -ne 0 ]; then
	printf '%d check(s) failed\n' "$FAILED"
	exit 1
fi
printf 'everything passed\n'
