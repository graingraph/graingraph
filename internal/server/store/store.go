// Package store wraps a graingraph database with the request-scoped services
// the HTTP API needs: auto-commit queries, an explicit-transaction
// registry with idle reaping, and Neo4j-style bookmark generation.
package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/graingraph/graingraph"
)

// bookmarkCounter produces opaque, unique bookmarks: a monotonically
// increasing sequence number plus random bytes.
type bookmarkCounter struct {
	mu  sync.Mutex
	seq uint64
}

func (c *bookmarkCounter) next() string {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf[0:8], seq)
	_, _ = rand.Read(buf[8:])
	return "FB:" + base64.StdEncoding.EncodeToString(buf)
}

// OpenTx is an explicit transaction owned by the store.
type OpenTx struct {
	// ID is the short identifier returned to clients.
	ID string
	// Meta is the optional client-supplied transaction metadata.
	Meta map[string]any

	tx *graingraph.Tx
	mu sync.Mutex // serialises Run / Commit / Rollback

	// timeout is the idle timeout (zero: none); it is immutable after Begin.
	timeout time.Duration
	// timeMu guards expiresAt. It is separate from mu so that Touch and
	// ExpiresAt never wait behind a long-running query.
	timeMu    sync.Mutex
	expiresAt time.Time // zero: no expiry
}

// ExpiresAt reports the transaction's idle expiry, or zero time when the
// transaction has no timeout.
func (t *OpenTx) ExpiresAt() time.Time {
	t.timeMu.Lock()
	defer t.timeMu.Unlock()
	return t.expiresAt
}

// expired reports whether the transaction's idle expiry has passed.
func (t *OpenTx) expired(now time.Time) bool {
	t.timeMu.Lock()
	defer t.timeMu.Unlock()
	return !t.expiresAt.IsZero() && now.After(t.expiresAt)
}

// Store wraps an open graingraph database.
type Store struct {
	db  *graingraph.DB
	mu  sync.Mutex
	txs map[string]*OpenTx
	// beginning is set while a transaction is being opened (see Begin).
	beginning bool

	bookmarks bookmarkCounter
	started   time.Time
	// txWait is how long a request waits for an open transaction to end
	// before failing with a *TransactionOpenError (zero: fail at once).
	txWait time.Duration
}

// Option customises store construction.
type Option func(*storeConfig)

type storeConfig struct {
	txWait    time.Duration
	readOnly  bool
	importDir string
	// Graingraph database tuning, forwarded to graingraph.Open when set. A zero
	// value leaves the corresponding SQLite default in place.
	cacheSize int64
	mmapSize  int64
	syncMode  graingraph.SyncMode // zero is SyncDefault: leave journal_mode's default
	pageSize  int
}

// WithReadOnly opens the graingraph database in read-only mode.
func WithReadOnly() Option { return func(c *storeConfig) { c.readOnly = true } }

// WithTxWait makes a request that meets an open explicit transaction wait up
// to d for it to end, instead of failing at once. Tools that open several short
// transactions concurrently (Neo4j Bloom does) need it; a client that holds a
// transaction open while issuing other requests still gets the error, after d.
func WithTxWait(d time.Duration) Option { return func(c *storeConfig) { c.txWait = d } }

// WithImportDirectory enables LOAD CSV restricted to files below dir.
func WithImportDirectory(dir string) Option {
	return func(c *storeConfig) { c.importDir = dir }
}

// WithPageCache sets the SQLite page cache in bytes (see graingraph.WithPageCache).
func WithPageCache(n int64) Option { return func(c *storeConfig) { c.cacheSize = n } }

// WithMMapSize sets memory-mapped I/O of the database file in bytes; a negative
// value disables it (see graingraph.WithMMapSize).
func WithMMapSize(n int64) Option { return func(c *storeConfig) { c.mmapSize = n } }

// WithSyncMode sets PRAGMA synchronous on the write connection (see
// graingraph.SyncMode); SyncDefault applies no pragma.
func WithSyncMode(m graingraph.SyncMode) Option {
	return func(c *storeConfig) { c.syncMode = m }
}

// WithPageSize sets the page size in bytes for a new database file (see
// graingraph.WithPageSize).
func WithPageSize(n int) Option { return func(c *storeConfig) { c.pageSize = n } }

