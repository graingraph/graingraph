# graingraph

**An embedded property graph database for Go — Cypher over SQLite.**

graingraph was created as a response to poor cell phone signal on U.K trains that prevented download of Neo4j docker container & associated Go driver; I was in midst of learning Go ( still am ) and wanted to try out using Neo4j drivers.  I felt, perhaps more in hope, that a smaller download may have made it. 

graingraph is a Go package that provides an embedded property graph accessible using openCypher or Cypher 25.  It uses SQLite for storage, either in memory or file based. It sets out to be a small, lightweight, and allows moving to full Neo4j with as least friction as possible. 

Like this

```go
import "github.com/graingraph/graingraph"

// In-memory — transient, great for tests
db, err := graingraph.Open(":memory:")
result, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
for result.Next(ctx) {
    fmt.Println(result.Record().Values()[0])
}
```

> graingraph does not implement the Bolt wire protocol. To reach it over the
> network, run [`graingraph-query`](#run-it-as-a-server-graingraph-query), an
> HTTP server compatible with Neo4j's Query API (v2) built on it, which also has
> a compact output format and a skill for AI agents.



---

## Cypher Compatibility

graingraph passes **every scenario of the openCypher Technology Compatibility Kit** that applies to Cypher 25 (3,895 scenarios; the few that rely on syntax Cypher 25 removed are listed with reasons in `compat/testdata/excluded.txt`). Queries are parsed by a hand-written Cypher 25 parser, checked by a semantic analysis pass that reports the standard error classes and codes, and run by a Go interpreter over SQLite.

| Feature | Supported |
|---|:---:|
| `MATCH` / `OPTIONAL MATCH` — labels, properties, directed and undirected relationships, variable-length paths, named paths | ✅ |
| `shortestPath()` / `allShortestPaths()`, pattern predicates, `EXISTS`/`COUNT`/`COLLECT` subqueries | ✅ |
| `WHERE`, `RETURN`, `WITH`, `ORDER BY`, `SKIP`/`LIMIT`, `DISTINCT`, `UNWIND`, `UNION [ALL]`, `RETURN *` | ✅ |
| Aggregation — `count`, `sum`, `avg`, `min`, `max`, `collect`, `percentile*`, `stDev*` | ✅ |
| `CASE`, list/pattern comprehensions, quantifiers (`all`/`any`/`none`/`single`), `reduce` | ✅ |
| Scalar, string, math, list, conversion and entity functions | ✅ |
| Temporal types — `date`, `time`, `localtime`, `datetime`, `localdatetime`, `duration` (constructors, accessors, truncation, arithmetic, `duration.between`) | ✅ |
| `CREATE`, `MERGE` (`ON CREATE`/`ON MATCH`), `SET`, `REMOVE`, `DELETE`/`DETACH DELETE`, `FOREACH` | ✅ |
| `CALL` procedures — built-in `db.labels()`, `db.relationshipTypes()`, `db.propertyKeys()` and your own via `DB.RegisterProcedure` | ✅ |
| `CALL { … }` subqueries, `FILTER`, `LET`, `FINISH` | ✅ |
| Named query parameters (`$param`) | ✅ |
| Bulk import — JSON, CSV (Neo4j format); bulk export — JSON | ✅ |
| Neo4j extensions — label/type expressions, dynamic labels and properties, map projections, `EXISTS`/`COUNT`/`COLLECT` and `CALL {}` subqueries, `FILTER`/`LET`/`FINISH`, `OFFSET`, `IS :: TYPE` predicates, `EXPLAIN`/`PROFILE` | ✅ |
| Quantified path patterns (`((a)-[:R]->(b)){1,3}`, `-[:R]->+`) and path selectors (`ANY`, `ALL`, `SHORTEST k`, `SHORTEST k GROUPS`) | ✅ |
| `POINT` (cartesian, WGS-84; `point.distance`, `point.withinBBox`) and `VECTOR` (`vector()`, `vector_distance`, `vector_norm`, `vector.similarity.*`) values; vector indexes with exact nearest-neighbour search (`db.index.vector.queryNodes`, `DB.VectorSearch`) | ✅ |
| Schema — `CREATE`/`DROP INDEX` and `CONSTRAINT` (unique, node/relationship key, existence, type), `SHOW INDEXES`/`CONSTRAINTS`/`PROCEDURES` | ✅ |
| `LOAD CSV` (opt-in: `WithImportDirectory`) | ✅ |
| Nested quantified path patterns, `SHOW FUNCTIONS`, `USE` and user/role/database management commands | ❌ (parsed; execution returns `ErrUnsupportedCypher`) |

Unsupported features return `ErrUnsupportedCypher` — they never silently produce wrong results.

**Shortest paths and limits.** `shortestPath()`, `allShortestPaths()`, `ANY SHORTEST` and `ALL SHORTEST` over a single relationship pattern use a breadth-first search (bidirectional when both ends are bound; `allShortestPaths` returns at most 100,000 paths). `SHORTEST k`, `SHORTEST k GROUPS`, `ANY k` and `ALL`, and selectors over longer patterns, enumerate matches per (start, end) pair: with both ends bound they stop as soon as the selector is satisfied, otherwise they fail with an error beyond 1,000,000 matches. `WithMaxPathHops(n)` caps unbounded variable-length patterns and rejects larger explicit bounds. Nested quantified path patterns are not supported.

---

## Install

```bash
go get github.com/graingraph/graingraph
```

Requires Go 1.26.9+ (earlier 1.26 releases have a `net/http` vulnerability, GO-2026-6617, that affects `graingraph-query`). No CGO required. Works on Linux (amd64/arm64), macOS (arm64), and Windows (amd64).

---

## Quick Start

### Opening a database

```go
import "github.com/graingraph/graingraph"

// In-memory — transient, great for tests
db, err := graingraph.Open(":memory:")

// File-backed — persists across restarts
db, err := graingraph.Open("/var/data/graph.db")

// With options
db, err := graingraph.Open("graph.db",
    graingraph.WithBusyTimeout(5*time.Second),
    graingraph.WithReadOnly(),
)

defer db.Close(ctx)
```

In tests, use `NewTestDB` to open an in-memory database that is closed automatically when the test ends:

```go
db := graingraph.NewTestDB(t)
```

### Concurrency

A `*DB` is safe for concurrent use. On a file-backed database, statements without updating clauses run on a small pool of read-only SQLite connections (`WithMaxReadConns`, default 2), each in its own snapshot transaction. Readers never wait for the writer and the writer never waits for readers: a read issued while another goroutine holds an open `BeginTx` transaction returns immediately and sees only committed data. Writes, schema commands and explicit transactions use the single write connection. In-memory databases have one connection and no read pool.

More read connections are not always faster, because the pure-Go SQLite shares allocator and file-lock state between connections. On a 20,000-node graph, two readers run indexed lookups 1.8x faster than one but gain nothing from more, and run a full-label scan 1.3x faster, while four run it slower than one. Raise `WithMaxReadConns` only after measuring (`go test -bench ParallelReads`).

### Indexes and constraints

Declare them in Cypher or from Go:

```cypher
CREATE INDEX FOR (p:Person) ON (p.name)
CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE
CREATE CONSTRAINT FOR (p:Person) REQUIRE (p.first, p.last) IS NODE KEY
CREATE CONSTRAINT FOR (p:Person) REQUIRE p.name IS NOT NULL
CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER
SHOW INDEXES      SHOW CONSTRAINTS      DROP CONSTRAINT <name>
```

```go
err := db.CreatePropertyIndex(ctx, "Person", "name")
err = db.CreateUniqueConstraint(ctx, "Person", "email")
schema, err := db.ListSchema(ctx) // []SchemaInfo; db.DropIndex / db.DropConstraint by name
```

Constraints are checked when a statement finishes, over the nodes and relationships it created or changed, and a violation rolls the whole statement back with a structured error:

```go
var cv *graingraph.ErrConstraintViolation
if errors.As(err, &cv) && cv.Kind == "UNIQUENESS" { /* cv.Label, cv.Properties, cv.Name */ }
```

A constraint cannot be created over data that already breaks it. Uniqueness works for multi-label nodes. Bulk `Import` is checked too: after the rows are inserted and before the import commits, every imported node and relationship is validated, and a violation fails the whole import with `*ErrConstraintViolation`. `Export` (JSON) writes the indexes and constraints in a `"schema"` section (Cypher `CREATE … IF NOT EXISTS` statements), and `Import` replays them after the data, in the same transaction, so a copy of a database keeps its schema. Indexes are SQLite expression indexes, so equality lookups on an indexed property stop scanning: on 100,000 nodes a lookup by property drops from 25 ms to 33 µs (and a relationship lookup, `MATCH ()-[r:R {w: $v}]->()`, from 28 ms to 40 µs, with an index created by `CREATE INDEX FOR ()-[r:R]-() ON (r.w)`). graingraph also creates a node index automatically once a property has narrowed a few scans on a graph of 500 or more nodes; `WithoutAutomaticIndexes` turns that off. The schema is stored in the database file, so it survives reopening and `Snapshot`.

### Vector search

Store embeddings as `VECTOR` values (or lists of numbers), declare a vector index, and search it:

```cypher
CREATE VECTOR INDEX doc_embeddings FOR (d:Doc) ON (d.embedding)
OPTIONS {indexConfig: {`vector.dimensions`: 384, `vector.similarity_function`: 'cosine'}}

CALL db.index.vector.queryNodes('doc_embeddings', 10, $query) YIELD node, score
RETURN node.title, score
```

```go
matches, err := db.VectorSearch(ctx, "doc_embeddings", queryVector, 10) // []VectorMatch{Node, Score}
```

The index validates the dimension on every write (a wrong-sized vector fails the statement) and supports `cosine` (score `(1 + cos)/2`) and `euclidean` (score `1/(1 + d²)`). The search is exact, not approximate: it scans an in-memory copy of the index's vectors (float32, contiguous, split across CPUs). Measured on an Apple M5 Max at 384 dimensions: 10,000 vectors in 0.2 ms, 100,000 in 0.8 ms, 1,000,000 in 7 ms (about 1.5 GB of memory). The copy is built on the first search after the database is opened (about 4 µs per vector: 0.4 s for 100,000, 4 s for a million, on all your cores) and then kept current as you write. Writes inside an explicit transaction are visible to searches in that transaction and to others only after it commits.

Limits: node indexes only (no relationship vector indexes), a write to the same file by another process is not seen until the database is reopened, and the in-memory copy is capped at 512 MiB across all indexes (`WithMaxVectorCacheBytes`); beyond that a search still works but rebuilds its copy each time. There is no approximate (HNSW) index, which only starts to matter beyond a few million vectors.

### Auto-commit queries

`RunQuery` executes a Cypher statement in auto-commit mode and returns a lazy `*Result` cursor.

```go
result, err := db.RunQuery(ctx,
    `MATCH (p:Person {name: $name})-[:KNOWS*1..3]->(f:Person) RETURN f.name AS name`,
    map[string]any{"name": "Alice"},
)
if err != nil {
    log.Fatal(err)
}
for result.Next(ctx) {
    fmt.Println(result.Record().Values()[0])
}
if _, err := result.Consume(ctx); err != nil {
    log.Fatal(err)
}
```

### Explicit transactions

`BeginTx` starts an explicit transaction. Call `Commit` or `Rollback` when done; `Close` is idempotent and always safe to defer.

```go
tx, err := db.BeginTx(ctx)
if err != nil {
    log.Fatal(err)
}
defer tx.Close()

_, err = tx.Run(ctx, `CREATE (n:Person {name: $name})`, map[string]any{"name": "Bob"})
if err != nil {
    _ = tx.Rollback()
    log.Fatal(err)
}
if err := tx.Commit(); err != nil {
    log.Fatal(err)
}
```

### Collecting results

`Collect` drains all records from a `*Result` into a slice:

```go
result, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n`, nil)
records, err := result.Collect(ctx)
for _, rec := range records {
    node, _ := rec.Get("n")
    fmt.Println(node.(*graingraph.Node).Props)
}
```

`Single` returns the one record in a result, or an error if there are zero or more than one:

```go
result, err := db.RunQuery(ctx, `MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
rec, err := result.Single(ctx)
// err is graingraph.ErrNoRecords or graingraph.ErrMultipleRecords when applicable
```

### Generic helpers

Use the generic helpers for typed property and record access:

```go
// Extract a typed property from a Node or Relationship
age, err := graingraph.GetProperty[int64](node, "age")

// Extract a typed value from a Record column
name, isNil, err := graingraph.GetRecordValue[string](rec, "name")

// Collect all records as a typed slice using a mapper
result, _ := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
names, err := graingraph.CollectT(ctx, result, func(rec *graingraph.Record) (string, error) {
    return graingraph.GetRecordValue[string](rec, "name")
})

// Single with a mapper
result, _ := db.RunQuery(ctx, `MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
node, err := graingraph.SingleT(ctx, result, func(rec *graingraph.Record) (*graingraph.Node, error) {
    n, _, err := graingraph.GetRecordValue[*graingraph.Node](rec, "n")
    return n, err
})
```

### Bulk import and export

```go
// Import from JSON
f, _ := os.Open("testdata/graph.json")
if err := db.Import(ctx, f, graingraph.FormatJSON); err != nil {
    log.Fatal(err)
}

// Export to JSON
var buf bytes.Buffer
if err := db.Export(ctx, &buf, graingraph.FormatJSON); err != nil {
    log.Fatal(err)
}
```

### Snapshots

`Snapshot` writes an atomic, consistent copy of the database to a file using SQLite `VACUUM INTO`. Works on both file-backed and in-memory databases.

```go
db, _ := graingraph.Open(":memory:")
// ... build or import graph data ...

if err := db.Snapshot("/var/data/graph-checkpoint.db"); err != nil {
    log.Fatal(err)
}

// Reopen the snapshot as a normal database.
snap, _ := graingraph.Open("/var/data/graph-checkpoint.db")
```

---

## Examples

Runnable programs live in [`examples/`](examples/); run them from the repository root.

| Example | What it shows |
|---|---|
| [`examples/basic`](examples/basic/main.go) | Create nodes and relationships, query them, update, run a transaction, and delete — `go run ./examples/basic` |
| [`examples/movies`](examples/movies/main.go) | Build the movies graph (171 nodes, 253 relationships) by reading a file of Cypher statements, then query it — `go run ./examples/movies` |

---

## API Reference

### Entry point

| Symbol | Signature | Description |
|---|---|---|
| `Open` | `Open(path string, opts ...Option) (*DB, error)` | Open or create a database. Use `":memory:"` for a transient in-memory store. |
| `NewTestDB` | `NewTestDB(t testing.TB) *DB` | Open an in-memory database; registers `t.Cleanup` to close it. |

### DB methods

| Method | Signature | Description |
|---|---|---|
| `RunQuery` | `(*DB) RunQuery(ctx, cypher string, params map[string]any) (*Result, error)` | Execute a Cypher statement in auto-commit mode. |
| `BeginTx` | `(*DB) BeginTx(ctx) (*Tx, error)` | Start an explicit transaction. |
| `Import` | `(*DB) Import(ctx, r io.Reader, format Format) error` | Bulk-import nodes and relationships from JSON or CSV. |
| `Export` | `(*DB) Export(ctx, w io.Writer, format Format) error` | Bulk-export the graph to JSON. |
| `Snapshot` | `(*DB) Snapshot(path string) error` | Write an atomic consistent copy of the database to a file. |
| `Close` | `(*DB) Close(ctx) error` | Release all resources. |

### Tx methods

| Method | Signature | Description |
|---|---|---|
| `Run` | `(*Tx) Run(ctx, cypher string, params map[string]any) (*Result, error)` | Execute a Cypher statement inside the transaction. |
| `Commit` | `(*Tx) Commit() error` | Commit the transaction. |
| `Rollback` | `(*Tx) Rollback() error` | Roll back the transaction. |
| `Close` | `(*Tx) Close() error` | Close the transaction (rolls back if not yet committed). |

### Result methods

| Method | Signature | Description |
|---|---|---|
| `Next` | `(*Result) Next(ctx) bool` | Advance the cursor; returns false when exhausted or on error. |
| `Record` | `(*Result) Record() *Record` | Return the current record (valid after `Next` returns true). |
| `Err` | `(*Result) Err() error` | Return any iteration error. |
| `Keys` | `(*Result) Keys() []string` | Return the projection column names. |
| `Collect` | `(*Result) Collect(ctx) ([]*Record, error)` | Drain all records into a slice. |
| `Single` | `(*Result) Single(ctx) (*Record, error)` | Return the single record, or `ErrNoRecords` / `ErrMultipleRecords`. |
| `Consume` | `(*Result) Consume(ctx) (*ResultSummary, error)` | Drain remaining records and return query counters. |

`ResultSummary` carries two views of a statement's side effects. `Counters()` compares the graph before and after
(the openCypher TCK's side effects): a node created and deleted in one statement leaves no trace. `Statistics()` counts
each change as it happens, the way a Neo4j server reports it: that same node counts as created and as deleted, setting
a property to the value it already has counts, and removing a property counts as a property set. Use `Statistics()` to
give clients of a Neo4j-compatible service the numbers Neo4j would.

### Generic helpers

| Function | Description |
|---|---|
| `GetProperty[T PropertyValue](entity, key)` | Extract a typed property from a `*Node` or `*Relationship`. |
| `GetRecordValue[T RecordValue](rec, key)` | Extract a typed value from a `*Record` column; second return is true when null. |
| `CollectT[T any](ctx, result, mapper)` | Collect all records, applying a mapper to each; returns `[]T`. |
| `SingleT[T any](ctx, result, mapper)` | Like `Single`, but applies a mapper to the one record; returns `T`. |

### Sentinel errors

| Error | Meaning |
|---|---|
| `ErrNoRecords` | `Single` found zero records. |
| `ErrMultipleRecords` | `Single` found more than one record. |
| `ErrReadOnly` | A write statement was attempted on a read-only database. |
| `ErrUnsupportedCypher` | The Cypher feature is not yet implemented. |

---

## Architecture

```
graingraph/
├── types.go          ← Node, Relationship, Record, error types
├── driver.go         ← graingraph.Open, DB, RunQuery, BeginTx
├── tx.go             ← Tx type (Run, Commit, Rollback, Close)
├── result.go         ← Result cursor (Next, Record, Err, Keys, Collect, Single, Consume)
├── helpers.go        ← generic helpers (GetProperty, GetRecordValue, CollectT, SingleT)
├── importer.go       ← Import / Export (JSON, CSV)
├── options.go        ← functional options (WithBusyTimeout, WithReadOnly)
├── engine.go         ← parse → analyze → interpret pipeline
├── cypher/
│   ├── syntax/       ← hand-written Cypher 25 lexer + parser + typed AST
│   ├── analyze/      ← semantic analysis (variables, aggregation, types)
│   ├── interp/       ← Go interpreter: executes a statement against SQLite
│   └── proc/         ← procedure signatures and registry (CALL)
├── store/
│   ├── store.go      ← Store interface
│   ├── sqlite.go     ← modernc.org/sqlite implementation
│   └── schema.go     ← DDL: nodes/edges tables + indexes
├── compat/
│   └── tck_test.go   ← openCypher TCK harness (opt-in: -tags=tck)
└── bench/
    └── *.go          ← benchmark suite
```

Storage uses two tables in SQLite WAL mode. Queries are executed by a Go interpreter that reads nodes and relationships from SQLite (narrowing scans with property equalities and automatic expression indexes) and applies writes in one transaction per statement.

```sql
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',   -- comma-separated: "Person,Employee"
    props  JSON    NOT NULL DEFAULT '{}'
);

