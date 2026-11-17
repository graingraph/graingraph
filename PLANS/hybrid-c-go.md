# Pragmatic Hybrid: C for speed, Go for developer surface

Goal: keep graingraph's Go API, tooling and Cypher front-end, move the hot execution paths into C via cgo. This avoids a full rewrite while capturing most of the performance wins.

## Why hybrid wins

* Developer facing: Cypher parser/analyzer, API types, schema, import/export, server – all ergonomics remain in Go.
* Performance facing: interpreter execution loops, JSON property decode, vector search kernels, aggregation/streaming pipelines, path matching – move to C.
* Risk: cgo boundary is narrow and stable. Go code stays testable, TCK stays in Go.
* Can ship incrementally: one hot component at a time, with A/B tests.

## Architecture

Go host
  ├─ Public API: DB, RunQuery, BeginTx, Result, helpers
  ├─ Cypher front-end: syntax/, analyze/ – stays Go, stdlib only
  ├─ Store interface: store/ – Go, talks to SQLite via modernc.org/sqlite
  └─ cgo bridge → C engine

C engine
  ├─ interp_core.c – execution of already-analyzed AST
  ├─ fastjson.c – hand-written JSON property decoder
  ├─ vector/matrix.c – contiguous float32 matrix, 8-accumulator kernels, worker pool
  ├─ agg_stream.c – aggregation pushdown, streaming pipeline primitives
  └─ path_match.c – rel-first matching, BFS shortest paths

Go calls `c_engine_run(stmt_id, params, out_rows)` for a statement that has been parsed and analyzed. The C engine uses arena allocators per statement and returns rows back to Go as []byte slabs.

## Candidates for C port – ordered by impact / risk

### Tier 1 – Low risk, high throughput

1. **fastjson decoder** – `cypher/interp/fastjson.go`
   ~600 LOC hand-written. Pure string → map[string]any for property blobs.
   C version: same state machine, no allocations, uses small fixed buffers.
   Impact: node scans and property access. `BenchmarkDecodeProps`.

2. **Vector search kernels** – `cypher/vector/*.go`
   `matrix.go`, `kernels.go`. Contiguous float32, dot product, cosine/euclidean, worker pool.
   C version: SIMD friendly, reduces Go allocation churn.
   Impact: `BenchmarkVectorSearch`, `BenchmarkSearchCosine100k`.

3. **JSON property number parsing** – `decodeProps` path uses fastjson + fallback. Move entirely to C.

### Tier 2 – Interpreter hot loops

4. **Expression evaluation `eval.go`** – hot for every row. Move eval of arithmetic, comparison, list ops, temporal accessors to C. Keep AST in Go, pass flat opcode array to C.

5. **Streaming executor primitives** – `stream.go` stage loops: `mapStage`, `aggregateStage`, `topN`. These are tight loops over `[]row`. C version with arena buffers.

6. **Aggregation pushdown** – `aggpush.go`. The `exec.aggregatePushdown` path already builds SQL; the remaining Go aggregation can be C.

### Tier 3 – Matching and paths

7. **Rel-first matching** – `relfirst.go` `matchRelFirst`, `scanRelPage`. Tight SQLite fetch + node cache loops.

8. **Shortest paths** – `match.go` BFS with predecessor lists, prefetch. Memory heavy, benefits from C stacks.

9. **Batch writes** – `batch.go` multi-row INSERT buffering. Already Go, but C buffer management may reduce GC.

### Keep in Go

* `cypher/syntax/` – hand-written parser, stdlib only, great Go ergonomics
* `cypher/analyze/` – semantic checks, TCK error codes, pure logic
* `store/` – SQLite via pure Go driver, no CGO dependency needed
* `importer.go`, `options.go`, `helpers.go` – developer API
* `cmd/graingraph-query` + `internal/server/` – HTTP, auth, config – keep Go
* Tests, TCK harness, benchmarks – stay Go

## cgo boundary design

```go
// go/engine_c.go
type CStmtHandle uintptr

func cCompile(ctx context.Context, ast *syntax.Statement) (CStmtHandle, error)
func cRun(handle CStmtHandle, params map[string]any) (*Result, error)
func cFree(handle CStmtHandle)
```

* AST is serialized to a C-friendly bytecode: opcodes + constant pool + symbol table.
* Memory ownership: C arena alloc per statement, freed on `cFree`. Go never frees C memory directly.
* Error propagation: C returns error code + message buffer, Go wraps as `*analyze.Error` / `*syntax.SyntaxError`.

## Migration order

1. **fastjson → C** – DONE
   * `c/fastjson.c` + Go wrapper with `WithFastJSONC()`. Default on for CGO builds.
   * Parity tests pass, `BenchmarkDecodeProps` baseline captured.

2. **Vector kernels → C** – DONE
   * `c/vector_kernels.c` + Go wrapper with `WithVectorC()`. Default on for CGO builds.
   * Parity tests pass.

3. **Expression eval core → C** – DONE
   * `c/eval_core.c` stack machine for literals, property lookup, arithmetic, comparisons, boolean logic, `IN`, `STARTS WITH`/`CONTAINS`/`ENDS`.
   * `WithInterpC()` default on for CGO builds. Parity tests `TestEvalParityC_*` pass, TCK valid scenarios pass.

4. **Streaming aggregation → C** – next
   * `aggregateStage` and `topN` loops.

5. **Rel-first + path matching → C** – later

Each step ships behind a build tag and can be toggled off.

## Build & CI

* Keep `CGO_ENABLED=0 go build ./...` for pure Go builds.
* Introduce `CGO_ENABLED=1` build target `graingraph-cgo` for hybrid.
* CI runs both: pure Go baseline, hybrid performance.

## Estimated effort

* Tier 1: 3-4 weeks
* Tier 2: 8-10 weeks
* Tier 3: 12+ weeks

Total ~6 months for meaningful gains vs 12-18 months for full C port.

## Open questions

* Do we keep SQLite in Go? Yes – modernc.org/sqlite is proven, CGO-free.
* How to handle Go generics helpers `GetProperty[T]`? Keep in Go; they operate on already-decoded values.
* Thread safety: C engine must be re-entrant per statement, not global state.

---
Next step: prototype fastjson C port and measure `BenchmarkDecodeProps` delta.
