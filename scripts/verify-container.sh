#!/bin/sh
# Proves the image is a deployment and carries none of one.
#
# The thing that goes wrong with a containerised agent is not that it fails to
# start. It is that it starts carrying something it should not — a settings
# file baked in that silently stops being read the day somebody writes their
# own, a credential in a layer that anybody who can pull the image can read
# back — or that it starts as root, or that it comes up without the things a
# command needs and every tool call fails at once.
#
# So the checks are about what is in the image and what is in the volume,
# rather than about whether it runs.
set -eu

cd "$(dirname "$0")/.."

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

if ! command -v docker >/dev/null 2>&1; then
	printf 'skipped: no docker here, and this is about an image\n'
	exit 0
fi
if ! docker info >/dev/null 2>&1; then
	printf 'skipped: docker is installed and not running\n'
	exit 0
fi

IMAGE=jingclaw-check:$$
VOLUME=jingclaw-check-$$
NAME=jingclaw-check-$$

cleanup() {
	# Best effort from here. Removing something that is already gone fails,
	# and under set -e that failure ends this function where it stands,
	# leaving a container holding a volume that the next run cannot remove.
	set +e
	docker rm -f "$NAME" >/dev/null 2>&1
	docker rm -f "$NAME-again" >/dev/null 2>&1
	docker rm -f "$NAME-first" >/dev/null 2>&1
	docker volume rm "$VOLUME" >/dev/null 2>&1
	docker image rm -f "$IMAGE" >/dev/null 2>&1
}
trap cleanup EXIT

docker build -t "$IMAGE" . >/tmp/container-build.log 2>&1 ||
	fail "the image does not build:
$(tail -25 /tmp/container-build.log)"
printf 'ok   the image builds\n'

# A secret written into the volume rather than passed on the command line.
# Both routes are supported and only one of them keeps the value out of
# docker inspect, out of the shell history that started it, and out of the
# process list of anything that can read /proc.
SECRET=this-must-never-leave-the-volume
docker volume create "$VOLUME" >/dev/null

docker run --rm -v "$VOLUME:/var/lib/jingclaw" --entrypoint sh "$IMAGE" -c \
	"printf '%s' '$SECRET' > /var/lib/jingclaw/discord.token &&
	 chmod 600 /var/lib/jingclaw/discord.token" >/dev/null ||
	fail "could not write into the volume as the image's own user"
printf 'ok   the volume is writable by the user the image runs as\n'

# What a first run looks like. An image that needs a credential must say
# which one and where to put it: an operator whose container exits with
# nothing to act on has only the log of a process that is already gone.
docker run --rm -v "$VOLUME:/var/lib/jingclaw" --entrypoint sh "$IMAGE" -c \
	'rm -f /var/lib/jingclaw/discord.token' >/dev/null

FIRST=$(docker run --rm --name "$NAME-first" -v "$VOLUME:/var/lib/jingclaw" "$IMAGE" 2>&1 || true)
printf '%s' "$FIRST" | grep -q "DISCORD_BOT_TOKEN" ||
	fail "a first run with no credential does not name the one it wants: $FIRST"
printf '%s' "$FIRST" | grep -q "/var/lib/jingclaw/discord.token" ||
	fail "it does not say where to put the credential: $FIRST"
printf 'ok   a first run with nothing set up says what it needs and where\n'

# Put it back for the rest.
docker run --rm -v "$VOLUME:/var/lib/jingclaw" --entrypoint sh "$IMAGE" -c \
	"printf '%s' '$SECRET' > /var/lib/jingclaw/discord.token &&
	 chmod 600 /var/lib/jingclaw/discord.token" >/dev/null

# The daemon on its own from here. The gateway wants a credential a platform
# will accept, and this check has no platform; what is being checked below is
# the image and the volume, which the daemon exercises exactly as well.
docker run -d --name "$NAME" -v "$VOLUME:/var/lib/jingclaw" "$IMAGE" daemon >/dev/null ||
	fail "the image does not run"

WAITED=0
while ! docker exec "$NAME" test -f /var/lib/jingclaw/run/daemon.json 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] &&
		fail "the daemon never published itself: $(docker logs "$NAME" 2>&1 | tail -20)"
	sleep 0.2
done
printf 'ok   it starts against a volume of its own and publishes itself\n'

# Not root. The agent runs commands somebody approved, which is a different
# thing from commands somebody wrote.
WHO=$(docker exec "$NAME" id -u)
[ "$WHO" != "0" ] || fail "it runs as root"
printf 'ok   and not as root (uid %s)\n' "$WHO"

# The things a command needs. Without git the one tool the code invokes by
# name fails; without CA certificates every provider call fails at TLS, which
# reads like a wrong key rather than a missing package.
docker exec "$NAME" git --version >/dev/null 2>&1 ||
	fail "git is not in the image, so the agent cannot use the one command it calls by name"
docker exec "$NAME" test -f /etc/ssl/certs/ca-certificates.crt ||
	fail "no CA certificates, so every model call fails at TLS"
printf 'ok   with git and CA certificates\n'

# Nothing about a deployment is in the image. A settings file shipped inside
# becomes the operator's by accident and then stops being read the day they
# write their own.
docker run --rm --entrypoint sh "$IMAGE" -c \
	'test ! -e /var/lib/jingclaw/config.toml && test ! -e /var/lib/jingclaw/data' ||
	fail "the image carries a deployment of its own"
docker run --rm --entrypoint sh "$IMAGE" -c 'test -f /usr/share/jingclaw/config.example.toml' ||
	fail "there is nothing to copy a configuration from"
printf 'ok   the image carries no settings, no database, and an example to start from\n'

# The secret is in the volume and nowhere a puller or a passer-by can read it.
docker inspect "$NAME" | grep -q "$SECRET" &&
	fail "the secret is in docker inspect"
docker history --no-trunc "$IMAGE" 2>/dev/null | grep -q "$SECRET" &&
	fail "the secret is in an image layer"
docker exec "$NAME" cat /var/lib/jingclaw/discord.token | grep -q "$SECRET" ||
	fail "the secret in the volume is not readable by the process that needs it"
printf 'ok   a secret in the volume is readable there and nowhere else\n'

# The volume is the deployment, so replacing the container keeps it.
SESSION=$(docker exec "$NAME" jingclaw session create --title "before" 2>/dev/null |
	grep -oE 'ses_[A-Za-z0-9]+' | head -1)
[ -n "$SESSION" ] || fail "could not create a session to prove the database persists"

docker rm -f "$NAME" >/dev/null
docker run -d --name "$NAME-again" -v "$VOLUME:/var/lib/jingclaw" "$IMAGE" daemon >/dev/null

WAITED=0
while ! docker exec "$NAME-again" test -f /var/lib/jingclaw/run/daemon.json 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] &&
		fail "it did not come back: $(docker logs "$NAME-again" 2>&1 | tail -20)"
	sleep 0.2
done

docker exec "$NAME-again" jingclaw session list 2>/dev/null | grep -q "$SESSION" ||
	fail "the session from before the container was replaced is gone"
printf 'ok   the database outlives the container\n'

printf '\nall checks passed\n'
