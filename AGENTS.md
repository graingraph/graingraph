# AGENTS.md — graingraph

## Project Overview

graingraph is an embedded property graph database for Go, backed by SQLite and queryable via a subset of openCypher. The primary entry point is `graingraph.Open`; queries are executed with `db.RunQuery` or via explicit transactions started with `db.BeginTx`.

- Module path: `github.com/graingraph/graingraph`
- Go minimum version: 1.26.9 (the `go` line in go.mod also selects the toolchain CI and releases build with; 1.26.9 fixes GO-2026-6617 in `net/http`)
- SQLite driver: `modernc.org/sqlite` (CGO-free, no mattn/go-sqlite3)

## Feedback Instructions

### Build
```bash
CGO_ENABLED=0 go build ./...
```

### Test (unit)
```bash
CGO_ENABLED=0 go test -tags=unit -count=1 ./...
```

### Test (all, excluding tck)
```bash
CGO_ENABLED=0 go test -count=1 ./...
```

### Vet
```bash
go vet ./...
```

## Package Layout

```
graingraph/
├── types.go        ← Node, Relationship, Record, error types
├── driver.go       ← graingraph.Open, DB, RunQuery, BeginTx
├── interfaces.go   ← exported interfaces (Driver, Session, Transaction, Result, …)
├── session.go      ← session, managedTx, Tx concrete types
├── result.go       ← QueryResult / Result cursor implementation
├── importer.go     ← Import / Export helpers
├── migrate.go      ← neo4j migration helpers (to be removed in v2)
├── neo4jadapter/   ← neo4j DriverCompat (to be removed in v2)
├── engine.go       ← parse → analyze → interpret pipeline (statement cache, procedures)
├── cypher/
│   ├── syntax/     ← hand-written Cypher 25 lexer/parser + typed AST (stdlib only)
│   ├── analyze/    ← semantic analysis: scopes, aggregation and type errors with TCK codes
│   ├── interp/     ← Go interpreter: the default execution engine (imports syntax + database/sql only)
│   └── proc/       ← procedure signatures, argument coercion and registry (stdlib only)
├── store/          ← Store interface + SQLite implementation + DDL
├── compat/         ← TCK harness (opt-in: -tags=tck)
├── cmd/graingraph-query/  ← the Neo4j Query API server binary (see "Server" below)
├── internal/server/       ← the server's packages: api, auth, config, store, skills, compat, perf
└── testdata/       ← .cypher fixture files
```

## Key Architectural Constraints

- The `store/` package must NEVER import Cypher types — it works with raw IDs, labels, JSON blobs only.
- `cypher/syntax` imports only the standard library (nothing from this module). `cypher/analyze` imports only `cypher/syntax` and `cypher/proc`. `cypher/interp` imports `cypher/syntax`, `cypher/proc`, `cypher/temporal`, `cypher/spatial`, `cypher/vector`, `golang.org/x/text/unicode/norm` and `database/sql` (never `store/`).
- All SQL must use parameterised queries — never `fmt.Sprintf` user input into SQL strings.
- CGO must remain disabled: always use `modernc.org/sqlite`, never `mattn/go-sqlite3`.

## Storage Schema

```sql
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',
    props  JSON    NOT NULL DEFAULT '{}'
);
CREATE TABLE edges (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    type     TEXT    NOT NULL,
    start_id INTEGER NOT NULL REFERENCES nodes(id),
    end_id   INTEGER NOT NULL REFERENCES nodes(id),
    props    JSON    NOT NULL DEFAULT '{}'
);
CREATE TABLE node_labels (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    label   TEXT    NOT NULL,
    UNIQUE (node_id, label)
);
-- idx_node_labels_label ON node_labels(label, node_id) — O(log n) label lookups
```

WAL mode is enabled via `PRAGMA journal_mode=WAL` on every open.
`node_labels` is written by the code that writes `nodes.labels` (`SQLiteStore.InsertNode`, `graph.createNode`/`persistLabels` in the interpreter) — NOT by triggers (they cost 2-3x the node insert itself and are dropped on open); any new writer of `nodes.labels` must also write `node_labels` (`TestNodeLabelsStayInSync`). Label lookups use EXISTS subquery or JOIN against node_labels rather than LIKE on nodes.labels.

## Gotchas and Learnings

