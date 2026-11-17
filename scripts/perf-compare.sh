#!/usr/bin/env bash
set -euo pipefail

BASE=$1
AFTER=$2

python3 - <<PY
import json, sys
base=json.load(open('$BASE'))
after=json.load(open('$AFTER'))
print(f"{'Bench':40} {'ns/op Δ%':>10} {'B/op Δ%':>10} {'allocs Δ%':>10}")
for b,a in zip(base['benches'], after['benches']):
    nb=b['bench']
    dn= (a['ns_per_op']-b['ns_per_op'])/b['ns_per_op']*100
    db= (a['bytes_per_op']-b['bytes_per_op'])/b['bytes_per_op']*100 if b['bytes_per_op'] else 0
    da= (a['allocs_per_op']-b['allocs_per_op'])/b['allocs_per_op']*100 if b['allocs_per_op'] else 0
    print(f"{nb:40} {dn:10.1f} {db:10.1f} {da:10.1f}")
PY
