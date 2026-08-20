"""MCP tools backed by the Go scorecard API.

Tools: list_services, get_scorecard, why_not_ready
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request

from mcp.server import MCPServer

API = os.environ.get("SCORECARD_API", "http://127.0.0.1:8091")
app = MCPServer("cortex-scorecard")


def _get(path: str, **params: str) -> str:
    q = urllib.parse.urlencode({k: v for k, v in params.items() if v})
    url = f"{API}{path}" + (f"?{q}" if q else "")
    try:
        with urllib.request.urlopen(url, timeout=15) as resp:
            return resp.read().decode()
    except urllib.error.URLError as exc:
        return json.dumps({"error": str(exc), "url": url})


@app.tool()
def list_services() -> str:
    """List all services in the context graph (nodes + owner/infra/repo edges)."""
    return _get("/services")


@app.tool()
def get_scorecard(service: str = "") -> str:
    """Get production-readiness scorecard (0-100) for one service, or all if omitted.

    Args:
        service: Optional service name. Empty string returns every scorecard.
    """
    return _get("/scorecard", service=service)


@app.tool()
def why_not_ready(service: str = "") -> str:
    """Explain why a service is not production-ready, with concrete gap details.

    Args:
        service: Optional service name. Empty string lists all not-ready services.
    """
    return _get("/why_not_ready", service=service)


if __name__ == "__main__":
    app.run()
