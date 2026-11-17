# Importing into graingraph

[graingraph](https://github.com/graingraph/graingraph) is an embedded property
graph (Cypher over SQLite) whose `graingraph-query` server speaks the Neo4j
Query API. This records whether the importer works against it, how it compares
with Neo4j, and what would make it faster.

Measured 12 Nov 2026 on one machine (Apple silicon, 18 cores): graingraph-query
v0.3.1 (release binary, file-backed database) against Neo4j 2026.10.2 community
in a container (6 GB heap, 4 GB page cache). Real Companies House data
(snapshot 2026-10-01): the first 50,000 and 500,000 rows of part 1. One run
each, fresh database each time unless stated, so indicative.

## Does the import work?

Yes, with a small change in this tool. `NEO4J_URI=http://127.0.0.1:7474` is all
it takes once the tool accepts the server:

- graingraph reports `neo4j_version: "42.0.0"` and has no `dbms.components()`,
  so the connect-time version probe failed. A server that names itself in the
  discovery document (`"server": "graingraph-query/0.3.1"`) is now accepted
  without the Neo4j 2026.10 gate, and its made-up version enables nothing
  Neo4j-only (no concurrent batches, no `DISJOINT BY`). The tool warns that
  behaviour may differ.
- It has no `db.awaitIndexes()`; the tool skips that call for such servers.
- It accepts everything else the importer sends: named constraints and
  indexes, `CALL {} IN TRANSACTIONS`, `date()`, `MERGE`, `LOAD CSV` (start it
  with `--import-dir`).

The live parity test (`internal/importer`, see the README) passes against it in
every variant: clean load, no-op refresh, refresh with moves, removals, SIC
changes and cleared addresses, and LOAD CSV. The graph fingerprints match
Neo4j's exactly at 50k and 500k.

## How it compares

Time in the import passes (`ch-import import --clean --workers 8 --batch 2000`):

| | 50k | 500k |
| --- | --- | --- |
| Neo4j, Bolt | 3 s | 14 s |
| Neo4j, Query API | 5 s | 18–22 s |
| graingraph v0.3.1 | 43 s | 7 m 31 s |
| graingraph v0.3.1 + the patch below | 16 s | 3 m 0 s |

graingraph is about 10× (50k) to 25× (500k) slower than Neo4j, and scales
linearly: 10× the data took 10.5× the time. A full 5.7M-company load would take
roughly 1.4 hours (3 minutes 20 seconds on Neo4j). That extrapolation was not
measured: I stopped a full run after it began.

Per pass at 500k (original → patched): Company nodes 3 m 36 s → 26 s, Address
nodes 1 m 29 s → 37 s, PreviousName 38 s → 16 s, REGISTERED_AT 22 s → 21 s,
HAS_SIC_CODE 41 s → 38 s, PREVIOUSLY_KNOWN_AS 43 s → 41 s.

## Where the time goes

The server process was profiled (pprof) under the importer's load, and the
interpreter in-process with a harness that runs the importer's exact
statements.

1. **`CREATE … SET` writes each property separately.** The interpreter runs one
   clause at a time: `CREATE` for all rows (flushing its insert buffer every
   2,000), then `SET` for all rows. By then the nodes are in the database, so
   every `SET c.x = …` is its own SQL `UPDATE` (`persistNodeProps`): a company
   with 18 properties costs 18. This was 66% of server CPU.
   - Same 5,000-row batch, in-process: 545 ms with `SET`; 85 ms with the
     properties inside the `CREATE (…)` pattern. That is 9.2k vs 55.7k rows/s.
2. **Indexes are not partial.** Every property index (declared, a constraint's
   backing index, or built automatically) is
   `CREATE INDEX … ON nodes(json_extract(props, '$."key"'))`, over the whole
   `nodes` table. A node of any label gets an entry (NULL) in every index. The
   importer's pre-load schema (four constraints, one composite index, all on
   other labels) takes Company inserts from 7.7k to 2.8k rows/s. The importer's
   13 post-load indexes made the next `--clean` reload 2.7× slower, which is how
   this was found.
