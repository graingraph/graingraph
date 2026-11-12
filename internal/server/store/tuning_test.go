package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/graingraph/graingraph"
)

// TestNewWithTuningOptions verifies the SQLite tuning options forward through
// store.New to graingraph.Open without error, and that a query still runs
// against the tuned database.
func TestNewWithTuningOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tune.db")
	st, err := New(path,
		WithPageCache(4<<20),
		WithMMapSize(1<<30),
		WithSyncMode(graingraph.SyncNormal),
		WithPageSize(4096),
	)
	if err != nil {
		t.Fatalf("New with tuning options: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.Run(context.Background(), "CREATE (n:A {k: 1})", nil); err != nil {
		t.Fatalf("query in tuned store: %v", err)
	}
}
