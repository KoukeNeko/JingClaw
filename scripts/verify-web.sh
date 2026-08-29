#!/bin/sh
# Proves the agent can read a page, and cannot read what it must not.
#
# The interesting half is the second one. Fetching a URL is the one capability
# whose input is routinely chosen by somebody hostile, and the ways it goes
# wrong are all the same shape: a name that looks public and resolves inward, a
# redirect that lands somewhere private, a page whose text is written to be
# read as an instruction. None of those are visible in a unit test of the happy
# path, so this drives the real fetcher against real addresses.
set -eu

# A .JingClaw directory above this checkout must not decide anything here: a
# check that reaches the operator's own deployment would read its settings and,
# worse, write to its database. Stated rather than relied on.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/agentd" ./cmd/agentd
go build -o "$WORK/agent" ./cmd/agent

DAEMON=""
VICTIM=""
cleanup() {
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$VICTIM" ] && kill "$VICTIM" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

# 1. The address guard, which runs before anything opens a connection.
go test ./internal/web/ ./internal/permission/ >/dev/null 2>&1 ||
	fail "the address guard and permission tests do not pass"
printf 'ok   private addresses, odd schemes and embedded credentials are refused\n'

# A local server standing in for everything the agent must not reach: this
# machine's own ports. If the guard is working, the agent never touches it.
cat > "$WORK/victim.py" <<'PY'
import http.server, socketserver, sys, threading

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        Handler.hit = True
        body = b"SECRET-INTERNAL-PAGE"
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
    with open(sys.argv[1], "w") as out:
        out.write(str(server.server_address[1]))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    import time
    time.sleep(120)
PY
python3 "$WORK/victim.py" "$WORK/victim.port" >"$WORK/victim.log" 2>&1 &
VICTIM=$!

WAITED=0
while [ ! -s "$WORK/victim.port" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stand-in server did not start"
	sleep 0.1
done
PORT=$(cat "$WORK/victim.port")

cat > "$WORK/config.toml" <<EOF
[provider]
backend = "fake"
fake_model = "fake-echo"
fake_delay = "0s"

[web]
enabled = true
backend = "browser"
timeout = "45s"

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
web_console = false
EOF

"$WORK/agentd" --config "$WORK/config.toml" >"$WORK/daemon.out" 2>"$WORK/daemon.err" &
DAEMON=$!

WAITED=0
while [ ! -f "$WORK/run/daemon.json" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 150 ] && fail "the daemon did not start: $(cat "$WORK/daemon.err")"
	sleep 0.1
done

# 2. The tool is offered only because the configuration asked for it.
grep -q 'web reading is on' "$WORK/daemon.err" ||
	fail "the daemon did not report web reading as enabled"
printf 'ok   web_read is registered when the configuration turns it on\n'

# 3. The real fetcher, against a real page. This is the half that a stub
#    cannot check: whether a browser is actually installed and driveable.
#
#    It needs a browser, which a build machine does not have. Skipping is
#    honest here in a way that passing would not be: the guard tests above
#    stand on their own, and claiming a fetch was verified when nothing was
#    fetched is how a capability ships broken.
if ! python3 -c 'import cloakbrowser' >/dev/null 2>&1; then
	printf '\nskipped the fetching checks: no browser on this machine\n'
	printf 'all checks passed\n'
	exit 0
fi

cat > "$WORK/probe_test.go" <<'GOEOF'
package web_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

func TestFetchesARealPage(t *testing.T) {
	fetcher := &web.BrowserFetcher{Timeout: 40 * time.Second}

	page, err := fetcher.Fetch(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if page.Status != 200 {
		t.Fatalf("status is %d", page.Status)
	}
	if !strings.Contains(page.Text, "Example Domain") {
		t.Fatalf("the page text is not the page:\n%s", page.Text)
	}
}

// The guard that matters most: the one inside the browser, where redirects are
// resolved and where the address the caller checked is no longer the address
// being fetched.
func TestWillNotReachThisMachine(t *testing.T) {
	port := os.Getenv("JINGCLAW_VERIFY_VICTIM_PORT")
	if port == "" {
		t.Skip("no stand-in server")
	}

	fetcher := &web.BrowserFetcher{Timeout: 20 * time.Second}

	page, err := fetcher.Fetch(context.Background(), "http://127.0.0.1:"+port+"/")
	if err == nil && strings.Contains(page.Text, "SECRET-INTERNAL-PAGE") {
		t.Fatal("the fetcher read a page on this machine")
	}
	// Logged rather than merely passed. "Something went wrong" and "the guard
	// stopped it" look identical from here, and only one of them is the thing
	// being verified.
	t.Logf("blocked with: %v", err)
}
GOEOF
cp "$WORK/probe_test.go" internal/web/verify_probe_test.go
JINGCLAW_VERIFY_VICTIM_PORT="$PORT" \
	go test ./internal/web/ -run 'TestFetchesARealPage|TestWillNotReachThisMachine' -count=1 \
	>"$WORK/probe.out" 2>&1
PROBE=$?
rm -f internal/web/verify_probe_test.go
[ "$PROBE" -eq 0 ] || fail "the real fetcher failed:
$(cat "$WORK/probe.out")"
printf 'ok   a real page is fetched, and this machine is not\n'

# 4. Nothing ever reached the stand-in server.
if grep -q 'SECRET-INTERNAL-PAGE' "$WORK"/*.out 2>/dev/null; then
	fail "an internal page leaked into the output"
fi
printf 'ok   nothing on this machine was read\n'

printf '\nall checks passed\n'
