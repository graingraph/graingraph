package interp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/graingraph/graingraph/cypher/proc"
	"github.com/graingraph/graingraph/cypher/syntax"
)

// hardMaxHops is the deepest variable-length expansion the interpreter will
// recurse into when no MaxPathHops is configured: each hop costs a few Go
// frames, so an unlimited search over a long chain would overflow the stack,
// which is fatal rather than an error.
const hardMaxHops = 100000

// maxHops is the effective cap on variable-length expansion.
func (e *Engine) maxHops() int64 {
	if e != nil && e.MaxPathHops > 0 {
		return int64(e.MaxPathHops)
	}
	return hardMaxHops
}

// Engine is the per-database state the interpreter keeps between statements:
// registered procedures and the property-index advisor.
type Engine struct {
	// Procs are the procedures callable with CALL, besides the built-ins.
	Procs proc.Set
	// MaxPathHops, when positive, caps variable-length patterns: an unbounded
	// pattern stops at this many hops and an explicit upper bound above it is
	// an error. Zero means the built-in limit of hardMaxHops, which keeps the
	// recursive path search inside the goroutine stack.
	MaxPathHops int
	// NoAutoIndexes disables the automatic property indexes.
	NoAutoIndexes bool
	// ImportDir is the directory LOAD CSV reads from; empty disables LOAD CSV.
	ImportDir string

	mu      sync.Mutex
	loaded  bool
	indexed map[string]bool // property keys with an automatic index
	scans   map[string]int  // how often each key was used to narrow a scan
	wanted  map[string]bool // keys read-only statements asked to have indexed

	// VectorCacheBytes bounds the memory of the cached vector matrices in
	// total (default DefaultVectorCacheBytes).
	VectorCacheBytes int64
	vecMu            sync.Mutex
	vec              vecCache

	stmtMu    sync.RWMutex
	stmts     map[string]*syntax.Statement // parsed and analysed statements, by text
	stmtBytes int                          // estimated size of stmts
}

// maxCachedStatements bounds the statement cache by count, and
// maxCachedStatementBytes by size (an AST costs several times its text); it is
// emptied when either is reached. A statement bigger than maxCacheableBytes is
// not cached at all: it is rarely repeated and could evict everything else.
const (
	maxCachedStatements     = 512
	maxCachedStatementBytes = 32 << 20
	maxCacheableBytes       = 64 << 10
	astBytesPerSourceByte   = 32
	maxTrackedScanKeys      = 1024
)

// Statement returns the cached statement for query text, if any. Statements
// are immutable once built and may be shared between goroutines.
func (e *Engine) Statement(q string) (*syntax.Statement, bool) {
	e.stmtMu.RLock()
	defer e.stmtMu.RUnlock()
	st, ok := e.stmts[q]
	return st, ok
}

// CacheStatement remembers a parsed and analysed statement.
func (e *Engine) CacheStatement(q string, st *syntax.Statement) {
	e.stmtMu.Lock()
	defer e.stmtMu.Unlock()
	if len(q) > maxCacheableBytes {
		return
	}
	cost := (len(q) + 64) * astBytesPerSourceByte
	if e.stmts == nil || len(e.stmts) >= maxCachedStatements || e.stmtBytes+cost > maxCachedStatementBytes {
		e.stmts = make(map[string]*syntax.Statement, 64)
		e.stmtBytes = 0
	}
	e.stmts[q] = st
	e.stmtBytes += cost
}

// ClearStatements drops the statement cache. Call it when the set of
// procedures changes, since analysis depends on it.
func (e *Engine) ClearStatements() {
	e.stmtMu.Lock()
	e.stmts = nil
	e.stmtBytes = 0
	e.stmtMu.Unlock()
}

// Automatic property indexes. A key becomes indexed once it has narrowed node
// scans indexScansBeforeIndex times on a graph of at least indexMinNodes nodes
// (a scan of fewer nodes costs less than maintaining an index), capped at
// indexMax indexes. Indexes are never dropped automatically.
const (
	indexPrefix           = "idx_auto_np_"
	indexScansBeforeIndex = 3
	indexMinNodes         = 500
	indexMax              = 32
)

// ResetIndexState forgets what the engine knows about existing indexes. Call it
// after a rolled-back statement, which may have undone an index creation.
func (e *Engine) ResetIndexState() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.loaded = false
	e.mu.Unlock()
}

// noteScan records that key narrowed a node scan and creates an expression
// index on it when the policy says so. Failures (a read-only database, say)
// are ignored: an index only ever makes queries faster.
func (e *Engine) noteScan(ctx context.Context, db DB, key string, readOnly bool) {
	if e == nil || e.NoAutoIndexes {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.loaded {
		e.indexed = map[string]bool{}
		if e.scans == nil {
			e.scans = map[string]int{}
		}
		rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE ?`, indexPrefix+"%")
		if err != nil {
			return
		}
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				e.indexed[strings.TrimPrefix(name, indexPrefix)] = true
			}
		}
		rows.Close()
		e.loaded = true
	}
	id := fmt.Sprintf("%x", key)
	if e.indexed[id] {
		return
	}
	if _, seen := e.scans[key]; !seen && len(e.scans) >= maxTrackedScanKeys {
		return // bounded: keys come from statements and parameters
	}
	e.scans[key]++
	if e.scans[key] < indexScansBeforeIndex || len(e.indexed) >= indexMax {
		return
	}
	var maxID int64
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM nodes`)
	if err != nil {
		return
	}
	if rows.Next() {
		_ = rows.Scan(&maxID)
	}
	rows.Close()
	if maxID < indexMinNodes {
		return
	}
	if readOnly {
		// A read-only connection cannot create the index; ask the caller to do
		// it on the write connection once this statement is done.
		if e.wanted == nil {
			e.wanted = map[string]bool{}
		}
		e.wanted[key] = true
		e.scans[key] = -1 << 29 // do not queue it again
		return
	}
	if err := e.createIndex(ctx, db, key); err != nil {
		e.scans[key] = -1 << 30 // do not retry
	}
}

// createIndex creates the expression index for key. The caller holds e.mu.
// It is a partial index (entries only for nodes that actually have `key`, so
// most writes touch it not at all); an `expr = <value>` lookup still proves the
// row qualifies, so SQLite uses it as before.
func (e *Engine) createIndex(ctx context.Context, db DB, key string) error {
	id := fmt.Sprintf("%x", key)
	// key is restricted to [A-Za-z0-9_] by pushableKey, so it is safe to inline.
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s%s ON nodes(json_extract(props, '$."%s"')) WHERE json_extract(props, '$."%s"') IS NOT NULL`, indexPrefix, id, key, key))
	if err == nil {
		e.indexed[id] = true
	}
	return err
}

// TakeWantedIndexes returns, and forgets, the property keys that read-only
// statements found worth indexing. Create them with CreateIndexes on the write
// connection.
func (e *Engine) TakeWantedIndexes() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var keys []string
	for k := range e.wanted {
		keys = append(keys, k)
	}
	e.wanted = nil
	sort.Strings(keys)
	return keys
}

// CreateIndexes creates the automatic indexes for keys on db, which must be
// able to write. Failures (a read-only database, say) are ignored: an index only
// ever makes queries faster.
func (e *Engine) CreateIndexes(ctx context.Context, db DB, keys []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.indexed == nil {
		e.indexed = map[string]bool{}
	}
	for _, k := range keys {
		if pushableKey(k) {
			_ = e.createIndex(ctx, db, k)
		}
	}
}
