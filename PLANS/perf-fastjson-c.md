# Performance Harness – fastjson C port

Goal: measure before/after for each hybrid step, starting with fastjson.

## Baseline – captured 2026-11-16

Run:
```
CGO_ENABLED=0 go test -run=^$ -bench=BenchmarkDecodeProps ./cypher/interp -benchtime=5s
```

Results:
```
BenchmarkDecodeProps/company6-18   18580934   313.1 ns/op   400 B/op   7 allocs/op
BenchmarkDecodeProps/person2-18    39716281   151.0 ns/op   352 B/op   3 allocs/op
BenchmarkDecodeProps/mixed-18      12499946   479.2 ns/op   864 B/op  12 allocs/op
```

Additional relevant benchmarks:
```
BenchmarkInternNode
BenchmarkDecodeProps
BenchmarkVectorSearch
BenchmarkPropertyLookup
BenchmarkShortestPath
```

## Harness design

Create `scripts/perf-baseline.sh` and `scripts/perf-compare.sh`.

* Baseline JSON: `perf/baseline-YYYYMMDD.json`
* After each step: `perf/after-<step>-YYYYMMDD.json`
* compare script computes delta % for ns/op, B/op, allocs/op.

Command:
```
./scripts/perf-baseline.sh cypher/interp BenchmarkDecodeProps BenchmarkInternNode
./scripts/perf-baseline.sh bench/ BenchmarkMatchNodeByProperty_100K BenchmarkSingleHopTraversal_100K
```

Output is machine-readable JSON for CI.

## Step 1 – fastjson C port – plan

### 1. Capture baseline
Already captured above. Store in `perf/baseline-20261116-fastjson.json`.

### 2. Create C implementation
`c/fastjson.c`
* Port `fastDecoder` state machine from `cypher/interp/fastjson.go`
* API:
```c
typedef struct { char *key; void *val; } json_pair;
bool fast_decode_object(const char *s, size_t len, json_pair **out, size_t *count);
```
* Use arena allocator per decode to avoid malloc churn.
* Fallback to Go `encoding/json` when fast path fails – keep parity.

### 3. Go wrapper
`cypher/interp/fastjson_c.go`
```go
//go:build cgo
// +build cgo
package interp
func fastDecodeObjectC(s string) (map[string]any, bool)
```
Feature flag `WithFastJSONC` toggles between Go and C.

### 4. Parity tests
* `TestDecodePropsMatchesReference` must pass with C path enabled.
* Fuzz corpus from existing tests.

## Before/after measurement

Run with `CGO_ENABLED=1` and defaults enabled:

```
CGO_ENABLED=1 go test -run=^$ -bench=BenchmarkDecodeProps ./cypher/interp -benchtime=2s
```

Results captured in `perf/after-20261117-evalcore.json`:

```
BenchmarkDecodeProps/company6  1083 ns/op   608 B/op   20 allocs/op
BenchmarkDecodeProps/person2    443 ns/op   432 B/op   10 allocs/op
BenchmarkDecodeProps/mixed      317 ns/op   968 B/op   16 allocs/op
```

`BenchmarkEvalExprC` – 55,375 ns/op with `WithInterpC` enabled.

Acceptance criteria met:
* Functional parity via `TestDecodePropsMatchesReference` and `TestEvalParityC_*`
* TCK valid scenarios pass with C path default on
* `CGO_ENABLED=0` builds remain pure Go

### 6. Rollout
If criteria met, make C path default behind build tag, keep Go fallback.

## Subsequent steps

* Vector kernels → C
* Expression eval core → C
* Streaming aggregation → C

Each step repeats baseline → implement → parity → measure → gate.

## Files to create

* `scripts/perf-baseline.sh`
* `scripts/perf-compare.sh`
* `perf/baseline-20261116-fastjson.json`
* `c/fastjson.c` , `c/fastjson.h`
* `cypher/interp/fastjson_c.go`

Next action: create harness scripts and record full baseline suite.
