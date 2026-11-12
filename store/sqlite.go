package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

// Config holds optional SQLite configuration applied at Open time.
// Zero value is valid and uses SQLite's defaults.
type Config struct {
	// BusyTimeout sets PRAGMA busy_timeout (milliseconds). When non-zero,
	// SQLite will retry locked operations for up to this duration before
	// returning SQLITE_BUSY. Useful under concurrent write contention.
	BusyTimeout time.Duration

	// ReadConns is the size of the read-connection pool. When positive and the
	// database is file-backed, read-only work runs on that many extra
	// connections (see [SQLiteStore.BeginReadTx]) so readers do not queue
	// behind the single writer. Zero disables the pool; in-memory databases
	// never get one, because a second connection would see a different
	// database.
	ReadConns int

	// CacheSize sets PRAGMA cache_size to that many bytes of page cache,
	// applied as a negative kibibyte count so it is independent of the page
	// size (a positive value would be pages). Zero leaves SQLite's default
	// (about 2 MiB per connection). The setting is per connection, so a
	// database with ReadConns readers uses roughly CacheSize × ReadConns of
	// cache in total.
	CacheSize int64

	// MMapSize sets PRAGMA mmap_size to that many bytes, enabling
	// memory-mapped I/O over the database file and lifting reads off the page
	// cache. A negative value disables mmap (-1); zero leaves SQLite's default
	// untouched. It is best for large files and reader-heavy workloads, and has
	// no effect on an in-memory database.
	MMapSize int64

	// Synchronous sets PRAGMA synchronous on the write connection: 0 (OFF,
	// fastest but loses committed data across a crash or power loss), 1 (NORMAL,
	// durable across a process crash under WAL — the usual tradeoff) and 2 (FULL,
	// synchronising after every commit). SyncUnset leaves journal_mode's default
	// in force. It is applied on the writer only; a query-only reader never
	// writes and so is never asked to apply it.
	Synchronous int

	// PageSize sets PRAGMA page_size to that many bytes for a new database file,
	// which must be a power of two between 512 and 32768 and is applied before any
	// page is written. An existing (non-empty) database keeps its page size, so the
	// setting is a no-op there — the option is safe to pass whether or not the file
	// pre-exists. Zero leaves SQLite's default (4 KiB).
	PageSize int
}

// SyncUnset means "do not set PRAGMA synchronous": journal_mode's default
// (FULL under WAL) stands. Synchronous 0 is itself a real value (OFF), so the
// unset state needs its own sentinel.
const SyncUnset = -1

// SQLiteStore is the SQLite-backed implementation of Store.
// It uses modernc.org/sqlite so no CGO is required.
//
// The q field is the active SQL executor: it is set to db (as a querier) for
// non-transactional operation and to a *sql.Tx for transactional operation.
// The db field is always the underlying connection pool; it is used for
// lifecycle operations (Close, Snapshot, BeginExecTx) regardless of
// transaction state. The tx field is non-nil only within a transaction and
// provides Commit/Rollback; it is always equal to q when non-nil.
type SQLiteStore struct {
	db *sql.DB
	q  querier // *sql.DB (no tx) or *sql.Tx (within a tx)
	tx *sql.Tx // non-nil only when this store is a transaction scope

	// readDB is the read-only connection pool, or nil when the database is
	// in-memory or the pool is disabled. Its connections run with
	// PRAGMA query_only = ON, so they cannot write.
	readDB *sql.DB
}

// Compile-time assertion: *SQLiteStore must satisfy both Store and Tx.
var _ Store = (*SQLiteStore)(nil)
var _ Tx = (*SQLiteStore)(nil)

