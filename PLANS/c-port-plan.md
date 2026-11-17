# Plan: Port graingraph from Go to C

Branch: `port/c-rewrite`
Date: 2026-11-16

## Context

graingraph is ~206 Go files, ~56k LOC, embedded property graph database with Cypher 25 parser/analyzer/interpreter, SQLite storage, HTTP server, bulk import/export, schema, vector search, temporal types.

Moving to C is a full rewrite, not a mechanical translation. Goal here is a feasible migration path that preserves behavior and test coverage.

## What must change

### 1. Language & runtime
* Go → C99/C11
* Go concurrency, goroutines, channels → pthreads / manual thread pools
* Go error handling → error codes / out-parameters
* Go generics, interfaces, slices, maps → manual structs, vtables, hash tables
* Memory management: Go GC → explicit malloc/free, arena allocators for query execution

### 2. Dependencies
* `modernc.org/sqlite` pure-Go → C SQLite3 via `sqlite3.h`. CGO-free requirement disappears, but we now depend on system SQLite.
* `golang.org/x/text/unicode/norm`, `time/tzdata` → ICU / C equivalents for Unicode normalization and timezone data
* No more Go module system → CMake/Meson/Makefile + pkg-config

### 3. Build & CI
* Go toolchain → C toolchain (clang/gcc), CMake, sanitizers
* Replace `go test` with cmocka / Unity + custom TCK runner
* Replace `go vet` / `golangci-lint` with clang-tidy, cppcheck

### 4. Codebase areas
* `store/` : SQLite schema, DDL, prepared statements. Relatively portable.
* `types.go`, `helpers.go`, `result.go` : Core value types, records. Need C equivalents.
* `cypher/syntax/` : Hand-written lexer/parser on stdlib only. Can be ported to C with minimal deps.
* `cypher/analyze/` : Semantic analysis. Pure data structures, portable.
* `cypher/interp/` : Interpreter, ~largest part. Heavy use of Go slices/maps, interfaces. Needs rewrite.
* `cypher/temporal/`, `spatial/`, `vector/` : Domain logic, portable with care.
* `importer.go` : CSV/JSON bulk import. JSON parser needed → jansson or custom fastjson.
* `cmd/graingraph-query/` + `internal/server/` : HTTP server. Go stdlib net/http → libuv / mongoose / custom HTTP in C, or keep as separate Go shim.
* Tests: 56k LOC Go tests → C tests + preserve TCK harness.

## Order of migration

### Phase 0 – Inventory & scaffolding [Week 1]
1. Create branch `port/c-rewrite` – done.
2. Inventory files, dependencies, public API surface.
3. Define C project layout:
```
src/
  core/
  store/
  cypher/
    syntax/
    analyze/
    interp/
  server/
include/graingraph/
tests/
CMakeLists.txt
```
4. Define public C API matching Go public API:
`gg_open`, `gg_run_query`, `gg_begin_tx`, `gg_result_next`, etc.

### Phase 1 – Storage layer [Weeks 2-3]
Port `store/` first. It has no Cypher types.
* Create SQLite schema DDL.
* `gg_store_open/close`, `insert_node`, `insert_edge`, `insert_labels`
* Parameterised queries, WAL mode, `SetMaxOpenConns` → single writer + read pool with `sqlite3_open_v2(SQLITE_OPEN_READONLY)`
* Tests: replicate `store` unit tests via C tests.

### Phase 2 – Core types & result cursor [Week 4]
* `Node`, `Relationship`, `Record`, `ResultSummary`
* JSON property storage/decoding → embed fast JSON decoder (port `fastjson.go` to C)
* Result cursor API with callbacks

### Phase 3 – Cypher front-end [Weeks 5-9]
* Lexer + Parser: `cypher/syntax`
* AST node structs, allocator-based
* Analyzer: `cypher/analyze` – scopes, types, TCK error codes
* Keep Go tests as reference; write C unit tests for parser on TCK samples

### Phase 4 – Interpreter core [Weeks 10-20]
Hardest part.
* Expression evaluator
* Pattern matching, node/relationship scans
* `pushdown.go`, `relfirst.go`, `batch.go`, `stream.go`, `aggpush.go`
* Shortest paths, quantified patterns, vector search
* Incremental port with equivalence tests against Go implementation

### Phase 5 – Import/Export, Schema, Temporal, Vector [Weeks 21-23]
* Bulk import/export JSON/CSV
* Schema commands
* Temporal types: port `cypher/temporal` with `time.h` + tzdata
* Vector search: port matrix builder

### Phase 6 – Server [Weeks 24-26]
* `graingraph-query` HTTP API. Options:
  A) Full C HTTP server → high cost
  B) Keep thin Go shim that embeds C library via cgo – pragmatic
* Recommend B for initial port to preserve compatibility

### Phase 7 – Test parity & TCK [Ongoing]
* Port `compat/tck_test.go` to C runner that invokes Go TCK scenarios and compares outputs
* Property tests, benchmarks

## Risks & decisions

* **Interpreter size**: ~30k LOC of complex Go logic. Full port may exceed ROI. Consider keeping interpreter in Go and only porting storage/API to C.
* **Memory safety**: Need ASan/MSan/UBSan in CI, fuzzers for parser.
* **Performance**: Go GC was hiding allocations. C version must be careful with arenas for query execution.
* **Team cost**: Estimated 12-18 months for feature parity by 2-3 engineers.

## Milestones

M1: C store passes Go store tests
M2: Parser passes TCK valid scenarios
M3: Analyzer reports correct TCK error codes
M4: `MATCH ... RETURN` queries match Go outputs
M5: Full TCK 3,895 scenarios pass
M6: Server API parity

## Immediate next steps

1. Freeze Go public API as header `include/graingraph/graingraph.h`
2. Stand up CMake skeleton + SQLite test
3. Port `store` and write first passing C test

---
This plan is a high-level roadmap. Detailed design required before coding.