CREATE TABLE edges (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    type     TEXT    NOT NULL,
    start_id INTEGER NOT NULL REFERENCES nodes(id),
    end_id   INTEGER NOT NULL REFERENCES nodes(id),
    props    JSON    NOT NULL DEFAULT '{}'
);
```

---

## Run it as a server (graingraph-query)

`graingraph-query` is an HTTP server on top of graingraph that is compatible
with Neo4j's Query API (v2): most clients that send Query API requests to Neo4j
work with it, and the differences are listed below. Download a binary from the
[Releases](https://github.com/graingraph/graingraph/releases) page, or
`go install github.com/graingraph/graingraph/cmd/graingraph-query@latest`.

It is built for one person or agent at a time. There is a single basic-auth
credential, and whoever holds it can do anything to the graph.

### Run

```sh
export GRAINGRAPH_QUERY_PASSWORD=change-me
go run ./cmd/graingraph-query --username admin --db graingraph.db

curl -u admin:change-me -H 'Content-Type: application/json' \
  localhost:7474/db/neo4j/query/v2 \
  -d '{"statement":"RETURN 1 AS n"}'
```

### Endpoints

| Method | Path | |
|---|---|---|
| GET | `/` | discovery (no auth) |
| GET | `/health` | liveness (no auth) |
| GET | `/skills` | list agent skills (no auth) |
| GET | `/skills/{name}` | one skill as Markdown (no auth) |
| POST | `/db/{db}/query/v2` | run a statement |
| POST | `/db/{db}/query/v2/tx` | open a transaction |
| POST | `/db/{db}/query/v2/tx/{id}` | run in a transaction |
| POST | `/db/{db}/query/v2/tx/{id}/commit` | commit |
| DELETE | `/db/{db}/query/v2/tx/{id}` | roll back |

Responses follow what a Neo4j server returns, checked side by side against
Neo4j 2026.07:

- Success is `202`. A failed statement is `400` with
  `{"errors":[{"code":"Neo.ClientError.Statement.SyntaxError","message":"…"}]}`.
  A failure *while* producing a result (arithmetic, type errors) is `202` with
  the error beside an empty `data` block.
- A failed statement inside a transaction destroys the transaction (later calls
  get `404`). Rollback answers `200 {}`.
- Unknown databases answer `404`; authentication failures `401`.
- Plain JSON, Typed JSON (`application/vnd.neo4j.query.v1.x`) and JSONL are
  supported via `Accept`, including comma-separated lists with q-values.
- `Accept: text/toon` is an extension for agents, see below.
- CORS is enabled and `OPTIONS` answers preflights (see `--cors-origins`).

**One transaction at a time.** graingraph serialises on a single connection, so
while an explicit transaction is open it holds the whole database. Other
queries, and attempts to open a second transaction, wait up to `--tx-wait`
(default 5s, enough for tools that open several short transactions at once)
and then fail with `Neo.TransientError.General.DatabaseUnavailable` naming the
open transaction, instead of hanging. Commit or roll it back; idle transactions are rolled back
after `--tx-timeout`.

### TOON output for agents

`Accept: text/toon` answers with a [TOON](https://toonformat.dev) table instead
of the Neo4j response document. It is an extension: Neo4j has no equivalent,
so use JSON when you need parity with a real server.

```sh
curl -u admin:change-me -H 'Content-Type: application/json' -H 'Accept: text/toon' \
  localhost:7474/db/neo4j/query/v2 \
  -d '{"statement":"MATCH (p:Person) RETURN p.name AS name, p.age AS age"}'