// New opens (or creates) the graingraph database at path.
func New(path string, opts ...Option) (*Store, error) {
	var cfg storeConfig
	for _, o := range opts {
		o(&cfg)
	}
	var dbOpts []graingraph.Option
	if cfg.readOnly {
		dbOpts = append(dbOpts, graingraph.WithReadOnly())
	}
	if cfg.importDir != "" {
		dbOpts = append(dbOpts, graingraph.WithImportDirectory(cfg.importDir))
	}
	// Each tuning option is forwarded only when set, so a server that does not
	// pass one leaves SQLite's default in place (matching the graingraph layer).
	if cfg.cacheSize > 0 {
		dbOpts = append(dbOpts, graingraph.WithPageCache(cfg.cacheSize))
	}
	if cfg.mmapSize != 0 {
		dbOpts = append(dbOpts, graingraph.WithMMapSize(cfg.mmapSize))
	}
	if cfg.syncMode != graingraph.SyncDefault {
		dbOpts = append(dbOpts, graingraph.WithSyncMode(cfg.syncMode))
	}
	if cfg.pageSize > 0 {
		dbOpts = append(dbOpts, graingraph.WithPageSize(cfg.pageSize))
	}
	db, err := graingraph.Open(path, dbOpts...)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	return &Store{
		db:      db,
		txs:     map[string]*OpenTx{},
		started: time.Now(),
		txWait:  cfg.txWait,
	}, nil
}

// Close releases the database and rolls back every open transaction.
func (s *Store) Close() error {
	// Detach the transactions under s.mu, then roll them back without it:
	// Commit and Rollback take t.mu before s.mu, so holding s.mu while
	// locking t.mu would invert the lock order and could deadlock.
	s.mu.Lock()
	open := make([]*OpenTx, 0, len(s.txs))
	for id, t := range s.txs {
		open = append(open, t)
		delete(s.txs, id)
	}
	s.mu.Unlock()
	for _, t := range open {
		t.mu.Lock()
		if t.tx != nil {
			_ = t.tx.Rollback()
			t.tx = nil
		}
		t.mu.Unlock()
	}
	return s.db.Close(context.Background())
}

// Outcome is the materialised result of a query: fields, rows (as raw
// graingraph values) and optional mutation counters.
type Outcome struct {
	Fields   []string
	Values   [][]any
	Counters *Counters
}

// Counters mirrors the graingraph mutation counters in the response shape.
type Counters struct {
	ContainsUpdates bool `json:"containsUpdates"`

	NodesCreated         int `json:"nodesCreated"`
	NodesDeleted         int `json:"nodesDeleted"`
	PropertiesSet        int `json:"propertiesSet"`
	RelationshipsCreated int `json:"relationshipsCreated"`
	RelationshipsDeleted int `json:"relationshipsDeleted"`
	LabelsAdded          int `json:"labelsAdded"`
	LabelsRemoved        int `json:"labelsRemoved"`
	IndexesAdded         int `json:"indexesAdded"`
	IndexesRemoved       int `json:"indexesRemoved"`
	ConstraintsAdded     int `json:"constraintsAdded"`
	ConstraintsRemoved   int `json:"constraintsRemoved"`

	ContainsSystemUpdates bool `json:"containsSystemUpdates"`
	SystemUpdates         int  `json:"systemUpdates"`
}

// countersOf reports a statement's side effects from graingraph's Statistics,
// which count each change as it happens, as Neo4j does. (graingraph's Counters
// net the changes out for the openCypher TCK: a node created and deleted in
// one statement would leave no trace, and removing a property would not be a
// property set.)
func countersOf(sum graingraph.ResultSummary) *Counters {
	st := sum.Statistics()
	return &Counters{
		ContainsUpdates:      st.ContainsUpdates(),
		NodesCreated:         st.NodesCreated,
		NodesDeleted:         st.NodesDeleted,
		PropertiesSet:        st.PropertiesSet,
		RelationshipsCreated: st.RelationshipsCreated,
		RelationshipsDeleted: st.RelationshipsDeleted,
		LabelsAdded:          st.LabelsAdded,
		LabelsRemoved:        st.LabelsRemoved,
		IndexesAdded:         st.IndexesAdded,
		IndexesRemoved:       st.IndexesRemoved,
		ConstraintsAdded:     st.ConstraintsAdded,
		ConstraintsRemoved:   st.ConstraintsRemoved,
	}
}

