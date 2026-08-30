#!/usr/bin/env python3
"""An MCP server behind an authorization server, for verify-mcp-oauth.sh.

Both halves in one process, because what is being checked is one chain: the
refusal names where the metadata is, the metadata names the authorization
server, the authorization server issues a client and then a token, and the
token gets the tools listed. A stub that skipped a link would pass while the
link was broken.

It is deliberately not lenient. A client that sends the wrong redirect_uri,
loses the PKCE verifier, or presents a retired refresh token is refused,
because a stub that accepts anything proves nothing about the client.
"""

import base64
import hashlib
import json
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

STATE = {
    "clients": {},        # client_id -> registration
    "codes": {},          # code -> {client_id, challenge, redirect_uri}
    "access": set(),      # live access tokens
    "refresh": {},        # refresh token -> client_id
    "retired": set(),     # refresh tokens already spent, to prove rotation
}
LOCK = threading.Lock()

ORIGIN = ""  # filled in once the port is known


def pkce_ok(verifier, challenge):
    digest = hashlib.sha256(verifier.encode()).digest()
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode() == challenge


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        # To stderr, where the check keeps it. What went wrong in one of these
        # flows is almost always visible as the order of these lines.
        print("stub: " + (fmt % args), file=sys.stderr, flush=True)

    def send(self, code, body=b"", headers=None):
        self.send_response(code)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def send_json(self, code, payload, headers=None):
        body = json.dumps(payload).encode()
        merged = {"content-type": "application/json"}
        merged.update(headers or {})
        self.send(code, body, merged)

    # ---- discovery -------------------------------------------------------

    def do_GET(self):
        path = urlparse(self.path).path

        # RFC 9728. The path is the well-known segment with the resource's
        # own path appended, not the resource path with the segment appended.
        if path == "/.well-known/oauth-protected-resource/mcp":
            return self.send_json(200, {
                "resource": f"{ORIGIN}/mcp",
                "authorization_servers": [ORIGIN],
                "scopes_supported": ["mcp:read"],
            })

        if path in ("/.well-known/oauth-authorization-server",
                    "/.well-known/openid-configuration"):
            return self.send_json(200, {
                "issuer": ORIGIN,
                "authorization_endpoint": f"{ORIGIN}/authorize",
                "token_endpoint": f"{ORIGIN}/token",
                "registration_endpoint": f"{ORIGIN}/register",
                "response_types_supported": ["code"],
                "grant_types_supported": ["authorization_code", "refresh_token"],
                "code_challenge_methods_supported": ["S256"],
                "scopes_supported": ["mcp:read", "offline_access"],
                "token_endpoint_auth_methods_supported": ["none"],
                # RFC 9207. Advertised because the redirect below carries iss,
                # and a client is required to reject an iss it was not told to
                # expect — which is what caught this stub sending one without
                # saying so.
                "authorization_response_iss_parameter_supported": True,
            })

        if path == "/authorize":
            return self.authorize()

        return self.send(404)

    def authorize(self):
        query = parse_qs(urlparse(self.path).query)
        one = lambda name: (query.get(name) or [""])[0]

        client_id = one("client_id")
        with LOCK:
            registered = STATE["clients"].get(client_id)
        if not registered:
            return self.send(400, b"unknown client")

        redirect = one("redirect_uri")
        if redirect not in registered["redirect_uris"]:
            return self.send(400, b"redirect_uri was not registered")
        if one("code_challenge_method") != "S256":
            return self.send(400, b"pkce is required")

        code = uuid.uuid4().hex
        with LOCK:
            STATE["codes"][code] = {
                "client_id": client_id,
                "challenge": one("code_challenge"),
                "redirect_uri": redirect,
            }

        # iss per RFC 9207, so the client can tell whose answer this is.
        back = f"{redirect}?code={code}&state={one('state')}&iss={ORIGIN}"
        return self.send(302, b"", {"location": back})

    # ---- registration, token, and the resource itself ---------------------

    def do_POST(self):
        path = urlparse(self.path).path
        length = int(self.headers.get("content-length") or 0)
        body = self.rfile.read(length) if length else b""

        if path == "/register":
            return self.register(body)
        if path == "/token":
            return self.token(body)
        if path == "/mcp":
            return self.mcp(body)
        return self.send(404)

    def register(self, body):
        asked = json.loads(body or b"{}")
        if not asked.get("redirect_uris"):
            return self.send_json(400, {"error": "invalid_redirect_uri"})

        # 2026-07-28 requires this of a client redirecting to loopback. An
        # authorization server that assumed "web" would refuse the http
        # redirect a native application has to use.
        if asked.get("application_type") != "native":
            return self.send_json(400, {
                "error": "invalid_client_metadata",
                "error_description": "a loopback redirect needs application_type native",
            })

        client_id = "client-" + uuid.uuid4().hex[:8]
        with LOCK:
            STATE["clients"][client_id] = asked
        return self.send_json(201, {
            "client_id": client_id,
            "redirect_uris": asked["redirect_uris"],
            "grant_types": asked.get("grant_types", ["authorization_code"]),
            "token_endpoint_auth_method": "none",
        })

    def token(self, body):
        form = parse_qs(body.decode())
        one = lambda name: (form.get(name) or [""])[0]
        grant = one("grant_type")

        if grant == "authorization_code":
            with LOCK:
                issued = STATE["codes"].pop(one("code"), None)
            if not issued:
                return self.send_json(400, {"error": "invalid_grant"})
            if one("redirect_uri") != issued["redirect_uri"]:
                return self.send_json(400, {"error": "invalid_grant",
                                            "error_description": "redirect_uri does not match"})
            if not pkce_ok(one("code_verifier"), issued["challenge"]):
                return self.send_json(400, {"error": "invalid_grant",
                                            "error_description": "pkce verifier does not match"})
            return self.issue(issued["client_id"])

        if grant == "refresh_token":
            presented = one("refresh_token")
            with LOCK:
                if presented in STATE["retired"]:
                    # Rotation, enforced. A client that kept presenting its
                    # first refresh token finds out here.
                    return self.send_json(400, {"error": "invalid_grant",
                                                "error_description": "that refresh token was rotated"})
                client_id = STATE["refresh"].pop(presented, None)
                if not client_id:
                    return self.send_json(400, {"error": "invalid_grant"})
                STATE["retired"].add(presented)
            return self.issue(client_id)

        return self.send_json(400, {"error": "unsupported_grant_type"})

    def issue(self, client_id):
        access = "access-" + uuid.uuid4().hex
        refresh = "refresh-" + uuid.uuid4().hex
        with LOCK:
            STATE["access"].add(access)
            STATE["refresh"][refresh] = client_id
        return self.send_json(200, {
            "access_token": access,
            "refresh_token": refresh,
            "token_type": "Bearer",
            "expires_in": 3600,
            "scope": "mcp:read",
        })

    def mcp(self, body):
        presented = (self.headers.get("authorization") or "").removeprefix("Bearer ")
        with LOCK:
            allowed = presented in STATE["access"]
        if not allowed:
            print(f"stub: /mcp refused, token={presented[:16] or '(none)'}",
                  file=sys.stderr, flush=True)
            # The challenge names where the metadata is, which is the first
            # link in the chain.
            return self.send(401, b"", {
                "WWW-Authenticate":
                    f'Bearer resource_metadata="{ORIGIN}/.well-known/oauth-protected-resource/mcp"',
            })

        asked = json.loads(body or b"{}")
        method, call = asked.get("method"), asked.get("id")

        if method == "initialize":
            return self.rpc(call, {
                "protocolVersion": "2026-07-28",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "stub-books", "version": "1.0.0"},
            })
        if method == "tools/list":
            return self.rpc(call, {"tools": [{
                "name": "shelf",
                "description": "Says what is on the shelf.",
                "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
            }]})
        if method and method.startswith("notifications/"):
            return self.send(202)
        return self.rpc(call, None, error={"code": -32601, "message": f"no method {method}"})

    def rpc(self, call, result, error=None):
        payload = {"jsonrpc": "2.0", "id": call}
        payload["error" if error else "result"] = error or result
        return self.send_json(200, payload)


def main():
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    global ORIGIN
    ORIGIN = f"http://127.0.0.1:{server.server_address[1]}"

    # Told to whoever started this, before anything else, so the check does
    # not have to guess a port.
    print(ORIGIN, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