3. **One SQL query per input row for `UNWIND … MATCH (n {key: row.k})`.**
   `scanNodes` is ~23% of server CPU in the relationship and `MERGE` passes
   (and `loadAdj` ~5%). The profile also shows ~31% of CPU in `pread` on the
   database file, even at 500k where it fits in the OS cache.
4. **Each statement commits** (`Tx.Commit`, ~9%).
5. **Smaller:** HTTP and JSON parsing are cheap (29 ms of a 650 ms batch, 4.5%).
   Batch size does not matter (50 to 20,000 rows per request: same rows/s). The
   unique-constraint check costs about as much as the insert (545 ms vs 270 ms
   per batch without it). A 512 MB page cache, `synchronous=NORMAL` and mmap
   together gave only 7% at 500k (3 m 6 s → 2 m 53 s); `synchronous=NORMAL`
   alone, nothing.

## Suggested changes to graingraph

In order of measured payoff:

1. **Partial expression indexes.** Add `WHERE <expr> IS NOT NULL` (all of the
   index's expressions, for a composite one) to `backingIndexSQL` and to the
   automatic `createIndex`. SQLite uses a partial index when the query's
   `expr = ?` implies `expr IS NOT NULL`. Importer schema: 2.5k → 27k rows/s on
   Company inserts (11×). Two sites, a few lines.
2. **Write-behind for `SET`.** Record that a node's properties changed and write
   each changed node once when the statement next touches the database
   (`flushWrites`), in multi-row `UPDATE … FROM (VALUES …)` statements of 400,
   rather than one `UPDATE` per `SET` item. About 45 lines. In-process Company
   load: 9.2k → 43.9k rows/s (4.8×); `MERGE … SET` addresses 10.3k → 21.2k;
   relationship creation (no `SET`) unchanged. Fusing `CREATE (n) SET n.a = …`
   into the insert would do the same for the common case.
3. **Batch the per-row lookups.** For `UNWIND $rows AS row MATCH (n:L {k:
   row.k})`, gather the batch's keys and fetch them in one query
   (`… IN (…)` or a join against a `VALUES` list), or at least cache the
   prepared statement per clause. Not prototyped; the profile suggests up to
   ~25% of the remaining time.
4. **Expose SQLite tuning:** `cache_size`, `mmap_size`, `synchronous` as
   options and `graingraph-query` flags (today only WAL, foreign keys and
   busy timeout are set). Worth little at 500k, expected to matter more when
   the database outgrows the OS cache (not measured).
5. **Neo4j-compatibility for tools:** `CALL dbms.components()` (name,
   versions), a no-op `db.awaitIndexes()`, and a documented way to learn that
   the server is not Neo4j. The discovery `server` field already does the last
   one; this tool relies on it.

[`graingraph-v0.3.1-performance.patch`](graingraph-v0.3.1-performance.patch)
implements 1 and 2 against v0.3.1 (63 lines, 4 files; applies cleanly with
`git apply`). With it, graingraph's own unit tests pass and the openCypher TCK
passes unchanged (3,895 of 3,895, 0 failed; 257 of 257 Neo4j extensions). It is
a prototype for discussion and has not been reviewed upstream. Two things worth
a second look from someone who knows the code: that deferring `SET` writes
cannot be observed by anything that reads through a path other than `flushDB`,
and rollback behaviour on a failed statement (the TCK covers both, but only
what it covers).

## Reproducing

```bash
# graingraph on :7474 with LOAD CSV enabled
GRAINGRAPH_QUERY_PASSWORD=pw graingraph-query --listen 127.0.0.1:7474 \
  --username neo4j --db /tmp/gg.db --import-dir /tmp/import

NEO4J_URI=http://127.0.0.1:7474 NEO4J_USERNAME=neo4j NEO4J_PASSWORD=pw \
  ch-import import --data-dir ./sample --keep-old --clean --yes

# parity against Neo4j (graph fingerprints must match)
NEO4J_TEST_BOLT_URI=neo4j://localhost:7687 NEO4J_TEST_HTTP_URI=http://127.0.0.1:7474 \
NEO4J_TEST_PASSWORD=pw go test -run TestParity -v ./internal/importer
```

Use a **fresh database file** for timing: reloading into a used one is slower
(see the index finding), and the parity test wipes the database it connects to.