// Open opens (or creates) a SQLite database at the given URI and returns a
// Store. The URI may be:
//   - ":memory:" for an in-memory database
//   - A file path (absolute or relative) for a persistent database
//   - Any valid modernc.org/sqlite DSN
//
// Open applies the schema DDL and enables WAL journal mode before returning.
func Open(uri string, cfg Config) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", uri, err)
	}

	// SQLite supports only one writer at a time and ":memory:" databases are
	// per-connection. A pool size of 1 ensures all operations share a single
	// connection, which is correct for both file-based and in-memory databases.
	db.SetMaxOpenConns(1)

	// The page size must be fixed before the first page is written, so it goes
	// before WAL and the schema DDL. It only takes on an empty file; an existing
	// database keeps its size (a documented no-op), which keeps the option safe to
	// pass whether or not the file pre-exists.
	if ps := cfg.PageSize; ps > 0 {
		if ps < 512 || ps > 32768 || (ps&(ps-1)) != 0 {
			_ = db.Close()
			return nil, fmt.Errorf("store: page size %d must be a power of two between 512 and 32768 bytes", ps)
		}
		if newDatabaseFile(uri) {
			if _, err := db.Exec(fmt.Sprintf("PRAGMA page_size=%d;", ps)); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("store: set page size: %w", err)
			}
		}
	}

	// Enable WAL mode for better concurrent-reader performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL mode: %w", err)
	}

	// Enforce foreign-key constraints. SQLite disables them by default;
	// enabling here ensures that edge inserts referencing non-existent node IDs
	// are rejected at the database level.
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}

	if cfg.BusyTimeout > 0 {
		ms := cfg.BusyTimeout.Milliseconds()
		if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d;", ms)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: set busy_timeout: %w", err)
		}
	}

	// Per-connection memory and durability tuning. The write connection is the one
	// and only pool member (see SetMaxOpenConns), so an inline PRAGMA covers it; the
	// same values are threaded into the read pool's DSN, where each pooled connection
	// must receive them independently.
	if cfg.CacheSize > 0 {
		// cache_size: bytes come in, but a negative value is kibibytes of cache
		// (page-size independent); a positive one would be a page count.
		if _, err := db.Exec(fmt.Sprintf("PRAGMA cache_size=%d;", -cfg.CacheSize/1024)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: set page cache: %w", err)
		}
	}
	if cfg.MMapSize != 0 {
		// mmap_size: a negative value disables memory-mapped I/O (-1); a positive
		// one sets the cap in bytes. ignored on an in-memory database.
		if _, err := db.Exec(fmt.Sprintf("PRAGMA mmap_size=%d;", cfg.MMapSize)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: set memory-mapped I/O size: %w", err)
		}
	}
	if cfg.Synchronous != SyncUnset {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA synchronous=%d;", cfg.Synchronous)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: set synchronous: %w", err)
		}
	}

	// Apply schema DDL (idempotent via IF NOT EXISTS guards).
	if _, err := db.Exec(schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	// Backfill node_labels for any pre-existing nodes that were created before
	// the junction table was added. This is a no-op on fresh databases.
	if _, err := db.Exec(backfillMigrationSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: backfill node_labels: %w", err)
	}

	s := &SQLiteStore{db: db, q: db}
	if cfg.ReadConns > 0 && fileBacked(uri) {
		readDB, err := openReadPool(uri, cfg)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		s.readDB = readDB
	}
	return s, nil
}

// fileBacked reports whether uri names a database file (as opposed to an
// in-memory database, whose connections each see a separate database).
func fileBacked(uri string) bool {
	u := strings.ToLower(uri)
	return uri != "" && !strings.Contains(u, ":memory:") && !strings.Contains(u, "mode=memory")
}

// newDatabaseFile reports whether uri names a database that has no pages yet: an
// in-memory database, or a file-backed one that does not exist. It is what makes
// WithPageSize a no-op on an existing file rather than an error — SQLite fixes the
// page size when the first page is written, so re-opening a populated database and
// requesting a new size silently keeps the old one. A stat error other than "not
// exist" is treated as existing, so a best-effort page-size request never blocks
// opening a database it can otherwise reach.
func newDatabaseFile(uri string) bool {
	if !fileBacked(uri) { // in-memory or empty: nothing was ever written to disk
		return true
	}
	_, err := os.Stat(uri)
	return os.IsNotExist(err) // only a truly-absent file is "new"
}

