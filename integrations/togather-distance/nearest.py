"""Nearest Togather outlet lookup.

Resolves a guest-supplied origin (address, postcode, Google Maps share link,
WhatsApp location pin "lat,lng") to coordinates, then ranks the 13 stores in
stores.json by great-circle distance. Geocoding uses OpenStreetMap Nominatim
(free, no API key); swap in a routing API here if driving distance is ever
needed.

Library module: raises GeocodeError instead of exiting so the MCP server can
turn failures into tool errors. Run as a script for a quick manual check:

    python3 nearest.py "Jalan Kenari 8, Puchong"
"""

from __future__ import annotations

import json
import math
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

NOMINATIM_URL = "https://nominatim.openstreetmap.org/search"
USER_AGENT = "weknora-togather-distance/1.0"
EARTH_RADIUS_KM = 6371.0088
STORES_FILE = Path(__file__).parent / "stores.json"

# Malaysia bounding box (Peninsular + Sabah + Sarawak) with a small buffer.
MALAYSIA_BBOX = (0.85, 99.5, 7.5, 119.5)  # (min_lat, min_lng, max_lat, max_lng)
# Wider Southeast Asia box: tolerated so Singapore / south-Thailand guests
# still get an answer, just with an honest "this may be far" note.
SEA_BBOX = (-2.0, 95.0, 10.0, 122.0)

_STREET_PREFIXES = (
    "Jalan", "Jln", "Lorong", "Lrg", "Persiaran", "Lebuh", "Lebuhraya",
    "Tingkat", "Solok", "Medan", "Lengkok", "Susur",
)


class GeocodeError(Exception):
    """Origin could not be resolved to usable coordinates."""


def _in_bbox(lat: float, lng: float, bbox: tuple) -> bool:
    min_lat, min_lng, max_lat, max_lng = bbox
    return min_lat <= lat <= max_lat and min_lng <= lng <= max_lng


def _haversine_km(lat1: float, lng1: float, lat2: float, lng2: float) -> float:
    phi1, phi2 = math.radians(lat1), math.radians(lat2)
    dphi = math.radians(lat2 - lat1)
    dlam = math.radians(lng2 - lng1)
    a = math.sin(dphi / 2) ** 2 + math.cos(phi1) * math.cos(phi2) * math.sin(dlam / 2) ** 2
    return 2 * EARTH_RADIUS_KM * math.asin(math.sqrt(a))


def _parse_latlng(text: str):
    m = re.fullmatch(r"\s*(-?\d+\.?\d*)\s*,\s*(-?\d+\.?\d*)\s*", text)
    if not m:
        return None
    lat, lng = float(m.group(1)), float(m.group(2))
    if -90 <= lat <= 90 and -180 <= lng <= 180:
        return lat, lng
    return None


def _resolve_maps_url(url: str):
    """Follow a Google Maps short link and pull coordinates from the long URL."""
    try:
        req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": USER_AGENT})
        with urllib.request.urlopen(req, timeout=10) as resp:
            final_url = resp.geturl()
    except Exception:
        return None
    m = re.search(r"!3d(-?\d+\.\d+)!4d(-?\d+\.\d+)", final_url)
    if m:
        return float(m.group(1)), float(m.group(2))
    m = re.search(r"@(-?\d+\.\d+),(-?\d+\.\d+)", final_url)
    if m:
        return float(m.group(1)), float(m.group(2))
    return None