# rows[2]{name,age}:
#   Alice,30
#   Bob,null
```

- One flat table, `rows[N]{columns}:`. A node or relationship becomes columns
  (`p._id`, `p._labels`, `p.name`, ... and `r._type`, `r._start`, `r._end`);
  a path column is a Cypher pattern, `(:Person {name:Keanu})-[:ACTED_IN]->(:Movie {title:The Matrix})`;
  other lists and maps are a compact JSON string in one cell.
- No bookmarks or timings. Non-zero mutation counters are always reported
  (`counters:`), without `includeCounters`.
- Every failure is an error status with an `errors[N]{code,message}:` table,
  including one raised while producing rows, which JSON answers with `202`.
- Transactions work as usual: opening returns a `transaction:` block with the
  id, and commit answers `committed: true`.

#### The same query, JSON and TOON

A statement that returns two nodes and the relationship between them, on the
Neo4j movies graph:

```cypher
MATCH (p:Person {name: $name})-[r:ACTED_IN]->(m:Movie {title: $title})
RETURN p, r, m
```

with parameters `{"name": "Tom Hanks", "title": "Cast Away"}`.

**JSON** (the default, the same document Neo4j returns; reformatted here, the
server sends it on one line):

```json
{
  "data": {
    "fields": ["p", "r", "m"],
    "values": [
      [
        {"elementId": "57", "labels": ["Person"], "properties": {"born": 1956, "name": "Tom Hanks"}},
        {"elementId": "97", "startNodeElementId": "57", "endNodeElementId": "165", "type": "ACTED_IN", "properties": {"roles": ["Chuck Noland"]}},
        {"elementId": "165", "labels": ["Movie"], "properties": {"released": 2000, "tagline": "At the edge of the world, his journey begins.", "title": "Cast Away"}}
      ]
    ]
  },
  "bookmarks": ["FB:AAAAAAAAAA8vsraGnMfqiQ=="],
  "queryType": "r",
  "resultAvailableAfter": 1,
  "resultConsumedAfter": 1
}
```

**TOON** (`Accept: text/toon`):

```
rows[1]{p._id,p._labels,p.born,p.name,r._id,r._type,r._start,r._end,r.roles,m._id,m._labels,m.released,m.tagline,m.title}:
  "57",Person,1956,Tom Hanks,"97",ACTED_IN,"57","165","[\"Chuck Noland\"]","165",Movie,2000,"At the edge of the world, his journey begins.",Cast Away
