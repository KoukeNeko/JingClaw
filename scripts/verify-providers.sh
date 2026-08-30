#!/bin/sh
# Proves the daemon can actually run against a local model server.
#
# The unit tests decode responses; this checks the part they cannot, which is
# whether the wiring holds: configuration reaching the adapter, the catalogue
# being read at startup, and the context window that compaction will plan
# against being the one the server actually reported.
#
# That last one is the reason these adapters exist. A model trained for 131072
# and loaded with 4096 has 4096, and a runtime that plans against the larger
# figure sends requests that cannot be served while waiting for a compaction
# threshold it will never reach. Both stand-in servers below report exactly
# that mismatch, and the check is that the smaller number wins.
set -eu

# A .JingClaw directory above this checkout must not decide anything here: a
# check that reaches the operator's own deployment would read its settings and,
# worse, write to its database. Stated rather than relied on.
export JINGCLAW_HOME=none

cd "$(dirname "$0")/../core"

WORK=$(mktemp -d)
go build -o "$WORK/jingclaw" ./cmd/jingclaw

DAEMON=""
SERVER=""
cleanup() {
	# Best effort from here. Killing something that has already exited fails,
	# and under set -e that failure ends this function where it stands: the
	# parts after it are not stopped and the work directory is not removed. A
	# check whose daemon died would then leave its stub holding a port, and the
	# next check to want that port would talk to the stub of a run that is over.
	set +e
	[ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
	[ -n "$SERVER" ] && kill "$SERVER" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

cat > "$WORK/server.py" <<'PY'
"""A stand-in for Ollama and for an OpenAI-compatible server.

Both report a model loaded with far less context than its weights allow, which
is the situation these adapters exist to notice.

The shapes here are copied from what a real daemon sends rather than from the
documentation, because the two differ: the listing carries the context length
and the capabilities, which the documentation describes as living only in
/api/show.
"""
import http.server, json, socketserver, sys, threading, time

LOADED, TRAINED = 4096, 131072


class Handler(http.server.BaseHTTPRequestHandler):
    def _send(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/api/tags":
            self._send({"models": [{"model": "qwen3:8b", "name": "qwen3:8b",
                                    "details": {"family": "qwen3", "parameter_size": "8.2B",
                                                "context_length": TRAINED},
                                    "capabilities": ["completion", "tools"]}]})
        elif self.path == "/api/ps":
            self._send({"models": [{"model": "qwen3:8b", "context_length": LOADED}]})
        elif self.path.endswith("/models"):
            self._send({"data": [{"id": "local-model", "max_model_len": LOADED,
                                  "meta": {"n_ctx_train": TRAINED}}]})
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if self.path == "/api/show":
            self._send({"capabilities": ["completion", "tools"],
                        "model_info": {"qwen3.context_length": TRAINED}})
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *args):
        pass


with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
    with open(sys.argv[1], "w") as out:
        out.write(str(server.server_address[1]))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    time.sleep(180)
PY

python3 "$WORK/server.py" "$WORK/port" >"$WORK/server.log" 2>&1 &
SERVER=$!

WAITED=0
while [ ! -s "$WORK/port" ]; do
	WAITED=$((WAITED + 1))
	[ "$WAITED" -gt 100 ] && fail "the stand-in server did not start"
	sleep 0.1
done
PORT=$(cat "$WORK/port")

# start writes a config for one provider and reports what the daemon settled on.
start() {
	NAME=$1
	rm -rf "$WORK/run" "$WORK/data"
	mkdir -p "$WORK/run" "$WORK/data" "$WORK/ws"

	"$WORK/jingclaw" daemon --config "$WORK/$NAME.toml" >"$WORK/$NAME.out" 2>"$WORK/$NAME.err" &
	DAEMON=$!

	WAITED=0
	while [ ! -f "$WORK/run/daemon.json" ]; do
		WAITED=$((WAITED + 1))
		[ "$WAITED" -gt 150 ] && fail "$NAME: the daemon did not start:
$(cat "$WORK/$NAME.err")"
		sleep 0.1
	done
	sleep 1

	kill "$DAEMON" 2>/dev/null
	DAEMON=""
}

cat > "$WORK/ollama.toml" <<EOF
[provider]
backend = "ollama"

[provider.ollama]
model = "qwen3:8b"
base_url = "http://127.0.0.1:$PORT"
keep_alive = "30m"

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

cat > "$WORK/compat.toml" <<EOF
[provider]
backend = "openai_compat"

[provider.openai_compat]
base_url = "http://127.0.0.1:$PORT/v1"
profile = "vllm"
name = "stand-in"

[workspace]
root = "$WORK/ws"

[server]
runtime_dir = "$WORK/run"
data_dir = "$WORK/data"
EOF

check() {
	NAME=$1
	WANT_PROVIDER=$2

	LINE=$(grep -o '"msg":"jingclaw daemon listening"[^}]*' "$WORK/$NAME.err" || true)
	[ -n "$LINE" ] || fail "$NAME: the daemon never reported listening:
$(cat "$WORK/$NAME.err")"

	echo "$LINE" | grep -q "\"provider\":\"$WANT_PROVIDER\"" ||
		fail "$NAME: the wrong provider was built: $LINE"

	# The number compaction will plan against, and where it came from.
	echo "$LINE" | grep -q '"context_window":4096' ||
		fail "$NAME: planned against the model's trained context rather than what the server loaded: $LINE"
	echo "$LINE" | grep -q '"context_window_from":"runtime"' ||
		fail "$NAME: the window's provenance was not recorded: $LINE"
}

start ollama
check ollama ollama
printf 'ok   ollama: the catalogue is read and the loaded context wins\n'

start compat
check compat "openai_compat/stand-in"
printf 'ok   openai_compat: the endpoint is read and the loaded context wins\n'

# A misconfiguration has to be refused at startup rather than on a first turn.
sed 's/profile = "vllm"/profile = "vlm"/' "$WORK/compat.toml" > "$WORK/typo.toml"
if "$WORK/jingclaw" daemon --config "$WORK/typo.toml" >"$WORK/typo.out" 2>&1; then
	fail "a misspelled profile was accepted"
fi
grep -q "profile" "$WORK/typo.out" || fail "the refusal does not name the setting:
$(cat "$WORK/typo.out")"
printf 'ok   a misspelled profile is refused, and says which setting\n'

printf '\nall checks passed\n'

