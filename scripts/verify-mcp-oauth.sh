#!/bin/sh
# Proves signing in to a tool server is something a person does, once.
#
# The daemon has no browser and nobody is watching it. So the whole of the
# interactive flow lives in a command, and what the daemon does with a server
# it cannot reach is say which command to run — not open a page nobody will
# see, and not retry a dead credential forever.
#
# The server here refuses anything sloppy: a redirect_uri that was not
# registered, a missing PKCE verifier, an application_type that is not native,
# a refresh token that has already been rotated. A lenient stub would pass
# while the client was wrong about all four.
set -eu

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)

export JINGCLAW_HOME="$WORK"
# Nobody wants a browser window from a check. The URL is still announced, so
# what this drives by hand is exactly what a person would have clicked.
export JINGCLAW_NO_BROWSER=1

go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
STUB=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	# Silently: the shell announces a killed job otherwise, on a run
	# that passed.
	[ -n "$STUB" ] && { kill "$STUB" 2>/dev/null; wait "$STUB" 2>/dev/null; }
	[ -n "${KEEP:-}" ] && { printf "kept %s\n" "$WORK"; return; }
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/workspace"

python3 ../scripts/support/oauth-mcp-server.py > "$WORK/stub.url" 2>"$WORK/stub.err" &
STUB=$!

WAITED=0
while [ ! -s "$WORK/stub.url" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stub server did not start: $(cat "$WORK/stub.err")"
	sleep 0.1
done
ORIGIN=$(cat "$WORK/stub.url")

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"

[server]
addr = "127.0.0.1:7811"
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"

[[mcp.servers]]
name = "books"
url = "$ORIGIN/mcp"
oauth = true
EOF

# 1. Before anybody signs in.
LISTED=$("$WORK/jingclaw" mcp list --config "$WORK/config.toml" 2>&1)
printf '%s' "$LISTED" | grep -q 'jingclaw mcp login books' ||
	fail "listing does not say what to run: $LISTED"
printf 'ok   a server nobody has signed in to says which command fixes it\n'

# And the daemon says the same thing, rather than reporting a fault.
"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!
WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

grep -q 'needs authorizing' "$WORK/daemon.err" ||
	fail "the daemon called an unauthorized server broken: $(grep -i books "$WORK/daemon.err")"
grep -q 'jingclaw mcp login books' "$WORK/daemon.err" ||
	fail "the daemon did not say what to run: $(grep -i books "$WORK/daemon.err")"
printf 'ok   and the daemon reports it as needing a person, not as a fault\n'

grep -q '0 of 1 mcp servers' "$WORK/daemon.out" ||
	fail "the startup line does not say the server is missing: $(grep Tools "$WORK/daemon.out")"
grep -q 'jingclaw mcp login' "$WORK/daemon.out" ||
	fail "the line a person reads does not say what to run: $(grep Tools "$WORK/daemon.out")"
printf 'ok   and does not offer its tools in the meantime\n'

kill "$DAEMON" 2>/dev/null; DAEMON=""

# 2. Somebody signs in. The browser is this script.
"$WORK/jingclaw" mcp login books --config "$WORK/config.toml" \
	>"$WORK/login.out" 2>"$WORK/login.err" &
LOGIN=$!

WAITED=0
while : ; do
	AUTHORIZE=$(grep -o "$ORIGIN/authorize?[^ ]*" "$WORK/login.err" 2>/dev/null | head -1)
	[ -n "$AUTHORIZE" ] && break
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 300 ] && fail "no authorization page was offered: $(cat "$WORK/login.err")"
	sleep 0.1
done
printf 'ok   signing in offers a page, so a machine with no browser can still do it\n'

# Following it is what a browser does: the authorization server redirects to
# the loopback address the client registered, and the client is waiting there.
curl -sL "$AUTHORIZE" >/dev/null || fail "the authorization page could not be followed"

wait "$LOGIN" || fail "signing in failed: $(cat "$WORK/login.err")"
printf 'ok   and the whole exchange completes: registration, pkce, and the token\n'

# 3. What was stored.
SESSION="$WORK/mcp-auth/books.json"
[ -f "$SESSION" ] || fail "nothing was stored"

MODE=$(stat -f '%Lp' "$SESSION" 2>/dev/null || stat -c '%a' "$SESSION")
[ "$MODE" = "600" ] || fail "the session is mode $MODE"
printf 'ok   the session is on disk, readable by nobody else\n'

python3 - "$SESSION" <<'CHECK' || exit 1
import json, sys

session = json.load(open(sys.argv[1]))
config, token = session.get("config"), session.get("token")

if not token or not token.get("refresh_token"):
    print(f"FAIL: no refresh token was kept: {token}", file=sys.stderr)
    raise SystemExit(1)

# The client was registered during this flow and exists nowhere else. A store
# that kept only the token would work until the first refresh needed it.
if not config or not config.get("ClientID", config.get("client_id")):
    print(f"FAIL: the client this was obtained with was not kept: {config}", file=sys.stderr)
    raise SystemExit(1)
CHECK
printf 'ok   and holds the client it was obtained with, not just the token\n'

# 4. The daemon picks it up without being told anything.
"$WORK/jingclaw" daemon --config "$WORK/config.toml" >"$WORK/daemon2.out" 2>"$WORK/daemon2.err" &
DAEMON=$!
WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not restart: $(cat "$WORK/daemon2.err")"
	sleep 0.1
done

grep -q '1 of 1 mcp servers' "$WORK/daemon2.out" ||
	fail "the daemon did not use the stored session: $(grep Tools "$WORK/daemon2.out")"
grep -q 'needs authorizing' "$WORK/daemon2.err" &&
	fail "the daemon asked for a sign-in that had already happened"
printf 'ok   the daemon uses it on its next start, with nothing else told to it\n'

LISTED=$("$WORK/jingclaw" mcp list --config "$WORK/config.toml" 2>&1)
printf '%s' "$LISTED" | grep -q 'signed in' ||
	fail "listing does not report the sign-in: $LISTED"
printf 'ok   and says so\n'

kill "$DAEMON" 2>/dev/null; DAEMON=""

# 5. Signing out is a thing somebody asks for, and only that.
"$WORK/jingclaw" mcp logout books --config "$WORK/config.toml" >/dev/null 2>&1 ||
	fail "signing out failed"
[ -f "$SESSION" ] && fail "the session survived being forgotten"
printf 'ok   signing out forgets it\n'

printf '\nall checks passed\n'