```

Each node and relationship is spread over columns: `p._id`, `p._labels`,
`p.born`, `r._type`, `r._start`, `r._end` and so on. A property a row lacks is
`null`, the `roles` list is a JSON string in its cell, and `"57"` is quoted
because it is a string that looks like a number (an element ID), not a number.
This one row is 147 tokens as JSON and 96 as TOON (GPT tokenizer); the header is
written once, so the saving grows with the number of rows. The element IDs
belong to this server and differ from Neo4j's.

#### Token cost and accuracy

Measured on a synthetic graph of 80 people and 12 companies. These are small
experiments, not guarantees; your data will differ.

**Tokens** (GPT `o200k` tokenizer, so approximate for other models). Percentages
are against plain JSON as the server returns it:

| Result shape | TOON | Typed JSON |
|---|---|---|
| flat table, 25-50 rows | 81-90% | about 320% |
| wide table, 12 columns | 84% | 350% |
| 25 nodes | 51% | 246% |
| 25 `(p)-[r]->(c)` rows | 47% | 225% |
| `collect()` list cells | 93% | 300% |
| mixed benchmark, all of the above | **58%** | 254% |

Most of the saving comes from flattening nodes and relationships and dropping
bookmarks and timings, not from the TOON syntax itself. For a tiny result such
as a count, the fixed response metadata is most of the JSON.

On the Neo4j movies graph (49 queries, from counts to nodes, relationships
and paths) served TOON was 60% of the tokens of plain JSON; paths were 51%.

**Accuracy.** Haiku and Sonnet, with no tools, were given the same query
result as JSON or TOON and asked about 10 questions each (lookups, filtered
counts, sums, maximums, nulls, list membership) over 3 seeded datasets of 80
rows, with awkward values: numeric-looking strings such as `"007"`, text with
commas, colons, quotes and newlines, empty strings against nulls. Answers were
graded against the true values (246 questions per format):

| | JSON | TOON |
|---|---|---|
| Haiku | 92.7% | 95.1% |
| Sonnet | 96.7% | 97.6% |

The difference is within noise, so read it as parity, not as TOON being more
accurate. Lookups, joins, null handling and the tricky strings were correct in
both formats apart from one wrong zip in TOON. Nearly every error was
arithmetic over many rows (sums and counts), which models get wrong in either
format; an agent with a shell should compute those instead of reading them.
Limits: two models, one run per question, 80 rows, and the prompts included a
short description of each format (the skill carries the same description for
TOON).

### Agent skills

An agent can learn how to use the server (and the Cypher it supports) by
fetching a skill: a `SKILL.md` document in the
[Agent Skills](https://agentskills.io) format. The skill is embedded in the
binary, so it always matches the version you run, and its examples point at
your server.

```sh
# over HTTP (no authentication; also linked as "skills" in discovery)
curl localhost:7474/skills                   # [{name, description, url}]
curl localhost:7474/skills/graingraph-query      # Markdown

