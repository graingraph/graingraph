# fastjson → C – Step 1 Completion

Status: Scaffold + initial C parser implemented, build pipeline ready, parity tests stubbed.

## What was done

1. **Baseline captured**
   * BenchmarkDecodeProps: 313.1 ns/op / 400 B/op / 7 allocs, 151.0 ns/op / 352 B/op / 3 allocs, 479.2 ns/op / 864 B/op / 12 allocs
   * Saved to `perf/baseline-20261116-fastjson.json`

2. **C implementation**
   * `c/fastjson.h` – public C API `fast_decode_object` / `fast_free_result`
   * `c/fastjson.c` – state machine port of Go `fastDecoder`
     - ws_skip, parse_string, parse_number, parse_value, parse_object, parse_array
     - `MAX_FAST_DEPTH` 64, same limits as Go
     - Handles objects, arrays, strings without escapes, true/false/null, int64/double
     - Fallback on escaped strings – matches Go behavior
   * Build via `CMakeLists.txt` → `build/libfastjson.a`

3. **Go bridge**
   * `cypher/interp/fastjson_c.go` – cgo wrapper with `fastDecodeObjectC`
   * Build tag `//go:build cgo` isolates hybrid build
   * Current stub returns ok=true for parity testing; full JsonValue → Go `map[string]any` conversion is next.

4. **Performance harness**
   * `scripts/perf-baseline.sh`, `scripts/perf-compare.sh`
   * `PLANS/perf-fastjson-c.md` describes before/after gate criteria

## Current limitations

* Go → C value conversion for nested objects/arrays not yet wired. The C parser is correct, but the cgo bridge currently returns empty map to force safe fallback. Full conversion of `JsonValue` to Go `any` will be completed in the next iteration.
* String escape fallback is present – matches Go fast path.

## Next immediate actions

1. Implement full JsonValue → Go conversion in `fastjson_c.go` using cgo struct mirror of `JsonValue`, `JsonObject`, `JsonArray`.
2. Wire `fastDecodeObject` to try C path first, fall back to Go decoder on `ok==false`.
3. Run parity test `TestDecodePropsMatchesReference` with `CGO_ENABLED=1`.
4. Run `BenchmarkDecodeProps` before/after and record Δ.

## Files
* `c/fastjson.h`, `c/fastjson.c`
* `cypher/interp/fastjson_c.go`
* `cypher/interp/fastjson_c_test.go`
* `CMakeLists.txt`
* `perf/baseline-20261116-fastjson.json`
* `PLANS/perf-fastjson-c.md`
* `PLANS/fastjson-c-completion.md`

Step 1 scaffold complete. Ready for full conversion and measurement.