// openReadPool opens the read-only pool. The pragmas go in the DSN so that
// every connection the pool opens gets them, not just the first.
func openReadPool(uri string, cfg Config) (*sql.DB, error) {
	pragmas := []string{"query_only(1)", "foreign_keys(1)"}
	if cfg.BusyTimeout > 0 {
		pragmas = append(pragmas, fmt.Sprintf("busy_timeout(%d)", cfg.BusyTimeout.Milliseconds()))
	}
	// Per-connection reads must get the memory tuning too, as every pooled
	// connection is independent of the writer's inline PRAGMAs. The same
	// kibibyte form is used so a page-size change keeps the meaning stable.
	if cfg.CacheSize > 0 {
		pragmas = append(pragmas, fmt.Sprintf("cache_size(%d)", -cfg.CacheSize/1024))
	}
	if cfg.MMapSize != 0 {
		pragmas = append(pragmas, fmt.Sprintf("mmap_size(%d)", cfg.MMapSize))
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	var q []string
	for _, p := range pragmas {
		q = append(q, "_pragma="+p)
	}
	db, err := sql.Open("sqlite", uri+sep+strings.Join(q, "&"))
	if err != nil {
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}
	db.SetMaxOpenConns(cfg.ReadConns)
	db.SetMaxIdleConns(cfg.ReadConns)
	// Fail now, not on the first query, if the database cannot be read.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}
	return db, nil
}

// HasReadPool reports whether read-only work runs on a separate pool.
func (s *SQLiteStore) HasReadPool() bool { return s.readDB != nil }

// BeginReadTx starts a read-only transaction. With a read pool it runs on one
// of the pool's connections: every statement in it sees one consistent
// snapshot (WAL readers never block, and are never blocked by, the writer) and
// any attempt to write fails. Without a pool it is a normal transaction on the
// single connection, which is what in-memory databases need.
func (s *SQLiteStore) BeginReadTx(ctx context.Context) (TxExecer, error) {
	if s.readDB == nil {
		return s.BeginExecTx(ctx)
	}
	tx, err := s.readDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin read tx: %w", err)
	}
	return tx, nil
}

// DB returns the underlying *sql.DB. This method is on the concrete type only
// and is not part of the Store interface — use Exec() in interface-typed code.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Exec returns the active SQL executor as a store.Execer. When called on a
// transaction-scoped store, the returned Execer runs within that transaction.
func (s *SQLiteStore) Exec() Execer { return s.q }

// BeginExecTx starts a new transaction and returns a TxExecer.
// Returns an error if called on an already-transactional store.
func (s *SQLiteStore) BeginExecTx(ctx context.Context) (TxExecer, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("store: cannot nest transactions")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin exec tx: %w", err)
	}
	return tx, nil
}

// Close releases all resources held by the store.
func (s *SQLiteStore) Close() error {
	var readErr error
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}
	if err := s.db.Close(); err != nil {
		return err
	}
	return readErr
}

// Snapshot writes an atomic, consistent copy of the database to path using
// VACUUM INTO. path must not already exist.
//
// Path-traversal protection: the path is cleaned via filepath.Clean and any
// remaining ".." components are rejected. Symlinks in the parent directory
// are resolved so that ".." after symlink expansion is also caught. Note that
// absolute paths with no ".." components are accepted; callers that need to
// restrict snapshot destinations to a specific directory must enforce that
// constraint themselves.
func (s *SQLiteStore) Snapshot(path string) error {
	// Apply the same path-traversal protection used in driver.Open: clean the
	// path, resolve parent-directory symlinks, then reject any ".." component.
	cleaned := filepath.Clean(path)
	if dir, err := filepath.EvalSymlinks(filepath.Dir(cleaned)); err == nil {
		cleaned = filepath.Join(dir, filepath.Base(cleaned))
	}
	if slices.Contains(strings.Split(cleaned, string(filepath.Separator)), "..") {
		return fmt.Errorf("store: snapshot: path traversal not allowed: %q", path)
	}

	if _, err := s.db.Exec("VACUUM INTO ?", cleaned); err != nil {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	return nil
}

// Begin starts a new database transaction and returns a Tx. The returned Tx
// is a *SQLiteStore whose querier is set to the underlying *sql.Tx so all
// CRUD methods execute within the transaction without per-method overrides.
// Returns an error if called on an already-transactional store.
func (s *SQLiteStore) Begin(ctx context.Context) (Tx, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("store: cannot nest transactions")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin transaction: %w", err)
	}
	return &SQLiteStore{db: s.db, q: tx, tx: tx}, nil
}

// Commit commits the transaction. Returns an error if the store is not in a
// transaction scope.
func (s *SQLiteStore) Commit() error {
	if s.tx == nil {
		return fmt.Errorf("store: commit called on non-transactional store")
	}
	return s.tx.Commit()
}

// Rollback aborts the transaction. Returns an error if the store is not in a
// transaction scope.
func (s *SQLiteStore) Rollback() error {
	if s.tx == nil {
		return fmt.Errorf("store: rollback called on non-transactional store")
	}
	return s.tx.Rollback()
}

