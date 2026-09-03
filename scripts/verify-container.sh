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
	docker rm -f "$NAME-seeded" >/dev/null 2>&1
	docker rm -f "$NAME-web" >/dev/null 2>&1
	docker volume rm "$VOLUME-seeded" >/dev/null 2>&1
	docker volume rm "$VOLUME-web" >/dev/null 2>&1
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

# A volume mounted where the documentation says is writable by the user the
# image runs as, and one mounted anywhere else is not.
#
# Both halves matter. A named volume takes its ownership from the directory it
# is mounted over, and the image owns that one; mounted somewhere the image has
# no directory, the runtime creates it owned by root and the image is not root.
# The documentation recommended the second arrangement until this was measured,
# and what that produces is a first run that can write nothing and says only
# "permission denied".
docker run --rm -v "$VOLUME:/var/lib/jingclaw" --entrypoint sh "$IMAGE" -c \
	'touch /var/lib/jingclaw/.writable && rm /var/lib/jingclaw/.writable' >/dev/null 2>&1 ||
	fail "a volume mounted where the documentation says is not writable by the image's own user"

ELSEWHERE="$VOLUME-elsewhere"
docker volume create "$ELSEWHERE" >/dev/null
WROTE=no
docker run --rm -v "$ELSEWHERE:/data" --entrypoint sh "$IMAGE" -c \
	'touch /data/x' >/dev/null 2>&1 && WROTE=yes
docker volume rm "$ELSEWHERE" >/dev/null 2>&1
[ "$WROTE" = "no" ] ||
	fail "a volume mounted at /data is writable, so the documentation's reason for
insisting on /var/lib/jingclaw is no longer true and should be rewritten"
printf 'ok   the documented mount path is the writable one, and another is not\n'

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

# Nothing is left behind by a command that forked and walked away.
#
# The agent runs shells, build tools and whatever somebody approved, and those
# fork. A grandchild orphaned by its parent exiting is re-parented to PID 1,
# and if PID 1 is not an init it never reaps it: Go waits on the children
# os/exec started and on nothing else. They accumulate as zombies until the
# process table is full and the container can start nothing at all — silently,
# because everything looks fine until it does not.
docker exec "$NAME" sh -c '
	sh -c "(sleep 1; exit 0) & exit 0"
	sleep 3' >/dev/null 2>&1

ZOMBIES=$(docker exec "$NAME" sh -c '
	for p in $(ls /proc | grep -E "^[0-9]+$"); do
		[ -r /proc/$p/stat ] || continue
		set -- $(cat /proc/$p/stat 2>/dev/null)
		[ "$3" = "Z" ] && echo "$1"
	done' 2>/dev/null | wc -l | tr -d " ")

[ "$ZOMBIES" = "0" ] ||
	fail "$ZOMBIES zombie(s) left after one orphan; PID 1 is $(docker exec "$NAME" cat /proc/1/comm) and is not reaping"
printf 'ok   an orphaned grandchild is reaped rather than left as a zombie\n'

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

# The files a deployment brings when a variable is the only way in. Checked
# here rather than only in a unit test, because this is the one arrangement
# where it matters: no shell, no way to put a file in the volume first, and a
# platform that supplies a configuration and gets a default instead has no
# symptom other than an agent behaving like nobody configured it.
SEEDED="$VOLUME-seeded"
docker volume create "$SEEDED" >/dev/null
docker run -v "$SEEDED:/var/lib/jingclaw" \
	-e JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"
' \
	-e JINGCLAW_PERSONA="base64:$(printf '# Who you are\n\nAnswer briefly.\n' | base64 | tr -d '\n')" \
	--name "$NAME-seeded" -d "$IMAGE" daemon >/dev/null

WAITED=0
while ! docker run --rm -v "$SEEDED:/var/lib/jingclaw" --entrypoint sh "$IMAGE" \
	-c 'test -f /var/lib/jingclaw/PERSONA.md' 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] &&
		fail "nothing was written from the environment: $(docker logs "$NAME-seeded" 2>&1 | tail -20)"
	sleep 0.2
done
docker rm -f "$NAME-seeded" >/dev/null

SEEN=$(docker run --rm -v "$SEEDED:/var/lib/jingclaw" --entrypoint sh "$IMAGE" -c \
	'cat /var/lib/jingclaw/PERSONA.md; echo ---; cat /var/lib/jingclaw/config.toml')
docker volume rm "$SEEDED" >/dev/null 2>&1

# The blank line is the point: a document that arrives base64-encoded arrives
# whole, and one whose newlines were dropped is a different document.
printf '%s' "$SEEN" | grep -q '^Answer briefly\.$' ||
	fail "the persona did not survive the trip through the environment: $SEEN"
printf '%s' "$SEEN" | grep -q 'fake-echo' ||
	fail "the deployment is running the example rather than the supplied configuration"
printf 'ok   a deployment can bring its own settings and persona in variables\n'

# Turning web reading on does not stop the daemon coming up.
#
# The check is a start, not a fetch. What broke a deployment was the refusal
# at startup — no python3 in the image, so a daemon with web.enabled = true
# never ran at all — and a page fetch here would test somebody else's network
# rather than this image.
WEBBED="$VOLUME-web"
docker volume create "$WEBBED" >/dev/null
docker run --name "$NAME-web" -d -v "$WEBBED:/var/lib/jingclaw" \
	-e JINGCLAW_CONFIG='[provider]
backend = "fake"
fake_model = "fake-echo"

[web]
enabled = true
' \
	"$IMAGE" daemon >/dev/null

WAITED=0
while ! docker exec "$NAME-web" test -f /var/lib/jingclaw/run/daemon.json 2>/dev/null; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] &&
		fail "it will not start with web reading on: $(docker logs "$NAME-web" 2>&1 | tail -20)"
	sleep 0.2
done
docker rm -f "$NAME-web" >/dev/null
docker volume rm "$WEBBED" >/dev/null 2>&1
printf 'ok   it starts with web reading turned on\n'

# The wrapper is importable and the browser it drives is not in the image.
#
# Both halves. Without the wrapper, web reading cannot work at all. With the
# binary baked in, this image would be redistributing a compiled Chromium that
# its licence says may not be redistributed — a check rather than a comment,
# because that is the kind of thing a later "just make it faster" undoes.
docker run --rm --entrypoint python3 "$IMAGE" -c 'import cloakbrowser' >/dev/null 2>&1 ||
	fail "cloakbrowser is not importable, so web reading cannot work in this image"

docker run --rm --entrypoint sh "$IMAGE" -c \
	'find / -xdev -name "*chrom*" -type f -size +50M 2>/dev/null | head -1' | grep -q . &&
	fail "a browser binary is in the image, which its licence does not allow redistributing"
printf 'ok   the wrapper is in the image and the browser it fetches is not\n'

printf '\nall checks passed\n'
