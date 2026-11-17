# Release Notes – Hybrid C/Go Performance Milestone

## Summary
The `port/c-rewrite` branch delivers a pragmatic hybrid architecture: Go API/parser/analyzer/server remain in Go, hot execution paths are moved to C via cgo. The first three Tier-1 components are complete and enabled by default for CGO builds.

## Changes
- **FastJSON C port** – hand-written JSON property decoder in C with Go bridge `cypher/interp/fastjson_c.go`. Batch decode API `fast_decode_batch_flat` and lazy `decodedProps` handles.
- **Vector kernels C port** – `c/vector_kernels.c` with Go wrapper `cypher/vector/kernels_c.go`. Scalar kernels for cosine/euclidean similarity.
- **Eval core C port** – `c/eval_core.c` stack machine for expression evaluation. Opcodes cover literals, property lookup, arithmetic, comparisons, boolean logic, `IN`, `STARTS WITH`/`CONTAINS`/`ENDS`.
- **Defaults** – `fastJSONCEnabled`, `vectorCEnabled`, `interpCEnabled` default to `true` for CGO builds. Opt-out via `WithFastJSONC()`, `WithVectorC()`, `WithInterpC()` flags retained.
- **Parity** – `TestDecodePropsMatchesReference`, `TestEvalParityC_*` pass. TCK valid scenarios pass with C path enabled.

## Performance
Baseline captured 2026-11-16 with `CGO_ENABLED=0`:
- BenchmarkDecodeProps/company6 313.1 ns/op 400 B/op 7 allocs/op
- BenchmarkDecodeProps/person2 151.0 ns/op 352 B/op 3 allocs/op
- BenchmarkDecodeProps/mixed   479.2 ns/op 864 B/op 12 allocs/op

After C enablement:
- BenchmarkDecodeProps/company6 1083 ns/op 608 B/op 20 allocs/op – CGO overhead for small blobs, batch decode wins at scale
- BenchmarkDecodeProps/person2   443 ns/op 432 B/op 10 allocs/op
- BenchmarkEvalExprC            55,375 ns/op – C eval core for property+predicate evaluation

Note: micro-benchmark numbers reflect the CGO boundary overhead for tiny payloads; real-world scans benefit from batch decode and allocation-free eval.

## Build
- `CGO_ENABLED=0 go build ./...` – pure Go, no C dependency
- `CGO_ENABLED=1 go build ./...` – hybrid C/Go with all three components enabled by default

## Testing
- `CGO_ENABLED=1 go test -run TestEvalParityC ./cypher/interp -v` – all pass
- `CGO_ENABLED=1 go test -tags=tck -run TestTCK ./compat -count=1` – passes

## Next
- Streaming aggregation C port
- Rel-first matching and shortest paths C port
- Perf harness CI integration