- `store.Open` calls `db.SetMaxOpenConns(1)` — CRITICAL. Without it, `:memory:` SQLite gives each pool connection a separate database.
- `Consume` and `Collect` on `QueryResult` must guard against `r.rows == nil` — write results have `rows: nil` and `consumed: true`.
- `NewRecord` panics on key/value length mismatch (programmer error).
- Labels are stored as comma-separated text in the `labels` column.
- `json_extract(props, '$.key')` is used for property access in SQLite queries.
- `go.mod` `go` directive is 1.26.5 (raised from 1.24 so govulncheck is clean: `golang.org/x/text` >= 0.39 and the net/url fix need Go 1.25+); `modernc.org/sqlite v1.35.0` builds fine at it.
- `testdata/` package is excluded from `./...` by Go design. Run explicitly: `CGO_ENABLED=0 go test github.com/graingraph/graingraph/testdata`.
- Only one file per package should have a `// Package foo ...` doc comment.
- When deleting files that export methods used in `example_test.go`, also remove the corresponding `Example*` functions — otherwise `go build ./...` fails even if core tests pass.
- neo4j driver fully removed in task-010 via `go mod tidy` + `go mod vendor` (both needed — the repo uses a vendor dir; `go build` fails with "inconsistent vendoring" if only tidy is run).
- `Tx` type lives in `tx.go` (moved from session.go in task-003); context params on Commit/Rollback/Close were removed in task-005 — all were blank identifiers so no behavior changed.
- `DB.Close` still takes `context.Context`; only `Tx` methods are context-free.
- `*ErrImportDepthExceeded` must never be wrapped with `fmt.Errorf("%w")` — the existing test uses a direct type assertion (not `errors.As`). Use a `wrapErr` helper that checks `errors.As(err, &depthErr)` and returns the unwrapped sentinel directly.
- `interfaces.go` is deleted in v2; all session-layer/compat interfaces (Driver, Session, Transaction, ResultTransformer, etc.) are gone.
- When replacing `NewEagerResult(ctx, qr)` calls, use `qr.Collect(ctx)` to get records directly — no intermediate struct needed.
- `QueryResult` is renamed to `Result` (task-004); `NewQueryResultFromRows` → `NewResultFromRows`; `newInMemoryQueryResult` → `newInMemoryResult`. `NewResultFromRows` is still exported until task-007 unexports it.
- When a rename touches test files that use `NewQueryResultFromRows` via dot-import, update those call sites mechanically in the same task to keep the unit test suite green.
- `ErrNoRecords` and `ErrMultipleRecords` are `fmt.Errorf` sentinels (consistent with `ErrReadOnly`); `errors.Is` works via pointer equality.
- `(*Result).Single()` uses `Consume()` to close the cursor in all paths. In the `ErrMultipleRecords` path, drain/close errors from `Consume()` are intentionally discarded (secondary to the primary sentinel); documented with a comment.
- Task-009 adds test coverage for `Single`, `ErrNoRecords`, and `ErrMultipleRecords` — but task-007 already added it in `result_test.go`.
- All formerly-exported internal helpers are now unexported (task-007): `newResultFromRows`, `newRecord`, `setCounters`, `queryCounters`. `MapColumnValue` and `SplitLabels` wrappers are deleted entirely.
- `driver.go` increments `queryCounters` fields directly (e.g. `ctr.nodesCreated++`); no exported `QueryCounters` struct exists.
- `result_test.go` and `types_test.go` use `graingraph.Open` + `db.RunQuery` to construct test fixtures — no raw `*sql.Rows` or `newResultFromRows` in tests.
- `testdata/integration_test.go` and `compat/tck_test.go` still reference `NewEagerResult` (removed in task-003) — they are out-of-scope for `go build ./...` and will be fixed in task-009.
- `helpers.go` adds `PropertyValue`, `RecordValue`, `GetProperty[T]`, `GetRecordValue[T]`, `CollectT[T]`, `SingleT[T]`. The unexported `propsGetter` interface is implemented by `*Node` and `*Relationship` via `getProps()` methods added to `types.go`-adjacent declarations in `helpers.go`.
- `convertTo[T]` uses `any(zero).(type)` type-switch (not reflection) to coerce JSON-decoded values; SQLite returns JSON numbers as `float64`, so `toInt64` converts `float64→int64` via truncation.
- `graingraph.EagerResult` and `graingraph.NewEagerResult` are deleted in v2. Test packages that need eager collection define a local `eagerResult` struct + `collectResult` helper using `qr.Collect(ctx)` followed by `qr.Consume(ctx)` (idempotent) to get counters.
- `types.go` had a second `// Package graingraph ...` doc block (v1-era text referencing Neo4j Aura); it was removed in task-011. Only `driver.go` carries the package doc comment.
- `testdata/integration_test.go` and `compat/tck_test.go` both define their own `eagerResult`/`collectResult` — they are separate packages and cannot share a common helper without a new exported type.
- `DB.Close` still takes `context.Context` (only `Tx` methods are context-free); any test calling `db.Close()` without args must be fixed to `db.Close(context.Background())`.
- `analyze.Check` reports only what it can prove (unknown types are accepted) and must have ZERO false positives: `cypher/analyze` `TestTCK_ValidScenariosPass` checks every valid TCK query and `TestTCK_CompileTimeErrors` requires the exact class and code for all 586 compile-time-error cases. Rules worth knowing: a path variable is bound AFTER its elements (so `p = (p)-->()` is "already bound" while a later node `r` after path `r` is a type conflict); in an aggregating item every variable/property leaf outside an aggregate must equal a projected non-aggregating item (so `me.age + you.age + count(*)` is ambiguous even if `me.age + you.age` is projected); `NoExpressionAlias` is reported after the other projection checks; `WITH *` with no variables is legal but `RETURN *` is `NoVariablesInScope`.
- The TCK harness (`compat`) requires class AND code to match for "should be raised at compile time"; runtime and any-time expectations still accept any error. Property access on a path is a `SyntaxError`, on a non-map value a `TypeError` (both `InvalidArgumentType`).
- `cypher/syntax` operator precedence is the one the TCK pins down (Precedence1-4), loosest to tightest: OR, XOR, AND, NOT, comparison (`= <> < >` …), predicates (`IN`, `STARTS WITH`, `CONTAINS`, `=~`, `IS NULL`, `:: TYPE`), `+ - ||`, `* / %`, `^` (left-associative), unary `-`/`+`, then postfix. So `[1]+2 IN [3]+4` is `([1]+2) IN ([3]+4)` and `false = true IN l` is `false = (true IN l)`. Operator and clause words (`NOT`, `AND`, `IN`, `WHEN`, …) cannot start an expression; other keywords are valid variable, label and function names.
- SQLite FOREIGN KEY constraint errors are detected via `strings.Contains(err.Error(), "FOREIGN KEY constraint failed")` — modernc.org/sqlite surfaces the constraint name verbatim in the error string. Catch this in `InsertEdge` callers and return a domain-appropriate error rather than exposing the raw SQLite message.
- CSV node `:ID` values are file-local labels only — the actual SQLite primary keys are AUTOINCREMENT-assigned by `InsertNode`. In a fresh empty DB, sequential inserts give IDs 1, 2, 3, … matching the CSV row order, which benchmarks rely on.
- `Result.rawVals`, `ptrs`, and `vals` are pre-allocated in `newResultFromRows` and reused across all `Next` calls. `ptrs[i] = &rawVals[i]` is stable because `rawVals` is never appended to. `newRecord` copies both keys and values internally, so reusing `vals` is safe.
- `importJSON` uses `io.ReadAll(io.LimitReader(r, importMaxBytes+1))` for size detection. Do NOT replace this with a streaming decoder approach: `json.Decoder` scans bytes one at a time in a whitespace loop, causing `TestImport_TooLarge` (which sends 500MB of spaces via `io.Pipe`) to hang for 30+ seconds.
- `decodeImportJSON` must handle `null` values for `"nodes"` and `"edges"` keys. When Go marshals a struct with nil slice fields, JSON produces `"nodes":null`; the decoder must treat this as an empty array (check `tok == nil` after `dec.Token()`).
- `go test -race ./cypher/...` takes about a second now that parsing no longer goes through ANTLR; the whole-module race run is dominated by the SQLite tests in the root package.
- `node_labels(node_id, label)` junction table is maintained in Go by the node writers (see Storage Schema); raw SQL that writes `nodes.labels` directly would leave it stale.
- Bulk node import is ~36k rows/s with the old `node_labels` triggers and ~101k rows/s with labels written by Go (`BenchmarkImportCSVNodes`; relationships ~137k rows/s, `BenchmarkImportCSVEdges`). Measured and NOT worth it: `PRAGMA temp_store=MEMORY` (no gain once the triggers were gone), a bigger `cache_size`, per-transaction prepared INSERT statements.
- `node_labels` has `UNIQUE(node_id, label)` so that `INSERT OR IGNORE` in `backfillMigrationSQL` truly prevents duplicate rows. Without a unique constraint, `INSERT OR IGNORE` is a no-op and does NOT deduplicate.
- The backfill migration uses `WHERE NOT EXISTS (... WHERE node_id = n.id)` to skip nodes already populated by triggers (i.e., inserted after the schema upgrade). `INSERT OR IGNORE` handles the edge case where a node partially appears in node_labels.
- The interpreter applies writes to SQLite immediately inside one transaction (rolled back on error), defers deletes to the end of the statement, and reports net counters. A null property is never stored (Cypher semantics). `collect()` returns a `[]any`, and a `WHERE` on `OPTIONAL MATCH` is part of the match (it never removes input rows).
- Missing `$parameters` are detected up front from `syntax.Statement.Params` and returned as an unwrapped `*ErrMissingParameter`.
- Procedures: `DB.RegisterProcedure(*proc.Procedure)`; built-ins are `db.labels`, `db.relationshipTypes`, `db.propertyKeys`. `analyze.CheckWith` validates calls (existence, argument count/type/passing mode) against the registry. The TCK harness registers its "there exists a procedure" declarations as lookup tables (`compat/tckproc_test.go`).
- Interpreter performance (`cypher/interp`): node scans are narrowed in SQLite by property equalities from inline property maps and top-level `WHERE var.key = <literal|param|bound var>` conjuncts (`pushdown.go`); they are only a prefilter and Go re-checks every match. Hint values must be string/int64/float64 (SQLite would equate a boolean with 1/0) and keys must be plain `[A-Za-z0-9_]` identifiers because the JSON path is inlined in the SQL (needed for expression indexes to apply). `interp.Engine` (one per `DB`) creates `idx_auto_np_<hex key>` expression indexes on `json_extract(props, '$."key"')` after a key has narrowed 3 scans on a graph with >=500 nodes (max 32, never dropped), and caches parsed+analysed statements (cleared when procedures change). `decodeProps` uses a hand-written JSON decoder (`fastjson.go`) with an encoding/json fallback; `fastjson_test.go` checks they agree.
- Query execution is `syntax.Parse` → `analyze.CheckWith` → `interp.RunWith` (`engine.go`); there is no SQL translator, planner or plan cache any more (removed after the interpreter reached parity and beat it on lookups/traversals). SQLite is the only store; the interpreter talks to it directly through `database/sql` and does not use `store.Store` for queries. Do not add a second storage backend.
- `cypher/syntax` parses Cypher 25 including Neo4j's extensions (label expressions, `EXISTS/COUNT/COLLECT {}`, `CALL (a) {} IN TRANSACTIONS`, quantified path patterns and path selectors, map projections, dynamic labels/properties, `IS :: TYPE`, `FILTER`/`LET`/`FINISH`, schema commands, `LOAD CSV`, hints, server-only commands). The interpreter executes all of these except nested quantified path patterns, `SHOW FUNCTIONS`, `USE` and the other server-only commands (user/role/database management), which return an "unsupported" error naming the construct. `CYPHER 5` is rejected; `NEXT`, `INSERT` and standalone `ORDER BY` are deliberately not parsed (not documented in the Cypher Manual). Syntax was checked against the Neo4j Cypher Manual, not guessed.
- Compile-time errors are `*syntax.SyntaxError` or `*analyze.Error`; `analyze.Describe(err)` returns the openCypher TCK class and detail code (`UndefinedVariable`, `VariableAlreadyBound`, `AmbiguousAggregationExpression`, `InvalidArgumentType`, …) through graingraph's `%w` wrapping. Lexer/parser errors carry a `Code` too (`IntegerOverflow`, `InvalidNumberLiteral`, …; default `UnexpectedSyntax`).
- TCK: `CGO_ENABLED=0 go test -count=1 -tags=tck ./compat/... -v`. Nothing is skipped and the harness requires 100% of the scenarios not listed in `compat/testdata/excluded.txt` (3,895 pass). `WithMaxPathHops` is an opt-in cap (default: none).
- Benchmarks: `go test -run XXX -bench BenchmarkQueries .` (2,000-node graph).
- Temporal values live in `cypher/temporal` and are returned to callers as `graingraph.Date`, `LocalTime`, `Time`, `LocalDateTime`, `DateTime` and `Duration` (aliases; `String()` is the canonical Cypher rendering, which the TCK compares). They are stored in the props JSON as `{"$t":<kind>,"v":"<canonical string>"}` (maps cannot be property values, so a nested object is unambiguous) and revived in `decodeProps`; they cannot be used as scan-pushdown hints. "Now" is fixed per statement (`exec.clock`) except `.realtime()`. Named zones need `time/tzdata`, which is embedded (about 450 KB of binary). A duration keeps months, days and seconds separately; fractional units cascade (month fraction -> days at 30.436875, day fraction -> seconds) using exact rational arithmetic in `duration.go`.
- Void procedures (no outputs) pass rows through an in-query CALL; procedures with outputs multiply rows by their results.
- The TCK harness checks every side-effect counter exactly (`no side effects` means all zero; unlisted names in `the side effects should be` must be zero). Label counters follow the TCK: `+labels`/`-labels` count labels that came into or went out of use in the database (compared before and after the statement), not label assignments; a node created and deleted in one statement leaves no counters; deleting an existing entity counts its properties (and labels) as removed.
- Neo4j-extension scenarios live in `compat/testdata/neo4j/*.feature` (run by `TestNeo4jExtensions`, which requires 100% except entries in `compat/testdata/neo4j-deferred.txt`). Add a scenario there with every extension construct you implement.
- Quantified path patterns (`group.go`): a group's variables are lists (one entry per iteration; `analyze` types them as lists but checks the group's own WHERE and property maps against per-iteration singletons), juxtaposed node patterns are one and the same node, relationships are never reused (so cycles end), and an unbounded quantifier is capped by `WithMaxPathHops` when set. Quantified relationships (`-[:R]->+`) are var-length relationships. Selectors other than ANY/ALL SHORTEST over a single relationship pattern enumerate all matches and select per (start, end) partition (`matchSelected`).
- Schema (`schema.go`, `show.go`): definitions are rows in `charta_schema` (created on first use); RANGE/TEXT indexes and the lookup index of a uniqueness/key constraint are SQLite expression indexes `gl_idx_<id>`; other index kinds are recorded only. Constraints (unique, key, existence, type; nodes and relationships) are checked in `graph.finish()` over the entities the statement created or changed, so a violation rolls the statement back. Bulk `Import` writes through the store and does NOT check constraints.
- `LOAD CSV` is disabled unless the DB is opened with `WithImportDirectory(dir)`; it then reads `file:///x.csv` (or `x.csv`) below `dir` only (path and symlink escapes are rejected, remote URLs are not supported). Empty fields are null; `linenumber()` and `file()` work directly after it.
- `EXPLAIN` compiles and analyses the query but does not run it and returns no rows; `PROFILE` just runs it.
- Concurrent reads: `store.Open` keeps a second `*sql.DB` (the read pool, `PRAGMA query_only` set through the DSN so every pooled connection gets it) for file databases when `Config.ReadConns > 0`; `Store.BeginReadTx` hands out a snapshot transaction on it (and is just `BeginExecTx` for `:memory:`). `runInterp` sends a statement for which `hasWrites` is false through `interp.RunReadOnly` on a read tx wrapped in `readOnlyDB` (whose `ExecContext` errors). Anything with `CREATE`/`MERGE`/`SET`/`REMOVE`/`DELETE`/`FOREACH`, a writing `CALL {}`, a schema command, or an explicit `BeginTx` stays on the write connection (`Tx` must read its own writes). Keep `hasWrites` conservative: a statement wrongly classified as read-only fails at run time with a read-only error. On the read path the automatic index advisor only queues keys (`Engine.TakeWantedIndexes`); `DB.buildIndexes` creates them on the write connection in a goroutine that `DB.Close` waits for.
- Default read pool size is 2 (`defaultReadConns`): the pure-Go SQLite serialises on allocator and file-lock mutexes, so more concurrent heavy scans get slower than serial (measured: 2 readers 1.3x faster, 4 readers 0.6x as fast on a 20k-node scan). `go test -bench ParallelReads` reproduces this; do not raise the default without re-measuring.
- Vector indexes (`schema.go`): `CREATE VECTOR INDEX ... OPTIONS {indexConfig: {`vector.dimensions`: n, `vector.similarity_function`: 'cosine'|'euclidean'}}` validates the options (dimension 1..4096 required, unknown keys rejected, HNSW tuning keys accepted and ignored) and stores them in the definition (`VectorDims`, `VectorSim`). `graph.finish()` rejects a created/changed node or relationship whose indexed property is neither null, a VECTOR of that dimension nor a list of that many finite numbers (`checkVectorIndexes`). `SHOW INDEXES YIELD options` reports the configuration.
- Vector search (`cypher/vector`, `cypher/interp/vecindex.go`): `db.index.vector.queryNodes(index, k, query)` and `DB.VectorSearch` scan an in-memory `vector.Matrix` (contiguous float32 rows, normalised for cosine, 8-accumulator kernels, worker-pool scan with per-worker bounded heaps, ties by node id; 100k x 384 in about 1.3 ms) instead of decoding properties: decoding a 384-float JSON property costs ~10 us, 100x the arithmetic. The matrix is built lazily on first use (`buildMatrixFast`: fetches each node's raw props text, a pool of parser goroutines extracts only the vector property with `vector.StoredVector` and a fast number parser straight into `vector.Builder` rows; about 4 us per 384-dim vector, limited by the SQLite fetch; the generic decoder and SQLite's json_extract both cost 11-30 us) and kept current from `VectorDelta`s that `graph.finish()` computes (created/changed/deleted nodes, label or property removal) and that the caller applies with `Engine.ApplyVectorDeltas` ONLY after the transaction commits (auto-commit path in `runInterp`, and `Tx.Commit`; a rollback discards them). Only statements with no updating clause and no enclosing explicit transaction use the shared matrix (`Options.VectorCache`); any other statement builds a private matrix from what it can see (read-your-writes, slower). A build that races a commit or a schema change is used for that query but not cached (`vecCache.epoch`). `WithMaxVectorCacheBytes` (default 512 MiB total) bounds the shared matrices. Limits: nodes only (relationship vector indexes are unsupported), and a write by another process to the same file is not seen until reopen.
- Relationship-first matching (`relfirst.go`): for a single-hop pattern `(a)-[r {k: v}]-(b)` whose relationship has pushable equality properties (inline map or the MATCH's WHERE) and whose ends are unbound with no property/WHERE constraints, `matchRelFirst` looks the relationships up by property (`graph.scanRels`, which uses a relationship property index when there is one) and then checks the ends, instead of scanning all nodes and walking their relationships. `TestRelFirstAgreesWithNodeFirst` compares it with the node-first plan (`relFirstDisabled`) on random graphs for every direction/type/label/path/WHERE shape; keep that test green when changing either strategy. `plan_internal_test.go` asserts with `EXPLAIN QUERY PLAN` that declared, constraint-backing, automatic and relationship indexes are really used. `WithoutAutomaticIndexes` turns the automatic node indexes off (`Engine.NoAutoIndexes`).
- Bulk import and the schema (`importer.go`, `cypher/interp/importcheck.go`): the importer inserts through the store, bypassing the interpreter, so after the inserts and before `Commit` it calls `DB.checkImport` -> `Engine.ValidateImported(ctx, tx.Exec(), nodeIDs, relIDs)`, which loads the imported rows in batches of 500 as if a statement had just created them and runs the constraint and vector-dimension checks; a violation rolls the import back and comes out as `*ErrConstraintViolation`. After a successful import `Engine.InvalidateVectors()` drops the vector matrices. JSON export adds a `"schema"` array (`Engine.SchemaStatements`: CREATE INDEX/CONSTRAINT ... IF NOT EXISTS, skipping the built-in lookup indexes) and JSON import replays it in the import transaction (`applyImportSchema` accepts only CREATE INDEX / CREATE CONSTRAINT). CSV formats carry no schema.

- Shortest paths (`match.go`): `shortestPaths` runs a node BFS with predecessor lists (`shortestPathsBFS`, `pathsFromPreds`; `allShortestPaths` is capped at 100,000 paths), or a bidirectional search for a single path with bound ends (`shortestPathBidirectional`; `shortestBidirectionalDisabled` test hook). Start == end and minimum length > 1 use the old path-state search (`shortestPathsSlow`). `WithMaxPathHops` errors if the requested bound exceeds the cap. Each BFS level is prefetched in batches (`prefetchLevel` -> `graph.prefetchAdj`/`prefetchNodes`, `IN (SELECT value FROM json_each(?))`; 100k grid far-corner 2.6 s -> 0.68 s). `matchSelected` (other selectors) enumerates all matches, failing above `maxSelectedMatches` (1,000,000), stops early for `ANY k` when both ends are bound, and for SHORTEST k / SHORTEST k GROUPS / ALL SHORTEST / ANY SHORTEST with bound ends searches by increasing path length (`exec.relLimit`/`overBudget` prune `walk`, `walkVarLength`; `matchParts` clears the limit for nested patterns; `selectedEarlyStopDisabled` disables both). `shortest_internal_test.go` compares the strategies on random multigraphs; `BenchmarkShortestPath` uses a 100k-node grid (results in `.plans/progress-shortest-path-traversal.txt`).
- Aggregation pushdown (`cypher/interp/aggpush.go`): a query that starts `MATCH (a[:L])` or `MATCH (a[:L])-[e[:T]]->(b[:M]) [WHERE ...]` and is immediately followed by `RETURN`/`WITH` with `var.key` keys and count/min/max/sum/avg runs as one SQL GROUP BY (`exec.aggregatePushdown`, called from `runQuery`; the projection's DISTINCT/ORDER BY/SKIP/LIMIT/WHERE still run through `finishProjection`). It returns `handled=false` for every value kind or shape it cannot reproduce exactly (lists, temporals, mixed numeric kinds, mixed types, float sums, DISTINCT, OR, `<>`) and when the statement has already written. Single-node patterns that read properties are deliberately not pushed (`aggPushNodeProps`). `TestAggregatePushdownAgrees` compares with the interpreter (`aggPushDisabled`) and asserts which queries are pushed; keep it green when touching either side. Known limits and the suggested larger fix (streaming executor, lazy props) are in `.plans/future-streaming-executor.md`.
- Streaming executor (`cypher/interp/stream.go`): a query whose remaining clauses are all `MATCH`/`UNWIND`/`WITH`/`RETURN`/`FILTER`/`LET`/`FINISH` runs as a push pipeline (`runStream`): each clause is a stage that passes every row on at once, instead of materialising a `[]row` per clause. Anything else (an updating clause, `CALL`, `FOREACH`, `LOAD CSV`) runs clause by clause as before, because a write must see all its input before it writes. A `LIMIT` (`mapStage`) returns `errPipelineDone`, which unwinds the matcher feeding it; `WITH ... WHERE`, `SKIP` and `LIMIT` keep the order `finishProjectionRows` gives them. Aggregation keeps one `accumulator` set per group (`aggregateStage`; count/sum/avg/min/max/collect/percentiles/stdev mirror `exec.aggregate`) and, when every aggregate item is a bare aggregate call, no row at all; ORDER BY and DISTINCT buffer (DISTINCT only the first row of each value) and finish with the shared `finishProjectionRows`; an ORDER BY with a LIMIT keeps only the best SKIP+LIMIT rows in a heap (`topN`: ties ordered by arrival, so the rows are exactly the first of the stable sort; a key error is held back until the input ends; `topNDisabled`/`topNUsed`). Relationship-first matching (`matchRelFirst`) inside a pipeline reads the relationships a page at a time (`scanRelPage`, by edge id, 512 then 2048), batch-loads the end nodes of each page with `prefetchNodes` and trims the caches between pages, so a LIMIT stops after the first page (`TestRelFirstPagesAgreeWithNodeFirst`). The `MATCH` stage swaps `ex.pushed` back to the outer value around each downstream push, so a WHERE's scan-narrowing equalities never reach a pattern evaluated by a later stage. `streamDisabled`/`streamUsed` (like `aggPushDisabled`) let `TestStreamAgreesWithClauseAtATime` compare both executors row for row, in order, errors included; add a query there for any new clause shape.
- Scans in a pipeline (`eachStartChunk`, `graph.scanPage`): a label and/or property-equality scan is read a page at a time by keyset (`id > after ORDER BY id LIMIT n`, a single pass overall; the first page is `startChunk` rows, later ones `scanPageSize`), so a `LIMIT` reads one page, not the label. Outside a pipeline `scanNodes` still reads everything at once, which is what keeps a `MATCH ... CREATE` from seeing its own writes.
- Cache bounding: when the whole statement only reads (`readOnlyBody`, or a read-only connection), `graph.evictable` is set and `trimCaches` drops the node, relationship and adjacency caches between pages of the outermost scan once they pass `cacheLimit`. This is safe because node and relationship equality, ordering and grouping all go by id (`value.go`), never by pointer. In a statement that writes it is NOT safe: `nodeSnap`, `createdNodes` and `delNodes` are keyed by `*Node`, so the caches stay. Measured on 1M companies / 1.5M relationships: `LIMIT 10` 1.3 s / 1.5 GB -> 1 ms / 27 MB; aggregations over every node 1.2-1.6 GB -> about 95 MB; a two-hop DISTINCT aggregation 1.6 GB -> 140 MB, at about 5% more time on a 3.6 s query (hot nodes are decoded again after a trim).
- Batched creates (`cypher/interp/batch.go`): `createNode`/`createRel` do not INSERT one row at a time. They take the id SQLite would assign (next `sqlite_sequence` value for `nodes`/`edges`, re-read whenever the buffer was empty), buffer the row, and `flushWrites` writes the buffer as multi-row INSERTs with explicit ids (nodes, then `node_labels`, then edges; at most ~1000 bound variables per statement). `graph.db` is a `flushDB` that flushes before every query or exec, so nothing sees the database without the buffered rows; `graph.raw` is the unwrapped DB for the flush itself and the sequence read. Any new SQL must go through `g.db`, never `g.raw`. A failed statement drops the buffer with the rest of its effects (the caller rolls back). `batchDisabled`/`batchedRows` support `TestBatchedCreatesAgreeWithRowAtATime`, which compares ids, rows, counters, errors and the final graph with and without batching, with and without a constraint. `netCounters` counts a label's nodes only up to the statement's own assignments + 1 (`LIMIT`), so a bulk-load statement does not cost the size of the database.

- Security scan fixes (2026-10): crafted statements must fail with an error, never crash the process (a goroutine stack overflow or out-of-memory is fatal and cannot be recovered). Rules that came out of it: (1) `runInterp` runs the interpreter under `guardRun`, which turns a panic into an error and rolls the transaction back (`Tx.Run` closes the `Tx` after one) - never let a panic leave a `*sql.Tx` open. (2) `syntax.Parse` checks the finished tree with `checkDepth` (`maxASTDepth` 10,000, iterative) because operator, postfix and label chains are built by loops and are invisible to `maxDepth`; the analyzer and interpreter recurse once per level and rely on it. `parsePatternParts` is capped at `maxPatternParts` (matching recurses per part). (3) Never compare expressions with `syntax.Dump` strings in a loop: use `syntax.Hasher` + `syntax.Equal` (`checker.same` in analyze; `exec.hasher` keys `exec.subst`). (4) Runtime values can nest arbitrarily (reduce, WITH chains): `equalsD`/`compareD`/`orderCompareD`/`writeKeyD`/`normalizeD`/`deleteValueD` carry a depth and panic with `errValueTooDeep` past `maxValueDepth`, which `run` converts to an error; `run` also rejects a returned value nested deeper (`nestingExceeds`) so encoders can recurse. New recursive value walkers need the same. (5) Size budgets: `maxStringBytes`, `maxListLen` (concatenation, `replace`, `range()`, `collect()` in both `project.go` and `stream.go`; it is a `var` so tests lower it, and `errListTooLong` is the one error); streaming `UNWIND range()` is lazy (`lazyRange`). (6) `exec.tick()` checks the context every 1024 calls from `eval` and the var-length DFS; add it to any new unbounded in-memory loop. (7) Variable-length expansion is capped at `Engine.maxHops()` (`MaxPathHops` or `hardMaxHops` 100,000). (8) Definitions in `charta_schema` come from the database file and are untrusted: `schemaDef.validate` skips bad rows (DROP still sees them via `loadSchemaRows(true)`), and DROP uses `backingIndexName(id)`, never the stored text. Schema `OPTIONS` may not contain subqueries (`checkSchemaOptions`); `rejectUpdates` covers WHEN/THEN branches. (9) Labels may not contain `,` (nodes.labels is comma-joined). (10) Caches are bounded: statement cache by bytes (`maxCachedStatementBytes`, statements over `maxCacheableBytes` are not cached), regex cache (`maxRegexCache`), advisor keys (`maxTrackedScanKeys`), scan hints (`maxScanHints`). (11) `tryPatternPart` is memoised per start token and pre-checks the matching `)` (`parser.match`); do not reintroduce speculative re-parsing that doubles per nesting level. (12) The server needs a JSON Content-Type (415 otherwise) and refuses `POST`/`DELETE` from a foreign `Origin` when CORS is restricted; typed List/Map parameters decode once (`convertNode`, `maxTypedDepth` 32); `Store.Begin` reserves the begin slot (`reserveBegin`) before `BeginTx`. (13) LOAD CSV errors must not contain the resolved path; the opened file must be a regular file; Windows device names are rejected.

## Server: graingraph-query

`graingraph-query` is an HTTP server that implements the Neo4j Query API (v2)
on top of the embedded database. Any client that talks Query API to Neo4j
should talk to this unchanged. It is single-user by design: one basic-auth
credential, full access. It lives in this module: the binary is
`cmd/graingraph-query` and everything else is under `internal/server/`. It
adds no dependencies beyond the standard library.

### Commands

```sh
go build ./...
go vet ./...
go test ./...       # CI runs this (with -race); also run by GoReleaser before a release
go run ./cmd/graingraph-query --username admin --db :memory:   # needs GRAINGRAPH_QUERY_PASSWORD
```

`internal/server/compat` and `internal/server/perf` are skipped unless a real Neo4j
is configured through `GRAINGRAPH_QUERY_COMPAT_NEO4J_*` / `GRAINGRAPH_QUERY_PERF_NEO4J_*`
(see README). A plain `go test ./...` therefore does not exercise them. If you
change a response shape, status code or error code, run the compat test when a
Neo4j is available, and say so if it wasn't run.

### Layout

```
cmd/graingraph-query/    flags, env, logging setup, server lifecycle, `skill` subcommand
internal/server/api/     HTTP handlers, content negotiation, encoding, errors, CORS, logging
internal/server/auth/    basic-auth middleware
internal/server/config/  Config, env helpers, Version
internal/server/store/   wraps graingraph: transactions, read-only, import dir, reaper
internal/server/skills/  embedded agent skills (<name>/SKILL.md) served by CLI and HTTP
internal/server/compat/  side-by-side tests against a real Neo4j
internal/server/perf/    latency harness against a real Neo4j (measurement, not pass/fail)
```

### Conventions and constraints

- **Neo4j parity is the contract.** Status codes, error codes
  (`Neo.ClientError.…`), body shapes and `Accept`/`Content-Type` handling copy
  what Neo4j returns. Known differences are listed in the README and marked in
  `internal/server/compat`; add to both rather than diverging silently.
- **Success is `202`.** A failure during execution (arithmetic, type errors) is
  also `202` with `errors` beside an empty `data`; parse/semantic failures are
  `400`. Use `writeRunFailure` / the helpers in `errors.go`, not ad hoc writes.
- **`Accept: text/toon` is the one deliberate departure from Neo4j** (`toon.go`):
  a flat table for agents, with every failure an error status. It never
  applies to the other formats. The encoder is hand-written and was checked
  against `@toon-format/toon`; if you change quoting or number rules, compare
  against that library again.
- **Responses are marshalled before anything is written** (`writeJSON`), so an
  encoding failure is a clean 500, not a truncated body. Keep it that way.
- **One transaction holds the database.** graingraph serialises on a single
  connection, so the store waits `--tx-wait` then fails with
  `DatabaseUnavailable`. Don't add code paths that take the connection without
  going through the store.
- **Never log parameters or passwords.** Statement text is logged only when
  `--log-statements` is on.
- **Don't trust request headers for URLs.** Use `Handler.baseURL`, which
  prefers `--public-url` over `Host`/`X-Forwarded-Proto`.
- **Auth:** `/db/...` is behind basic auth. Public by design: `/`, `/health`,
  `/skills`, `/skills/{name}`. Anything new that is public must not return
  secrets or database content.
- **Config:** each flag has a `GRAINGRAPH_QUERY_*` env var (via `config.EnvOr` and
  friends). Add both, validate in `Config.Validate`, and document in the README
  table. Passwords are preferred via env, not flags.
- `cmd/graingraph-query/main_test.go` blanks all `GRAINGRAPH_QUERY_*` variables so a
  developer's shell can't leak in; do the same in any new test that calls `run`.
- Tests are table-driven, use real servers via `httptest` (`newTestServer`,
  `do` in `internal/server/api`), and avoid mocks of graingraph.

### Agent skills

The skill lives in `internal/server/skills/graingraph-query/SKILL.md` (Agent Skills format:
YAML frontmatter with `name` and `description`, then Markdown). It is embedded
with `go:embed` and served by `graingraph-query skill` and `GET /skills[/{name}]`.

- `{{BASE_URL}}` in the text is replaced with the server's URL when rendered.
- The skill is documentation for *other* agents. When behaviour changes
  (endpoints, formats, error handling, supported Cypher, limits), update it in
  the same change, and check every claim against a running server rather than
  from memory. Do not put credentials or real hostnames in it.
- A new skill is a new `internal/server/skills/<name>/SKILL.md`; the name must equal
  its directory, with no dots or slashes.

### Releases

Tag-driven via GoReleaser (`.github/workflows/release.yml`, `.goreleaser.yaml`):
`graingraph-query` binaries for linux/darwin/windows on amd64/arm64
(`CGO_ENABLED=0`) plus a source archive. The tag is the version of the library
and of the server; `config.Version` (in `internal/server/config`) is what
discovery and `--version` report, so bump it with the release.

## Git

Don't commit or push unless asked. Keep commits focused, and run `go vet ./...`
and `go test ./...` first.
