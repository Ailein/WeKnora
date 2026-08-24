# Togather Distance MCP Server

Stdio MCP server exposing one tool, `togather_nearest_store`: resolve a guest
location (address / postcode / Google Maps link / WhatsApp location pin /
`lat,lng`) and rank the 13 Togather Cafe outlets in `stores.json` by
straight-line distance. Geocoding via OpenStreetMap Nominatim (free, no key;
~1 req/s fair-use — switch to a paid geocoder if traffic grows).

Zero third-party dependencies (Python stdlib only). WeKnora disables stdio
MCP for security, so this runs as its own compose service
(`togather-distance`, python:3.11-slim) speaking MCP Streamable HTTP.

## Register in WeKnora

1. `docker compose up -d togather-distance`
2. Add `togather-distance` to `SSRF_WHITELIST` in `.env` (comma-separated)
   and recreate the app container so it takes effect.
3. MCP service settings — Transport: `http-streamable`,
   URL: `http://togather-distance:9310/mcp`.

`integrations/togather-cs/setup.py` does step 3 automatically. The
registered tool name becomes `mcp_<service>_togather_nearest_store` (service
name sanitized: `togather-distance` →
`mcp_togather_distance_togather_nearest_store`).

## Manual test

```bash
python3 nearest.py "Bandar Puchong Jaya 47170"          # geocode + rank
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"togather_nearest_store","arguments":{"origin":"3.2105,101.6517"}}}' \
  | python3 server.py
```

## Updating stores

Edit `stores.json` (id/name/state/address/lat/lng/maps_url per store). The
server reads it on every call — no restart needed.
