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
for CHECK in verify-config.sh verify-api.sh verify-compaction.sh \
	verify-mcp.sh verify-memory.sh verify-images.sh verify-artifacts.sh \
	verify-web.sh verify-providers.sh verify-home.sh \
	verify-telegram.sh verify-reasoning.sh verify-processes.sh \
	verify-approval-preview.sh verify-takeover.sh verify-model-choice.sh \
	verify-shutdown.sh verify-plan.sh verify-ask.sh verify-patch.sh \
	verify-memory-time.sh verify-memory-trust.sh verify-memory-expand.sh \
	verify-approval-buttons.sh verify-tables.sh verify-service.sh verify-log-cursor.sh \
	verify-console.sh verify-foreign-approval.sh verify-skills.sh verify-sandbox.sh \
	verify-sandbox-linux.sh \
	verify-investigate.sh verify-mcp-oauth.sh verify-schedule.sh \
	verify-command-trust.sh verify-skills-install.sh \
	verify-attach.sh verify-gateway-restart.sh verify-clock.sh \
	verify-working-line.sh \
	verify-parity.sh; do
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
