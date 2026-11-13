package interp

import (
	"testing"

	"github.com/graingraph/graingraph/store"
)

// TestBatchedLookupAgreesWithRowCount checks that the batched UNWIND…MATCH
// start-node lookup produces exactly the same rows, counters and final graph as
// the per-row json_extract = ? scan, across multiplicity (a repeated key re-emits
// its whole node set), optional-match null-padding (one clone per unmatched input
// row, not per missing key), a non-string key fallback, and the final-graph
// invariants. It mirrors TestSetWriteBehindAgreesWithRowAtATime.
func TestBatchedLookupAgreesWithRowAtATime(t *testing.T) {
	// default it after so a failure cannot leak the toggle into a later test
	defer func() { lookupBatchDisabled = false }()

	script := []string{
		`UNWIND range(1, 50) AS i CREATE (:K {s: 'k'+i, i: i})`,
		// Bulk UNWIND-driven lookup forced out of the pipeline by a trailing SET so
		// it runs clause-by-clause. The repeated key i=2 (node k2) must appear
		// twice; order of results is input-row order then node id.
		`UNWIND [1, 2, 3, 2, 4] AS i MATCH (n:K {s: 'k'+i}) SET n.touched = true RETURN n.i ORDER BY n.i`,
		// Many distinct string keys with an updating clause, so the batched path
		// fires over all of them at once.
		`UNWIND range(1, 40) AS i MATCH (n:K {s: 'k'+i}) CREATE (n)-[:Seen]->(:Seen {v: i}) RETURN count(*)`,
		// A non-string key value falls back to the per-row scan unchanged (no
		// type-coercion widening); also a single distinct value (a batch of one is
		// no faster than the per-row path).
		`UNWIND range(1, 30) AS i MATCH (n:K {i: i}) RETURN n.s ORDER BY n.i`,
		`UNWIND [5] AS i MATCH (n:K {s: 'k'+i}) RETURN n.i`,
		// Optional match: a missing key (zzz) still passes a null-padded row through,
		// one per unmatched input row. The forced-updating CREATE keeps it in the
		// clause-at-a-time executor so the batched OPTIONAL match path is exercised.
		`UNWIND ['k1', 'zzz', 'k3'] AS s MATCH (n:Z) OPTIONAL MATCH (m:K {s: s}) CREATE (:Hit {s: s}) RETURN s ORDER BY s`,
		// Final-graph invariants must match exactly.
		`MATCH (n:K) RETURN id(n) AS id, n.i AS i, n.s AS s, n.touched AS t ORDER BY id LIMIT 60`,
		`MATCH ()-[r]->() RETURN type(r) AS t ORDER BY 1`,
	}

	setup := []string{`CREATE (:Z)`}

	run := func(disabled bool) []string {
		lookupBatchDisabled = disabled
		db, err := store.Open(":memory:", store.Config{})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var out []string
		for _, q := range setup {
			out = append(out, q+" => "+runWithEffects(t, db, q))
		}
		for _, q := range script {
			out = append(out, q+" => "+runWithEffects(t, db, q))
		}
		return out
	}

	before := batchedLookups.Load()
	g0 := batchedLookups.Load()
	want := run(true) // per-row: the oracle
	if batchedLookups.Load() != g0 {
		t.Fatal("with lookup-batch off, a batched lookup leaked in")
	}
	got := run(false) // batched on
	if len(got) != len(want) {
		t.Fatalf("%d != %d results", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("per-row:\n  %s\nbatched:\n  %s", want[i], got[i])
		}
	}
	// The batch must actually have fired on some clause of the batched run.
	if delta := batchedLookups.Load() - before; delta == 0 {
		t.Fatal("no lookup was batched across the script; the optimisation did not fire")
	}
}