# from the command line, no server needed
graingraph-query skill --url https://graph.example.com > SKILL.md
graingraph-query skill --list
```

For Claude Code, save it as `~/.claude/skills/graingraph-query/SKILL.md`. Behind a
proxy, set `--public-url` so the HTTP version advertises the right address.
The skill holds no credentials; it tells the agent to ask the user for them.

### Configuration

Every flag has an environment variable (`--tx-timeout` is `GRAINGRAPH_QUERY_TX_TIMEOUT`).

| Flag | Default | |
|---|---|---|
| `--listen` | `:7474` | listen address |
| `--db` | `graingraph.db` | database file, or `:memory:` |
| `--username`, `--password` | required | basic-auth credential (prefer the env var for the password) |
| `--database-name` | `neo4j` | name advertised by discovery |
| `--read-only` | false | open the database read-only |
| `--tx-timeout` | `5m` | idle timeout for explicit transactions |
| `--tx-wait` | `5s` | how long a request waits for an open transaction to end before failing with "transaction X is open" (`0` fails at once) |
| `--import-dir` | | enables `LOAD CSV` below this directory |
| `--page-cache` | 0 | SQLite page cache in bytes (raises `PRAGMA cache_size`; about 2 MiB per connection when `0`) |
| `--mmap-size` | 0 | memory-mapped I/O of the database file, in bytes; a negative value disables it, `0` uses SQLite's default |
| `--synchronous` | | `PRAGMA synchronous`: `full`, `normal` or `off`; blank leaves journal_mode's default (`off` is lossy across a crash) |
| `--page-size` | 0 | page size in bytes for a new database file; a power of two between 512 and 32768, ignored on an existing file |
| `--tls-cert`, `--tls-key` | | enable HTTPS |
| `--cors-origins` | `*` | origins allowed by CORS; `none` disables |
| `--public-url` | from request | base URL advertised by discovery |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | `text` | `text` or `json` |
| `--log-file` | stderr | append logs to a file |
| `--log-statements` | true | include Cypher text in request logs |

### Logging

One line per request: `request_id` (also returned as `X-Request-Id`), method,
path, status, duration, response `bytes`, remote address, user agent, and the
`user` name presented. Statement requests add `statement`, `query_type`,
`rows`, and `tx_id`. A failed statement adds `error_code` and `error`, and is
logged at WARN (ERROR for database errors) even though it answers `202`.
Passwords and query parameters are never logged.

### Testing the server

```sh
go test ./...
```

#### Compatibility with a real Neo4j

`internal/server/compat` sends identical requests to a Neo4j server and to an
in-process graingraph-query and requires the responses to match (status, content
type and body, ignoring element IDs, bookmarks, timings, transaction IDs and
error wording). It covers every response format, parameters, errors, and
transactions over a small graph. It is skipped unless a server is configured:

```sh
GRAINGRAPH_QUERY_COMPAT_NEO4J_URL=http://localhost:7474 \
GRAINGRAPH_QUERY_COMPAT_NEO4J_PASSWORD=secret \
  go test ./internal/server/compat -v