// --- Node operations ---

// InsertNode inserts a new node and returns its integer ID.
func (s *SQLiteStore) InsertNode(ctx context.Context, labels Labels, propsJSON string) (int64, error) {
	id, err := insertNode(ctx, s.q, labels, propsJSON)
	if err != nil {
		return 0, err
	}
	if err := writeNodeLabels(ctx, s.q, id, labels); err != nil {
		return 0, err
	}
	return id, nil
}

// GetNode returns the node with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetNode(ctx context.Context, id int64) (*NodeRow, error) {
	return getNode(ctx, s.q, id)
}

// DeleteNode removes the node with the given ID.
func (s *SQLiteStore) DeleteNode(ctx context.Context, id int64) error {
	return deleteNode(ctx, s.q, id)
}

// ListNodes returns all nodes.
func (s *SQLiteStore) ListNodes(ctx context.Context) ([]*NodeRow, error) {
	return listNodes(ctx, s.q)
}

// ListNodesByLabel returns nodes whose labels column contains labelName.
func (s *SQLiteStore) ListNodesByLabel(ctx context.Context, labelName string) ([]*NodeRow, error) {
	return listNodesByLabel(ctx, s.q, labelName)
}

// UpdateNodeProps replaces the props JSON for the node with the given ID.
func (s *SQLiteStore) UpdateNodeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateNodeProps(ctx, s.q, id, propsJSON)
}

// --- Edge operations ---

// InsertEdge inserts a new edge and returns its integer ID.
func (s *SQLiteStore) InsertEdge(ctx context.Context, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	return insertEdge(ctx, s.q, edgeType, startID, endID, propsJSON)
}

// GetEdge returns the edge with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetEdge(ctx context.Context, id int64) (*EdgeRow, error) {
	return getEdge(ctx, s.q, id)
}

// DeleteEdge removes the edge with the given ID.
func (s *SQLiteStore) DeleteEdge(ctx context.Context, id int64) error {
	return deleteEdge(ctx, s.q, id)
}

// ListEdges returns all edges.
func (s *SQLiteStore) ListEdges(ctx context.Context) ([]*EdgeRow, error) {
	return listEdges(ctx, s.q)
}

// ListEdgesByType returns edges with the given relationship type.
func (s *SQLiteStore) ListEdgesByType(ctx context.Context, edgeType string) ([]*EdgeRow, error) {
	return listEdgesByType(ctx, s.q, edgeType)
}

// ListEdgesByStartNode returns all edges whose start_id equals startID.
func (s *SQLiteStore) ListEdgesByStartNode(ctx context.Context, startID int64) ([]*EdgeRow, error) {
	return listEdgesByStartNode(ctx, s.q, startID)
}

// ListEdgesByEndNode returns all edges whose end_id equals endID.
func (s *SQLiteStore) ListEdgesByEndNode(ctx context.Context, endID int64) ([]*EdgeRow, error) {
	return listEdgesByEndNode(ctx, s.q, endID)
}

// EdgeExistsForNode returns true if any edge references the given node ID.
func (s *SQLiteStore) EdgeExistsForNode(ctx context.Context, nodeID int64) (bool, error) {
	return edgeExistsForNode(ctx, s.q, nodeID)
}

// UpdateEdgeProps replaces the props JSON for the edge with the given ID.
func (s *SQLiteStore) UpdateEdgeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateEdgeProps(ctx, s.q, id, propsJSON)
}

// ============================================================================
// querier — shared interface for *sql.DB and *sql.Tx
// ============================================================================

// querier is satisfied by both *sql.DB and *sql.Tx, allowing helper functions
// to be reused for both non-transactional and transactional contexts.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ============================================================================
// Helper functions (shared for both non-transactional and transactional use)
// ============================================================================

// writeNodeLabels records a node's labels in node_labels (one row per distinct
// label). The nodes table's comma-separated labels column is written by the
// caller; the two are kept in step by every writer of nodes.labels.
func writeNodeLabels(ctx context.Context, q querier, id int64, labels Labels) error {
	seen := make(map[string]bool, len(labels))
	var sb strings.Builder
	var args []any
	for _, l := range labels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		if len(args) > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?)")
		args = append(args, id, l)
	}
	if len(args) == 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO node_labels (node_id, label) VALUES `+sb.String(), args...); err != nil {
		return fmt.Errorf("store: insert node labels: %w", err)
	}
	return nil
}

func insertNode(ctx context.Context, q querier, labels Labels, propsJSON string) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO nodes (labels, props) VALUES (?, json(?))`,
		labels.Encode(), propsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert node: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert node last-id: %w", err)
	}
	return id, nil
}

