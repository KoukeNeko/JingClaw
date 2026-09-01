# A stub standing in for Telegram, so a check needs no bot and no network.
#
# Shared by every check that needs a platform to talk to. Copied into each of
# them once, it drifted: a stub that answers one check's calls and not
# another's is a stub two checks disagree about.
#
# Arguments: port, where to record the calls, the chat id, the user id, and
# optionally a file to take further messages from — one per line, consumed as
# they are read. Without that last one it delivers a single message and then
# nothing, which is what a check that only needs one wants.
import json, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, calls_path, chat_id, user_id = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
inject_path = sys.argv[5] if len(sys.argv) > 5 else None

lock = threading.Lock()
next_update = 0


def take_one_line(path):
    """The next message a check asked for, removed as it is handed over.

    Consumed rather than read, so a message is delivered once however many
    times the gateway polls."""
    try:
        with open(path) as pending:
            lines = [line.rstrip("\n") for line in pending if line.strip()]
    except FileNotFoundError:
        return None
    if not lines:
        return None
    with open(path, "w") as rest:
        rest.write("\n".join(lines[1:]) + ("\n" if lines[1:] else ""))
    return lines[0]
delivered = False
next_id = 5000


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        global delivered, next_id, next_update

        method = self.path.rsplit("/", 1)[-1]
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw) if raw else {}
        except ValueError:
            body = {"_raw": raw.decode("utf-8", "replace")}

        with lock:
            with open(calls_path, "a") as log:
                log.write(json.dumps({"method": method, "body": body}) + "\n")

            if method == "getMe":
                result = {"id": 1, "username": "jingclaw_bot"}
            elif method == "getUpdates":
                said = None
                if not delivered:
                    delivered = True
                    said = "say something back"
                elif inject_path:
                    said = take_one_line(inject_path)

                if said is None:
                    result = []
                else:
                    next_update += 1
                    result = [{
                        "update_id": next_update,
                        "message": {
                            "message_id": 10 + next_update,
                            "from": {"id": user_id, "is_bot": False, "username": "someone",
                                     "first_name": "Someone"},
                            "chat": {"id": chat_id, "type": "private"},
                            "date": 0,
                            "text": said,
                        },
                    }]
            else:
                next_id += 1
                result = {"message_id": next_id, "chat": {"id": chat_id, "type": "private"}}

        payload = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