// Run executes statement in auto-commit mode and returns the
// materialised outcome.
//
// graingraph serialises on one connection, so a query would otherwise wait
// behind an open explicit transaction until it ends. It fails fast with
// a *TransactionOpenError instead.
func (s *Store) Run(ctx context.Context, statement string, params map[string]any) (*Outcome, error) {
	if err := s.checkNoOpenTx(ctx); err != nil {
		return nil, err
	}
	res, err := s.db.RunQuery(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return collect(ctx, res)
}

// collect drains a graingraph result into an Outcome.
func collect(ctx context.Context, res *graingraph.Result) (*Outcome, error) {
	recs, err := res.Collect(ctx)
	if err != nil {
		return nil, err
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		return nil, err
	}
	out := &Outcome{
		Fields: res.Keys(),
		Values: make([][]any, 0, len(recs)),
	}
	for _, r := range recs {
		out.Values = append(out.Values, r.Values())
	}
	if summary != nil {
		out.Counters = countersOf(summary)
	}
	return out, nil
}

// TransactionOpenError reports that an explicit transaction holds the
// database, so the requested operation cannot proceed.
type TransactionOpenError struct{ ID string }

func (e *TransactionOpenError) Error() string {
	return fmt.Sprintf("store: transaction %s is open", e.ID)
}

// checkNoOpenTx returns a *TransactionOpenError when an explicit transaction
// is still open after the store's wait (see WithTxWait).
func (s *Store) checkNoOpenTx(ctx context.Context) error {
	deadline := time.Now().Add(s.txWait)
	for {
		s.mu.Lock()
		open := ""
		for id := range s.txs {
			open = id
			break
		}
		s.mu.Unlock()
		if open == "" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return &TransactionOpenError{ID: open}
		}
		select {
		case <-ctx.Done():
			return &TransactionOpenError{ID: open}
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// NewBookmark returns a fresh opaque bookmark string.
func (s *Store) NewBookmark() string { return s.bookmarks.next() }

// Begin opens an explicit transaction. id must be unique among open
// transactions. timeout is the idle timeout (zero: no timeout).
func (s *Store) Begin(ctx context.Context, id string, timeout time.Duration, meta map[string]any) (*OpenTx, error) {
	// Reserve the right to begin: the check for an open transaction and the
	// registration of the new one must not be separated by other callers, or
	// every request released by a commit would pass the check together and queue
	// on the single connection.
	if err := s.reserveBegin(ctx); err != nil {
		return nil, err
	}
	release := func() {
		s.mu.Lock()
		s.beginning = false
		s.mu.Unlock()
	}
	// database/sql rolls a transaction back when the context given to
	// BeginTx is cancelled. The transaction must outlive the HTTP request
	// that opens it, so detach it from that request's cancellation, and stop
	// waiting for it ourselves when the request goes away.
	type begun struct {
		tx  *graingraph.Tx
		err error
	}
	ch := make(chan begun, 1)
	go func() {
		tx, err := s.db.BeginTx(context.WithoutCancel(ctx))
		ch <- begun{tx, err}
	}()
	var tx *graingraph.Tx
	select {
	case b := <-ch:
		if b.err != nil {
			release()
			return nil, b.err
		}
		tx = b.tx
	case <-ctx.Done():
		go func() { // whoever gets the connection later must not keep it
			if b := <-ch; b.err == nil {
				_ = b.tx.Rollback()
			}
			release()
		}()
		return nil, &TransactionOpenError{}
	}
	ot := &OpenTx{
		ID:      id,
		Meta:    meta,
		tx:      tx,
		timeout: timeout,
	}
	if timeout > 0 {
		ot.expiresAt = time.Now().Add(timeout)
	}
	s.mu.Lock()
	s.beginning = false
	if _, exists := s.txs[id]; exists {
		s.mu.Unlock()
		_ = tx.Rollback()
		return nil, fmt.Errorf("store: transaction ID %q already exists", id)
	}
	s.txs[id] = ot
	s.mu.Unlock()
	return ot, nil
}

// reserveBegin waits up to the store's tx wait for no transaction to be open
// or being opened, and marks one as being opened, atomically.
func (s *Store) reserveBegin(ctx context.Context) error {
	deadline := time.Now().Add(s.txWait)
	for {
		s.mu.Lock()
		open := ""
		for id := range s.txs {
			open = id
			break
		}
		if open == "" && !s.beginning {
			s.beginning = true
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		if !time.Now().Before(deadline) {
			return &TransactionOpenError{ID: open}
		}
		select {
		case <-ctx.Done():
			return &TransactionOpenError{ID: open}
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Tx looks up the open transaction with the given ID.
func (s *Store) Tx(id string) (*OpenTx, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txs[id]
	return t, ok
}

// Touch restarts the idle timeout of a transaction that has one. It is
// safe to call concurrently with the reaper and with running queries.
func (t *OpenTx) Touch() {
	if t.timeout <= 0 {
		return
	}
	t.timeMu.Lock()
	t.expiresAt = time.Now().Add(t.timeout)
	t.timeMu.Unlock()
}

// Run executes statement within the transaction. The transaction
// serialises its own queries, as graingraph cursors share one connection.
func (t *OpenTx) Run(ctx context.Context, statement string, params map[string]any) (*Outcome, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The idle clock starts when the query finishes, so a long-running
	// statement cannot leave the transaction about to expire.
	defer t.Touch()
	if t.tx == nil {
		return nil, ErrTransactionClosed
	}
	res, err := t.tx.Run(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return collect(ctx, res)
}

// Commit commits and removes the transaction, returning a bookmark.
// It returns an error if the transaction is unknown or already closed.
func (s *Store) Commit(id string) (string, error) {
	t, ok := s.Tx(id)
	if !ok {
		return "", ErrTransactionNotFound
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tx == nil {
		return "", ErrTransactionClosed
	}
	if err := t.tx.Commit(); err != nil {
		// A failed commit leaves the transaction in an undefined state
		// that the client cannot usefully retry: release it now rather
		// than leaving it registered until the reaper (if any) runs.
		_ = t.tx.Rollback()
		t.tx = nil
		s.mu.Lock()
		delete(s.txs, id)
		s.mu.Unlock()
		return "", err
	}
	t.tx = nil
	s.mu.Lock()
	delete(s.txs, id)
	s.mu.Unlock()
	return s.NewBookmark(), nil
}

// Rollback rolls back and removes the transaction.
func (s *Store) Rollback(id string) error {
	t, ok := s.Tx(id)
	if !ok {
		return ErrTransactionNotFound
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tx == nil {
		return ErrTransactionClosed
	}
	if err := t.tx.Rollback(); err != nil {
		return err
	}
	t.tx = nil
	s.mu.Lock()
	delete(s.txs, id)
	s.mu.Unlock()
	return nil
}

// reaper is the background goroutine that rolls back idle transactions.
type reaper struct {
	store *Store
	stop  chan struct{}
	done  chan struct{}
}

// StartReaper launches the idle-transaction reaper if timeout > 0. It
// returns a stop function; calling it blocks until the reaper exits.
func (s *Store) StartReaper(timeout time.Duration) func() {
	if timeout <= 0 {
		return func() {}
	}
	r := &reaper{store: s, stop: make(chan struct{}), done: make(chan struct{})}
	go r.run(timeout)
	return func() {
		close(r.stop)
		<-r.done
	}
}

func (r *reaper) run(timeout time.Duration) {
	defer close(r.done)
	interval := timeout / 10
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
			r.store.ReapIdle(timeout)
		}
	}
}

// ReapIdle rolls back every transaction idle for longer than timeout.
func (s *Store) ReapIdle(timeout time.Duration) int {
	now := time.Now()
	s.mu.Lock()
	var due []*OpenTx
	for _, t := range s.txs {
		if t.expired(now) {
			due = append(due, t)
		}
	}
	s.mu.Unlock()
	reaped := 0
	for _, t := range due {
		t.mu.Lock()
		// Re-check under t.mu: a query that was running when the
		// transaction was selected will have touched it on completion.
		if t.tx == nil || !t.expired(time.Now()) {
			t.mu.Unlock()
			continue
		}
		_ = t.tx.Rollback()
		t.tx = nil
		t.mu.Unlock()
		s.mu.Lock()
		delete(s.txs, t.ID)
		s.mu.Unlock()
		reaped++
	}
	return reaped
}

// ErrTransactionNotFound is returned for operations on an unknown
// transaction ID.
var ErrTransactionNotFound = errors.New("store: transaction not found")

// ErrTransactionClosed is returned for operations on an already
// committed or rolled back transaction.
var ErrTransactionClosed = errors.New("store: transaction already closed")

// txIDAlphabet omits characters that are easily confused (0/O, 1/l/I, U).
const txIDAlphabet = "23456789ABCDEFGHJKLMNPQRSTVWXYZabcdefghijkmnopqrstuvwxyz"

// txIDLength is long enough (~93 bits) that an ID is unguessable even
// before the per-user ownership check.
const txIDLength = 16

// NewTxID generates a random transaction ID. Each character is drawn
// uniformly from txIDAlphabet using crypto/rand.
func NewTxID() (string, error) {
	alphabetLen := big.NewInt(int64(len(txIDAlphabet)))
	id := make([]byte, txIDLength)
	for i := range id {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("store: generate transaction ID: %w", err)
		}
		id[i] = txIDAlphabet[n.Int64()]
	}
	return string(id), nil
}