```

`GRAINGRAPH_QUERY_COMPAT_NEO4J_USER` (default `neo4j`) and
`GRAINGRAPH_QUERY_COMPAT_NEO4J_DB` (default `neo4j`) are optional. The reference
database is never modified: writes happen in explicit transactions that are
always rolled back, on private `Cq*` labels, and no schema is created (Neo4j
does still register the property key names the writes used).

Known differences, each marked in the test with its reason:

- graingraph-query also supports Typed JSON over JSONL (`application/vnd.neo4j.query+jsonl`);
  Neo4j 2026.07 answers 406.
- A non-string `statement` is `Request.Invalid` here, `Statement.SyntaxError` in Neo4j.
- Neo4j adds `notifications` (such as deprecation warnings); graingraph-query has none.
- A request body needs a JSON `Content-Type` (`application/json` or a Typed JSON type);
  a missing one is `415`. A body without a `Content-Type` is the one shape a web page
  can send to another origin without a CORS preflight, so refusing it keeps a page
  from running statements with credentials the browser has cached. For the same
  reason a `POST` or `DELETE` whose `Origin` is neither the server's own host nor in
  `--cors-origins` is `403`.

Also seen when comparing 54 queries on the Neo4j movies graph (not covered by
the compatibility test; the rest matched, including every temporal and float
format):

- `avg` and `stDev` can differ in the last digits (graingraph-query computes
  `avg` more exactly; `stDev` is rounded to 15 significant digits).
- `keys(n)` lists the properties sorted; Neo4j lists them in insertion order.
- graingraph does not support `datetime({epochSeconds: ...})` (Neo4j does).
- Functions that return strings, such as `toString(localtime('12:30'))`, use
  graingraph's shortest form (`12:30`); only values returned as temporal types are
  written the way Neo4j writes them (`12:30:00`).

#### Performance against a real Neo4j

`internal/server/perf` is a measurement harness, not a pass/fail test: it prints
latency tables (p50, p95, mean, speed-up) and requests per second for the
same requests against Neo4j and graingraph-query, and fails only if a request
errors. It covers request overhead and result serialisation for each response
format and size, then queries and writes over a small graph. It is skipped
unless a reference server is configured:

```sh
GRAINGRAPH_QUERY_PERF_NEO4J_URL=http://localhost:7474 \
GRAINGRAPH_QUERY_PERF_NEO4J_PASSWORD=secret \
  go test ./internal/server/perf -v -timeout 30m
