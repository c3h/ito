#!/usr/bin/env bash
# Seeds the demo store used by docs/demo.tape: an "atlas" maps project with
# Issues across every status (each with a body), plus two Batches whose members
# form derived Waves — including a conflicts_with edge that splits two otherwise
# parallel members into different Waves. Re-run before regenerating the GIF:
#
#   ./docs/demo-seed.sh && vhs docs/demo.tape
#
# ITO points at the binary the tape runs (the repo-root ./ito — build it with
# "go build -o ito ."); override to test another build. The store lives in a
# throwaway ITO_HOME, never your real ~/.ito.
set -euo pipefail

ITO="${ITO:-$(cd "$(dirname "$0")/.." && pwd)/ito}"
export ITO_HOME=/tmp/ito-demo-home
PROJECT_DIR=/tmp/atlas

rm -rf "$ITO_HOME" "$PROJECT_DIR"
mkdir -p "$PROJECT_DIR"
cd "$PROJECT_DIR"

$ITO init --name atlas --prefix ATLAS >/dev/null

# --- Unbatched Issues: one per status, the daily flow you watch in the Digest.
$ITO new --title "Fix marker drift at high zoom" --status in_progress --priority urgent --label bug \
  --body "## Repro
Zoom past level 18 and pan — markers lag the basemap by a few pixels.

## Cause
Projected marker coordinates are cached, but the cache key ignores the zoom delta.

## Fix
Key the projection cache on (lat, lng, zoom)." >/dev/null

$ITO new --title "Ship the offline tile cache" --status in_review --priority high --label feature \
  --body "A bounded, disk-backed LRU of recently viewed tiles so the map survives a flaky connection.

- [x] Disk-backed store
- [x] Eviction by age and size
- [ ] Settings toggle" >/dev/null

$ITO new --title "Migrate raster layers to vector tiles" --status todo --priority high --label feature \
  --body "Move the basemap off raster PNGs to vector tiles: crisp at every zoom, smaller payloads." >/dev/null

$ITO new --title "Document the projection API" --status backlog --priority low --label docs \
  --body "Public reference for project() / unproject() and the list of supported coordinate systems." >/dev/null

$ITO new --title "Retire the legacy raster renderer" --status done --priority medium --label chore \
  --body "Delete the old PNG renderer once vector tiles ship." >/dev/null

# --- Batch "search-revamp": a feature, three members in a linear chain → Waves 1·2·3.
$ITO batch new search-revamp >/dev/null
S1=$($ITO new --title "Design the place index schema" --batch search-revamp --status todo --priority high --label feature \
  --body "Inverted-index layout for place names, plus the ranking signals it needs to carry.")
S2=$($ITO new --title "Build the geosearch endpoint" --batch search-revamp --status backlog --priority medium --label feature \
  --body "Turn a query string into ranked place hits: GET /search?q=<text>&near=<lat,lng>.

Returns the top matches by relevance, each with a centroid and a bounding box. Needs the place index schema in place first, so it sits in Wave 2 of the search-revamp batch.")
S3=$($ITO new --title "Add typeahead to the search bar" --batch search-revamp --status backlog --priority medium --label feature \
  --body "Debounced typeahead that calls the geosearch endpoint as you type.")
$ITO edit "$S2" --block "$S1" >/dev/null
$ITO edit "$S3" --block "$S2" >/dev/null

# --- Batch "tiles-perf": a refactor, 1/3 done; a conflicts_with edge keeps the two
#     render-loop rewrites out of the same Wave (tie-break: priority, then ID).
$ITO batch new tiles-perf >/dev/null
$ITO new --title "Profile the render loop" --batch tiles-perf --status done --priority medium --label refactor \
  --body "Flame graph of a pan at zoom 16. Projection and tile decode dominate." >/dev/null
T2=$($ITO new --title "Cache projected coordinates" --batch tiles-perf --status todo --priority high --label refactor \
  --body "Memoize the per-tile projection — the biggest hotspot in the profile.")
T3=$($ITO new --title "Parallelize tile decode" --batch tiles-perf --status todo --priority medium --label refactor \
  --body "Decode tiles on a worker pool. Rewrites the same render loop as the projection cache, so the two cannot run in parallel.")
$ITO edit "$T3" --conflict "$T2" >/dev/null

echo "Seeded atlas into $ITO_HOME (project dir $PROJECT_DIR)."