def _nominatim_request(params: dict) -> list:
    base = {"format": "json", "limit": 5, "addressdetails": 1}
    base.update(params)
    qs = urllib.parse.urlencode(base)
    req = urllib.request.Request(f"{NOMINATIM_URL}?{qs}", headers={"User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        print(f"[warn] Nominatim request failed: {e}", file=sys.stderr)
        return []


def _pick_first_in_my(results: list):
    for r in results:
        try:
            lat, lng = float(r["lat"]), float(r["lon"])
        except (KeyError, TypeError, ValueError):
            continue
        if _in_bbox(lat, lng, MALAYSIA_BBOX):
            return lat, lng, r.get("display_name", "")
    return None


def _extract_street(text: str):
    for part in (p.strip() for p in text.split(",")):
        for prefix in _STREET_PREFIXES:
            if re.match(rf"^{prefix}\b", part, re.IGNORECASE):
                return part
    return None


def _extract_postcode(text: str):
    m = re.search(r"\b(\d{5})\b", text)
    return m.group(1) if m else None


def _geocode_nominatim(query: str):
    """Try several query shapes; Malaysian addresses are frequently written
    with inconsistent Taman/state fields, so structured street+postcode and
    postcode-only fallbacks recover most of what full-text misses."""
    strategies = [("full-text (MY)", {"q": query, "countrycodes": "my"})]
    street = _extract_street(query)
    postcode = _extract_postcode(query)
    if street and postcode:
        strategies.append((
            f"structured (street + postcode={postcode})",
            {"street": street, "postalcode": postcode, "country": "Malaysia"},
        ))
    elif street:
        strategies.append(("structured (street only)", {"street": street, "country": "Malaysia"}))
    if postcode:
        strategies.append((f"postcode-only ({postcode})", {"q": f"{postcode} Malaysia", "countrycodes": "my"}))
    strategies.append(("full-text (no country filter)", {"q": query}))

    for label, params in strategies:
        hit = _pick_first_in_my(_nominatim_request(params))
        if hit:
            print(f"[geocode] strategy: {label}", file=sys.stderr)
            return hit
    return None


def resolve_origin(text: str):
    """Return ((lat, lng), source_label) or raise GeocodeError."""
    text = text.strip()

    coords = _parse_latlng(text)
    if coords:
        if not _in_bbox(*coords, SEA_BBOX):
            raise GeocodeError(
                f"coordinates ({coords[0]:.4f}, {coords[1]:.4f}) are far outside "
                "Malaysia/Southeast Asia; check for a swapped lat/lng or a wrong link"
            )
        return coords, "raw lat,lng"

    if text.startswith(("http://", "https://")) and ("goo.gl" in text or "google.com/maps" in text):
        coords = _resolve_maps_url(text)
        if coords:
            if not _in_bbox(*coords, SEA_BBOX):
                raise GeocodeError("the Google Maps link points far outside Malaysia/Southeast Asia")
            return coords, "Google Maps URL"
        print("[warn] could not extract coords from URL; falling back to Nominatim", file=sys.stderr)

    hit = _geocode_nominatim(text)
    if hit:
        lat, lng, display = hit
        print(f"[matched] {display}", file=sys.stderr)
        return (lat, lng), "Nominatim (OpenStreetMap, MY only)"

    raise GeocodeError(f"could not geocode {text!r}")


def _format_km(km: float) -> str:
    if km < 1:
        return f"{round(km * 1000)} m"
    return f"{km:.2f} km"


def find_nearest(origin: str, limit: int = 3) -> dict:
    """Rank all stores by distance from origin. Returns the full result dict;
    raises GeocodeError when the origin cannot be resolved."""
    limit = max(1, min(13, int(limit)))
    (lat, lng), source = resolve_origin(origin)

    with STORES_FILE.open() as f:
        data = json.load(f)

    results = []
    for s in data["stores"]:
        d = _haversine_km(lat, lng, s["lat"], s["lng"])
        results.append({
            "id": s["id"],
            "name": s["name"],
            "state": s["state"],
            "address": s["address"],
            "lat": s["lat"],
            "lng": s["lng"],
            "maps_url": s["maps_url"],
            "distance_km": round(d, 3),
        })
    results.sort(key=lambda r: r["distance_km"])

    return {
        "user": {"lat": lat, "lng": lng, "source": source, "input": origin},
        "brand": data["brand"],
        "results": results[:limit],
    }


def build_summary(data: dict) -> str:
    """Render the ranked list as text for the agent. Wording mirrors the
    battle-tested openclaw plugin: the nearest outlet is called out on its own
    so the model never swaps names/distances between ranked entries."""
    user = data["user"]
    nearest = data["results"]
    lines = [
        f"Origin: {user['input']}",
        f"Resolved location: {user['lat']:.6f}, {user['lng']:.6f} ({user['source']})",
        "Distance type: straight-line estimate. Actual driving distance/time may differ.",
    ]
    if nearest:
        first = nearest[0]
        lines += [
            "",
            f"Nearest outlet (use this for a one-outlet answer): {first['name']} ({first['state']}) - {_format_km(first['distance_km'])}",
            f"Nearest outlet address: {first['address']}",
            f"Nearest outlet maps: {first['maps_url']}",
            "Do not swap store names, distances, addresses, or map links between ranked outlets.",
        ]
    lines += ["", f"Ranked nearby outlets ({len(nearest)}):"]
    for i, store in enumerate(nearest, 1):
        lines += [
            f"{i}. {store['name']} ({store['state']}) - {_format_km(store['distance_km'])}",
            f"   Address: {store['address']}",
            f"   Maps: {store['maps_url']}",
        ]
    return "\n".join(lines)


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit('usage: nearest.py "<address | maps url | lat,lng>" [limit]')
    lim = int(sys.argv[2]) if len(sys.argv) > 2 else 3
    try:
        print(build_summary(find_nearest(sys.argv[1], lim)))
    except GeocodeError as e:
        sys.exit(f"[error] {e}")