```

`GRAINGRAPH_QUERY_PERF_NEO4J_USER` and `GRAINGRAPH_QUERY_PERF_NEO4J_DB` default to
`neo4j`. As with the compatibility test the reference database is never
modified: data-dependent cases build their graph on private `Cq*` labels in a
transaction that is always rolled back.

By default graingraph-query runs inside the test process and shares its CPU and
garbage collector. For a fair comparison start it separately and point the
harness at it (same credentials) with `GRAINGRAPH_QUERY_PERF_GRAINGRAPH_URL`.

---

## Testing

```bash
# Unit and integration tests
CGO_ENABLED=0 go test -count=1 ./...

# TCK harness (openCypher Technology Compatibility Kit)
CGO_ENABLED=0 go test -tags=tck ./compat/... -v

# Property-based tests
CGO_ENABLED=0 go test -run TestRapid ./...

# Benchmarks
CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=10s ./bench/...
```

---

## API Stability

**No breaking changes are made to the public API after v1.0 without a major version bump.**

This covers the root package and all documented sub-packages. Adding new exported symbols is not a breaking change. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full definition.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, test suite commands, the 5-step guide for adding a Cypher feature, benchmark baseline process, and PR guidelines.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).

Neo4j is a registered trademark of Neo4j, Inc. graingraph is an independent
project and is not affiliated with, endorsed by or sponsored by Neo4j, Inc.
References to Neo4j describe compatibility.
