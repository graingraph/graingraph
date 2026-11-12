package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/graingraph/graingraph/store"
)

func TestPageCacheIsApplied(t *testing.T) {
	// 5 MiB applied as a negative kibibyte count (page-size independent).
	s, err := store.Open(filepath.Join(t.TempDir(), "pc.db"), store.Config{CacheSize: 5 << 20})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var kb int64
	s.DB().QueryRow("PRAGMA cache_size;").Scan(&kb)
	if kb != -5120 {
		t.Errorf("cache_size = %d kibibytes, want -5120 (5 MiB)", kb)
	}
}

func TestPageCacheZeroLeavesDefault(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "def.db"), store.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	var kb int64
	s.DB().QueryRow("PRAGMA cache_size;").Scan(&kb)
	// The default is a negative kibibyte count; it must not have been forced to 0.
	if kb >= 0 {
		t.Errorf("cache_size = %d, want the negative-bytes default (left at SQLite's default)", kb)
	}
}

func TestMMapIsAppliedOnWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mm.db")
	s, err := store.Open(path, store.Config{MMapSize: 1 << 30})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Give the file some pages so the effective mmap size is non-zero.
	for i := 0; i < 256; i++ {
		if _, err := s.DB().Exec("INSERT INTO nodes (labels, props) VALUES ('N', json(1))"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var size int64
	s.DB().QueryRow("PRAGMA mmap_size;").Scan(&size)
	if size <= 0 {
		t.Errorf("mmap_size = %d, want a positive effective size after memory-mapped I/O is requested", size)
	}
}

func TestMMapDisableDoesNotError(t *testing.T) {
	// A negative request disables mmap; opening must still succeed.
	s, err := store.Open(filepath.Join(t.TempDir(), "mmoff.db"), store.Config{MMapSize: -1})
	if err != nil {
		t.Fatalf("open with disabled mmap: %v", err)
	}
	defer func() { _ = s.Close() }()
}

func TestSynchronousIsApplied(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "syn.db"), store.Config{Synchronous: 1}) // NORMAL
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var v int64
	s.DB().QueryRow("PRAGMA synchronous;").Scan(&v)
	if v != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", v)
	}
}

func TestSynchronousUnsetIsSane(t *testing.T) {
	// Leaving synchronous unset applies no pragma; the value SQLite reports is a
	// matter of platform (on macOS the default is 0, on some others 2). We only
	// require that opening succeeds and the pragma reads as a valid level.
	s, err := store.Open(filepath.Join(t.TempDir(), "synfull.db"), store.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var v int64
	s.DB().QueryRow("PRAGMA synchronous;").Scan(&v)
	if v < 0 || v > 2 {
		t.Errorf("synchronous = %d, want a valid level in [0,2]", v)
	}
}

func TestPageSizeIsAppliedOnNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ps.db")
	s1, err := store.Open(path, store.Config{PageSize: 8192})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Write, then reopen: the size is fixed in the file header and must persist.
	if _, err := s1.DB().Exec("INSERT INTO nodes (labels, props) VALUES ('N', json(1))"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := store.Open(path, store.Config{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var size int
	s2.DB().QueryRow("PRAGMA page_size;").Scan(&size)
	if size != 8192 {
		t.Errorf("page_size = %d after reopen, want 8192", size)
	}
}

func TestPageSizeIgnoredOnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "psexist.db")
	// First open at the default (4 KiB), then request a different size on reuse.
	s1, err := store.Open(path, store.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s1.DB().Exec("INSERT INTO nodes (labels, props) VALUES ('N', json(1))"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := store.Open(path, store.Config{PageSize: 8192})
	if err != nil {
		t.Fatalf("reopen with page size on an existing file must be a no-op, not an error: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var size int
	s2.DB().QueryRow("PRAGMA page_size;").Scan(&size)
	if size == 8192 {
		t.Errorf("page_size changed to %d on an existing file; it should have stayed 4096", size)
	}
}

func TestPageSizeRejected(t *testing.T) {
	// A positive-but-invalid page size must fail: below the 512-byte floor, not a
	// power of two, or above SQLite's 32768-byte ceiling.
	for _, ps := range []int{256, 1000, 1 << 16} {
		ps := ps
		t.Run("ps", func(t *testing.T) {
			_, err := store.Open(filepath.Join(t.TempDir(), "bad.db"), store.Config{PageSize: ps})
			if err == nil {
				t.Fatalf("page size %d: expected an error", ps)
			}
		})
	}
	// A non-positive page size is a documented no-op (leave SQLite's default), so
	// it must be ignored rather than rejected. On Windows an un-closed store keeps
	// its temp file handle locked and t.TempDir cleanup fails, so close it.
	{
		s, err := store.Open(filepath.Join(t.TempDir(), "nop.db"), store.Config{PageSize: -4096})
		if err != nil {
			t.Fatalf("non-positive page size should be ignored, not error: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}
}

// TestTuningReachesReadPool proves the per-connection tuning is threaded into the
// read pool's DSN, not only set on the writer. A read-pool transaction can run a
// system pragma, so we read it straight off one of those connections.
func TestTuningReachesReadPool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.db")
	s, err := store.Open(path, store.Config{CacheSize: 4 << 20, MMapSize: 1 << 30, ReadConns: 3})
	if err != nil {
		t.Fatalf("open (a malformed DSN pragma would fail here at Ping): %v", err)
	}
	defer func() { _ = s.Close() }()
	if !s.HasReadPool() {
		t.Fatal("expected a read pool with ReadConns > 0")
	}

	ctx := context.Background()
	tx, err := s.BeginReadTx(ctx)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, "PRAGMA cache_size")
	if err != nil {
		t.Fatalf("query pool connection: %v", err)
	}
	var kb int64
	for rows.Next() {
		if err := rows.Scan(&kb); err != nil {
			t.Errorf("scan: %v", err)
		}
	}
	_ = rows.Close()
	if kb != -4096 { // 4 MiB in kibibytes
		t.Errorf("pool connection cache_size = %d, want -4096 (4 MiB)", kb)
	}
}

func TestTuningInMemoryDoesNotError(t *testing.T) {
	// Per-connection settings must be harmless on an in-memory database.
	s, err := store.Open(":memory:", store.Config{CacheSize: 1 << 20, MMapSize: 1 << 20, Synchronous: 1})
	if err != nil {
		t.Fatalf("open in-memory with tuning: %v", err)
	}
	defer func() { _ = s.Close() }()
}
