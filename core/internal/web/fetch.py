"""Fetch one page with CloakBrowser and print what it says as JSON.

This runs as a child process of agentd, one process per fetch. It is written
to be boring: read a request on stdin, print one JSON object on stdout, exit.
Anything it cannot do is an error object rather than a partial answer, because
a caller that cannot tell a blank page from a failed fetch will report the
first as the second.

The context is anonymous and thrown away when the process exits. It is never
the operator's own browser profile: a page fetched because a model decided to
must not be fetched carrying the operator's sessions, or "read this page"
becomes "read this page as me".
"""

import ipaddress
import json
import socket
import sys

EXIT_REFUSED = 3
EXIT_FAILED = 4

# Resolution is cached for the life of one fetch. A page makes many requests to
# the same few hosts, and asking the resolver each time adds latency to every
# image on the page.
_resolved: dict[str, bool] = {}


def is_private_host(host: str) -> bool:
    """Report whether a hostname resolves anywhere this agent must not reach."""
    if host in _resolved:
        return _resolved[host]

    private = False
    try:
        for family, _, _, _, address in socket.getaddrinfo(host, None):
            del family
            try:
                parsed = ipaddress.ip_address(address[0])
            except ValueError:
                continue
            # link_local covers 169.254.169.254, where a cloud host keeps the
            # credentials of everything running on it.
            if (
                parsed.is_private
                or parsed.is_loopback
                or parsed.is_link_local
                or parsed.is_reserved
                or parsed.is_multicast
                or parsed.is_unspecified
            ):
                private = True
                break
    except socket.gaierror:
        # A name that does not resolve is not reachable either. Letting the
        # browser try is harmless and produces a better error than guessing.
        private = False

    _resolved[host] = private
    return private


def guard(route, request) -> None:
    """Abort any request leaving for an address this agent must not reach.

    The guard lives here rather than in the caller because redirects are
    resolved inside the browser. A caller that checks only the address it was
    given is checking the one hop an attacker controls least.
    """
    from urllib.parse import urlparse

    host = urlparse(request.url).hostname
    if host and is_private_host(host):
        route.abort()
        return
    route.continue_()


def extract_links(page, limit: int) -> list[dict]:
    """Collect the destinations a page offers, deduplicated, in document order."""
    raw = page.eval_on_selector_all(
        "a[href]",
        """els => els.map(el => ({
            text: (el.innerText || el.textContent || '').trim().slice(0, 200),
            url: el.href
        }))""",
    )

    seen: set[str] = set()
    links = []
    for link in raw:
        url = link.get("url") or ""
        if not url.startswith(("http://", "https://")) or url in seen:
            continue
        seen.add(url)
        links.append({"text": link.get("text") or "", "url": url})
        if len(links) >= limit:
            break
    return links


def fetch(request: dict) -> dict:
    import cloakbrowser

    url = request["url"]
    timeout_ms = int(request.get("timeout_ms", 30000))
    max_links = int(request.get("max_links", 50))

    with cloakbrowser.launch_context(headless=True) as context:
        page = context.new_page()
        page.route("**/*", guard)

        response = page.goto(url, wait_until="domcontentloaded", timeout=timeout_ms)
        if response is None:
            raise RuntimeError("the page did not load")

        # Scripted pages render after domcontentloaded. Waiting for the network
        # to settle catches most of them; a page that never settles still has
        # whatever it rendered by the deadline, which beats returning nothing.
        try:
            page.wait_for_load_state("networkidle", timeout=min(timeout_ms, 10000))
        except Exception:
            pass

        return {
            "status": response.status,
            "final_url": page.url,
            "title": page.title(),
            "text": page.inner_text("body"),
            "links": extract_links(page, max_links),
        }


def main() -> int:
    try:
        request = json.load(sys.stdin)
    except json.JSONDecodeError as err:
        print(json.dumps({"error": f"unreadable request: {err}"}), file=sys.stdout)
        return EXIT_FAILED

    try:
        print(json.dumps(fetch(request), ensure_ascii=False))
    except ImportError as err:
        print(json.dumps({"error": f"cloakbrowser is not installed: {err}"}))
        return EXIT_REFUSED
    except Exception as err:
        # The class name is worth keeping: a caller reading "TimeoutError" can
        # say something useful, and reading only its message often cannot.
        print(json.dumps({"error": f"{type(err).__name__}: {err}"}))
        return EXIT_FAILED

    return 0


if __name__ == "__main__":
    sys.exit(main())