func getNode(ctx context.Context, q querier, id int64) (*NodeRow, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, labels, props FROM nodes WHERE id = ?`, id,
	)
	var n NodeRow
	if err := row.Scan(&n.ID, &n.Labels, &n.Props); err != nil {
		return nil, err // preserve sql.ErrNoRows
	}
	return &n, nil
}

func deleteNode(ctx context.Context, q querier, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete node %d: %w", id, err)
	}
	return nil
}

func listNodes(ctx context.Context, q querier) ([]*NodeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, labels, props FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanNodeRows(rows)
}

func listNodesByLabel(ctx context.Context, q querier, labelName string) ([]*NodeRow, error) {
	// Use the idx_node_labels_label index via a JOIN against the node_labels
	// junction table. This replaces the previous LIKE-based scan and avoids
	// full table scans on the nodes table.
	rows, err := q.QueryContext(ctx,
		`SELECT n.id, n.labels, n.props
		 FROM nodes n
		 JOIN node_labels nl ON nl.node_id = n.id
		 WHERE nl.label = ?`,
		labelName,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes by label %q: %w", labelName, err)
	}
	defer func() { _ = rows.Close() }()
	return scanNodeRows(rows)
}

func updateNodeProps(ctx context.Context, q querier, id int64, propsJSON string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE nodes SET props = json(?) WHERE id = ?`,
		propsJSON, id,
	)
	if err != nil {
		return fmt.Errorf("store: update node props %d: %w", id, err)
	}
	return nil
}

func insertEdge(ctx context.Context, q querier, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, json(?))`,
		edgeType, startID, endID, propsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert edge: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert edge last-id: %w", err)
	}
	return id, nil
}

func getEdge(ctx context.Context, q querier, id int64) (*EdgeRow, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE id = ?`, id,
	)
	var e EdgeRow
	if err := row.Scan(&e.ID, &e.Type, &e.StartID, &e.EndID, &e.Props); err != nil {
		return nil, err // preserve sql.ErrNoRows
	}
	return &e, nil
}

func deleteEdge(ctx context.Context, q querier, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM edges WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete edge %d: %w", id, err)
	}
	return nil
}

func listEdges(ctx context.Context, q querier) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, type, start_id, end_id, props FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("store: list edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByType(ctx context.Context, q querier, edgeType string) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE type = ?`, edgeType,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by type %q: %w", edgeType, err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByStartNode(ctx context.Context, q querier, startID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE start_id = ?`, startID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by start_id %d: %w", startID, err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByEndNode(ctx context.Context, q querier, endID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE end_id = ?`, endID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by end_id %d: %w", endID, err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func edgeExistsForNode(ctx context.Context, q querier, nodeID int64) (bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM edges WHERE start_id = ? OR end_id = ? LIMIT 1)`,
		nodeID, nodeID,
	)
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("store: edge exists for node %d: %w", nodeID, err)
	}
	return exists, nil
}

func updateEdgeProps(ctx context.Context, q querier, id int64, propsJSON string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE edges SET props = json(?) WHERE id = ?`,
		propsJSON, id,
	)
	if err != nil {
		return fmt.Errorf("store: update edge props %d: %w", id, err)
	}
	return nil
}

// ============================================================================
// Row scanners
// ============================================================================

func scanNodeRows(rows *sql.Rows) ([]*NodeRow, error) {
	var result []*NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.ID, &n.Labels, &n.Props); err != nil {
			return nil, fmt.Errorf("store: scan node row: %w", err)
		}
		result = append(result, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate node rows: %w", err)
	}
	return result, nil
}

func scanEdgeRows(rows *sql.Rows) ([]*EdgeRow, error) {
	var result []*EdgeRow
	for rows.Next() {
		var e EdgeRow
		if err := rows.Scan(&e.ID, &e.Type, &e.StartID, &e.EndID, &e.Props); err != nil {
			return nil, fmt.Errorf("store: scan edge row: %w", err)
		}
		result = append(result, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate edge rows: %w", err)
	}
	return result, nil
}
