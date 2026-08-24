#!/usr/bin/env python3
"""Togather nearest-store MCP server.

Zero third-party dependencies (stdlib only). Two transports:

  python3 server.py               # stdio: newline-delimited JSON-RPC 2.0
  python3 server.py --http 9310   # MCP Streamable HTTP (stateless):
                                  #   POST /mcp with a JSON-RPC request →
                                  #   application/json response; notifications
                                  #   → 202; GET → 405 (no server stream).

WeKnora disables stdio MCP for security, so production runs the HTTP mode in
its own compose container (service `togather-distance`) and registers:

    transport: http-streamable
    url:       http://togather-distance:9310/mcp

(add `togather-distance` to SSRF_WHITELIST in .env so the app may call it).

Exposes one tool, togather_nearest_store. stderr is the log channel.
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from nearest import GeocodeError, build_summary, find_nearest

SERVER_INFO = {"name": "togather-distance", "version": "1.0.0"}
DEFAULT_PROTOCOL_VERSION = "2024-11-05"

TOOL = {
    "name": "togather_nearest_store",
    "description": (
        "Find the nearest Togather Cafe outlet from a customer address, "
        "postcode, Google Maps URL, WhatsApp location pin coordinates, or "
        "lat,lng. Returns straight-line distance estimates, sorted "
        "nearest-first."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "origin": {
                "type": "string",
                "description": (
                    "Guest location: address, postcode, Google Maps URL, "
                    "WhatsApp location coordinates, or lat,lng."
                ),
            },
            "limit": {
                "type": "number",
                "minimum": 1,
                "maximum": 13,
                "default": 3,
                "description": "Number of nearest outlets to return. Default 3, max 13.",
            },
        },
        "required": ["origin"],
    },
}


def tool_error(message: str) -> dict:
    return {"content": [{"type": "text", "text": message}], "isError": True}


def call_nearest_store(arguments: dict) -> dict:
    origin = str(arguments.get("origin") or "").strip()
    if not origin:
        return tool_error(
            "origin is required. Provide an address, postcode, Google Maps "
            "URL, WhatsApp location coordinates, or lat,lng."
        )
    try:
        limit = int(arguments.get("limit") or 3)
    except (TypeError, ValueError):
        limit = 3
    try:
        data = find_nearest(origin, limit)
    except GeocodeError as e:
        return tool_error(
            "Could not find a nearest outlet for that location. Ask the guest "
            "for a clearer address, postcode, Google Maps link, or WhatsApp "
            f"location pin.\n\nTool error: {e}"
        )
    except Exception as e:  # network hiccups etc. — keep the agent informed
        return tool_error(f"Nearest-store lookup failed: {e}")
    return {"content": [{"type": "text", "text": build_summary(data)}]}


def handle_request(method: str, params: dict):
    """Return the JSON-RPC result for a request, or raise ValueError for an
    unknown method."""
    if method == "initialize":
        # Echo the client's protocol version when given; mcp-go accepts the
        # server's choice either way.
        return {
            "protocolVersion": params.get("protocolVersion") or DEFAULT_PROTOCOL_VERSION,
            "capabilities": {"tools": {}},
            "serverInfo": SERVER_INFO,
        }
    if method == "ping":
        return {}
    if method == "tools/list":
        return {"tools": [TOOL]}
    if method == "tools/call":
        name = params.get("name")
        if name != TOOL["name"]:
            raise ValueError(f"unknown tool: {name}")
        return call_nearest_store(params.get("arguments") or {})
    if method in ("resources/list", "resources/templates/list"):
        return {"resources": []}
    if method == "prompts/list":
        return {"prompts": []}
    raise ValueError(f"method not found: {method}")


def dispatch(msg: dict):
    """Handle one JSON-RPC message; None for notifications (no response)."""
    msg_id = msg.get("id")
    method = msg.get("method") or ""
    if msg_id is None:
        return None
    try:
        result = handle_request(method, msg.get("params") or {})
        return {"jsonrpc": "2.0", "id": msg_id, "result": result}
    except ValueError as e:
        return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": str(e)}}
    except Exception as e:
        return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32603, "message": f"internal error: {e}"}}


def stdio_main() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as e:
            print(f"[warn] bad JSON on stdin: {e}", file=sys.stderr)
            continue
        response = dispatch(msg)
        if response is not None:
            print(json.dumps(response, ensure_ascii=False), flush=True)


class StreamableHTTPHandler(BaseHTTPRequestHandler):
    """Stateless MCP Streamable HTTP endpoint: every POSTed request gets a
    single application/json response (no SSE stream, no session ids)."""

    protocol_version = "HTTP/1.1"

    def _send(self, code: int, body: bytes = b"", content_type: str = "application/json"):
        self.send_response(code)
        if body:
            self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length") or 0)
            msg = json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception as e:
            self._send(400, json.dumps({
                "jsonrpc": "2.0", "id": None,
                "error": {"code": -32700, "message": f"parse error: {e}"},
            }).encode("utf-8"))
            return
        # Batches are not part of the current MCP spec; handle single messages.
        response = dispatch(msg)
        if response is None:
            self._send(202)
            return
        self._send(200, json.dumps(response, ensure_ascii=False).encode("utf-8"))

    def do_GET(self):
        # No server-initiated stream in stateless mode.
        self._send(405)

    def do_DELETE(self):
        # Session termination — nothing to clean up.
        self._send(200)

    def log_message(self, fmt, *args):  # route access logs to stderr
        print("[http] " + fmt % args, file=sys.stderr)


def http_main(port: int) -> None:
    server = ThreadingHTTPServer(("0.0.0.0", port), StreamableHTTPHandler)
    print(f"[http] togather-distance MCP listening on :{port}", file=sys.stderr)
    server.serve_forever()


if __name__ == "__main__":
    if len(sys.argv) >= 3 and sys.argv[1] == "--http":
        http_main(int(sys.argv[2]))
    else:
        stdio_main()
