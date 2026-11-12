package graingraph

import (
	"context"
	"testing"
	"time"
)

// Option is a functional option for configuring a graingraph database.
// Pass one or more Options to [Open] to customise behaviour.
type Option func(*dbConfig)

// defaultReadConns is the read-pool size of a file-backed database.
//
// Two is deliberate. The pure-Go SQLite shares allocator and file-lock state
// between connections, so heavy concurrent scans slow each other down: on a
// 20,000-node scan two readers run 1.3x faster than one, but four run 0.6x as
// fast. Light queries (indexed lookups) gain about 1.8x with two and gain
// nothing more from more; raise it with WithMaxReadConns only after measuring.
const defaultReadConns = 2

type dbConfig struct {
	busyTimeout time.Duration
	readOnly    bool
	maxPathHops int
	importDir   string
	readConns   int
	vectorCache int64
	noAutoIndex bool
	// Per-connection SQLite tuning, applied at Open time. All are optional; a
	// zero value leaves SQLite's default (see the With* constructors).
	cacheSize int64
	mmapSize  int64
	syncMode  SyncMode // zero is SyncDefault: leave journal_mode's default
	pageSize  int
}

// WithBusyTimeout sets the SQLite busy_timeout pragma. When a write operation
// encounters a locked database, SQLite will retry for up to d before returning
// an error. The default (zero) uses SQLite's built-in behaviour (no retry).
//
// Useful when multiple goroutines or processes share the same database file.
func WithBusyTimeout(d time.Duration) Option {
	return func(c *dbConfig) { c.busyTimeout = d }
}

// WithReadOnly opens the database in read-only mode. Write queries issued via
// [DB.RunQuery] return [ErrReadOnly], and [DB.BeginTx] returns [ErrReadOnly]
// before any transaction is opened. The database file must already exist and
// contain the graingraph schema.
//
// Read-only enforcement is applied in the graingraph API layer: any Cypher
// statement that would mutate the graph (CREATE, SET, DELETE, MERGE) is
// rejected before reaching SQLite.
func WithReadOnly() Option {
	return func(c *dbConfig) { c.readOnly = true }
}

// WithMaxPathHops caps the number of hops in variable-length Cypher path
// patterns such as MATCH (a)-[*1..n]->(b). An unbounded pattern (-[*]->) stops
// at n hops, and an explicit upper bound above n returns an error.
//
// By default there is no cap (relationships are never reused within a path, so
// traversal always terminates, but it can take exponential time on dense
// graphs). Passing n <= 0 is a no-op.
func WithMaxPathHops(n int) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.maxPathHops = n
		}
	}
}

// NewTestDB opens an in-memory graingraph database, registers db.Close with
// t.Cleanup, and returns a ready-to-use *DB. t.Fatal is called on any error.
//
// This is the recommended way to create a graingraph database in tests:
//
//	func TestMyFeature(t *testing.T) {
//	    db := graingraph.NewTestDB(t)
//	    // db is closed automatically when the test ends
//	}
func NewTestDB(t testing.TB, opts ...Option) *DB {
	t.Helper()
	db, err := Open(":memory:", opts...)
	if err != nil {
		t.Fatalf("graingraph.NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// WithImportDirectory enables LOAD CSV and restricts it to files below dir:
// LOAD CSV FROM 'file:///people.csv' reads dir/people.csv, and nothing outside
// dir can be read. Without this option LOAD CSV is disabled, so a query cannot
// read arbitrary files.
func WithImportDirectory(dir string) Option {
	return func(c *dbConfig) { c.importDir = dir }
}

// WithMaxReadConns sets how many read-only connections a file-backed database
// keeps for concurrent queries (default 2). Statements without updating
// clauses run on these connections, each in a snapshot transaction, so readers
// never wait for the writer and the writer never waits for readers. In-memory
// databases have a single connection and ignore this option. n <= 0 is a
// no-op.
func WithMaxReadConns(n int) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.readConns = n
		}
	}
}

// WithMaxVectorCacheBytes bounds the memory used by the in-memory vector
// matrices that serve db.index.vector.queryNodes (default 512 MiB across all
// vector indexes). A search on an index that does not fit is still answered, from
// a matrix built for that one query, which is slower. n <= 0 is a no-op.
func WithMaxVectorCacheBytes(n int64) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.vectorCache = n
		}
	}
}

// WithoutAutomaticIndexes stops graingraph creating property indexes on its own.
// By default, once equality filters on a property have narrowed a few scans of a
// graph with at least 500 nodes, an index on that property is created in the
// background; use this option if you want to control the schema yourself and
// create indexes only with CREATE INDEX or [DB.CreatePropertyIndex].
func WithoutAutomaticIndexes() Option {
	return func(c *dbConfig) { c.noAutoIndex = true }
}

// SyncMode selects the durability level applied to PRAGMA synchronous on the
// write connection (see SQLite's "WAL and Durability"). It is named by
// WithSyncMode; SyncDefault leaves journal_mode's default in force. Under WAL,
// SyncNormal is durable across a process crash but not a power loss or OS crash,
// which is the usual speed/durability tradeoff; SyncOff is fastest and can lose
// committed data — use it deliberately.
type SyncMode int

const (
	// SyncDefault leaves PRAGMA synchronous at journal_mode's default (FULL).
	SyncDefault SyncMode = iota
	// SyncOff synchronises as little as possible; fastest, and lossy across a
	// crash or power loss.
	SyncOff
	// SyncNormal is durable across a process crash under WAL, but not a power
	// loss. The recommended setting once WAL is in use.
	SyncNormal
	// SyncFull synchronises after every commit; the safest.
	SyncFull
)

// WithPageCache sets the SQLite page cache to n bytes by applying
// PRAGMA cache_size as a negative kibibyte count (-n/1024), which is independent
// of the page size (a positive value would be a page count). The setting is per
// connection, so a database opened with WithMaxReadConns(r) uses roughly n × r of
// cache in total. n <= 0 leaves SQLite's default (about 2 MiB); it has no effect
// on an in-memory database.
func WithPageCache(n int64) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.cacheSize = n
		}
	}
}

// WithMMapSize enables memory-mapped I/O over the database file of n bytes via
// PRAGMA mmap_size, lifting reads off the page cache and onto the mapping. A
// positive n sets the cap; a negative value disables mmap (-1); zero leaves
// SQLite's default untouched. It is most useful for large files that outgrow the
// file-system cache (see [WithPageCache]), and has no effect on an in-memory
// database.
func WithMMapSize(n int64) Option {
	return func(c *dbConfig) {
		if n != 0 {
			c.mmapSize = n
		}
	}
}

// WithSyncMode sets PRAGMA synchronous on the write connection to m (see
// SyncMode). It is a durability-versus-speed tradeoff; the default SyncDefault
// applies no pragma and leaves journal_mode's FULL in place.
func WithSyncMode(m SyncMode) Option {
	return func(c *dbConfig) { c.syncMode = m }
}

// WithPageSize requests a page size of n bytes for a new database file, applied
// by PRAGMA page_size before the first page is written. It must be a power of two
// between 512 and 32768, otherwise [Open] returns an error. On a file that already
// holds pages the setting is a no-op (SQLite fixes the size at creation), so it
// matters only for new databases; n <= 0 leaves SQLite's default (4 KiB).
func WithPageSize(n int) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.pageSize = n
		}
	}
}
