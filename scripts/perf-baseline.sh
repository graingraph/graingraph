#!/usr/bin/env bash
set -euo pipefail

PKG=${1:-./cypher/interp}
BENCHES=${2:-BenchmarkDecodeProps}

OUTDIR="perf"
mkdir -p "$OUTDIR"

DATE=$(date +%Y%m%d-%H%M%S)
FILE="$OUTDIR/baseline-${DATE}.json"

echo "Running benchmarks for $PKG"
CGO_ENABLED=0 go test -run=^$ -bench="$BENCHES" "$PKG" -benchtime=5s -json > "$OUTDIR/tmp.json"

# Simple parser: collect bench results
python3 - <<PY
import json, sys, re
path = sys.argv[1]
out = sys.argv[2]
results = []
with open(path) as f:
    for line in f:
        obj = json.loads(line)
        if obj.get('Action') == 'bench':
            results.append({
                'bench': obj.get('Name'),
                'ns_per_op': obj.get('NsPerOp'),
                'bytes_per_op': obj.get('BytesPerOp'),
                'allocs_per_op': obj.get('AllocsPerOp')
            })
with open(out, 'w') as f:
    json.dump({'package': '$PKG', 'benches': results}, f, indent=2)
print(f"Wrote {out}")
PY "$OUTDIR/tmp.json" "$FILE"

rm "$OUTDIR/tmp.json"
