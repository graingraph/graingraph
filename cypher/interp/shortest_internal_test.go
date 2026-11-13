package interp

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/graingraph/graingraph/cypher/syntax"
	"github.com/graingraph/graingraph/store"
)

func pathKey(p *Path) string {
	ids := make([]string, len(p.Rels))
	for i, r := range p.Rels {
		ids[i] = fmt.Sprint(r.ID)
	}
	return fmt.Sprintf("%d>%d:%s", p.Nodes[0].ID, p.Nodes[len(p.Nodes)-1].ID, strings.Join(ids, ","))
}

func pathKeys(ps []*Path) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = pathKey(p)
	}
	sort.Strings(out)
	return out
}

// randomGraph builds a small random multigraph (parallel relationships,
// self-loops, two relationship types) in a fresh in-memory database.
func randomGraph(t *testing.T, seed int64, nodes, rels int) *store.SQLiteStore {
	t.Helper()
	db, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := rand.New(rand.NewSource(seed))
	runInternal(t, db, fmt.Sprintf("UNWIND range(1, %d) AS i CREATE (:N {i: i})", nodes), nil)
	for k := 0; k < rels; k++ {
		runInternal(t, db, fmt.Sprintf("MATCH (a:N {i: %d}), (b:N {i: %d}) CREATE (a)-[:%s]->(b)",
			r.Intn(nodes)+1, r.Intn(nodes)+1, []string{"R", "S"}[r.Intn(2)]), nil)
	}
	return db
}

func TestShortestPathStrategiesAgree(t *testing.T) {
	ctx := context.Background()
	skipped, compared := 0, 0
	for seed := int64(1); seed <= 6; seed++ {
		db := randomGraph(t, seed, 18, 36)
		g := newGraph(ctx, db.DB())
		ex := &exec{g: g}
		nodes, err := g.scanNodes("N", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []syntax.Direction{syntax.DirRight, syntax.DirLeft, syntax.DirNone} {
			for _, typ := range []string{"", "R"} {
				for _, max := range []int64{-1, 3} {
					rp := &syntax.RelPattern{Dir: dir}
					if typ != "" {
						rp.Types = &syntax.LabelName{Name: typ}
					}
					for i := 0; i < 60; i++ {
						s, e := nodes[rand.Intn(len(nodes))], nodes[rand.Intn(len(nodes))]
						label := fmt.Sprintf("seed %d dir %d type %q max %d %d->%d", seed, dir, typ, max, s.ID, e.ID)
						used := map[int64]bool{}

						slowAll, err := ex.shortestPathsSlow(rp, s, e, 1, max, true, row{}, used)
						if err != nil {
							skipped++
							continue // the old algorithm's state cap: no oracle for this pair
						}
						compared++
						bfsAll, err := ex.shortestPaths(rp, s, e, 1, max, true, row{}, used)
						if err != nil {
							t.Fatal(err)
						}
						if strings.Join(pathKeys(slowAll), ";") != strings.Join(pathKeys(bfsAll), ";") {
							t.Fatalf("%s: all shortest differ\n slow %v\n bfs  %v", label, pathKeys(slowAll), pathKeys(bfsAll))
						}

						slowOne, _ := ex.shortestPathsSlow(rp, s, e, 1, max, false, row{}, used)
						one, err := ex.shortestPaths(rp, s, e, 1, max, false, row{}, used) // bidirectional
						if err != nil {
							t.Fatal(err)
						}
						if len(slowOne) != len(one) {
							t.Fatalf("%s: one search found %d paths, the other %d", label, len(slowOne), len(one))
						}
						if len(one) == 1 {
							if len(one[0].Rels) != len(slowOne[0].Rels) {
								t.Fatalf("%s: lengths %d vs %d", label, len(one[0].Rels), len(slowOne[0].Rels))
							}
							// The path found must be a real path of that length, found among all shortest ones.
							found := false
							for _, k := range pathKeys(slowAll) {
								if k == pathKey(one[0]) {
									found = true
								}
							}
							if !found && s.ID != e.ID {
								t.Fatalf("%s: %s is not one of the shortest paths %v", label, pathKey(one[0]), pathKeys(slowAll))
							}
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d pairs, skipped %d", compared, skipped)
	if compared < 5*skipped {
		t.Fatalf("the oracle skipped too many pairs: compared %d, skipped %d", compared, skipped)
	}
}

func TestBidirectionalSearchRespectsUsedRelationships(t *testing.T) {
	ctx := context.Background()
	db := randomGraph(t, 9, 12, 30)
	g := newGraph(ctx, db.DB())
	ex := &exec{g: g}
	nodes, _ := g.scanNodes("N", nil)
	rp := &syntax.RelPattern{Dir: syntax.DirNone}
	s, e := nodes[0], nodes[len(nodes)-1]
	free, _ := ex.shortestPaths(rp, s, e, 1, -1, false, row{}, map[int64]bool{})
	if len(free) == 0 {
		t.Skip("no path in this graph")
	}
	// Forbid every relationship of that path: the next path (if any) avoids them all.
	used := map[int64]bool{}
	for _, rel := range free[0].Rels {
		used[rel.ID] = true
	}
	for _, strategy := range []bool{false, true} {
		shortestBidirectionalDisabled = strategy
		got, err := ex.shortestPaths(rp, s, e, 1, -1, false, row{}, used)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range got {
			for _, rel := range p.Rels {
				if used[rel.ID] {
					t.Errorf("a relationship already used by the MATCH was reused (bidirectional disabled = %v)", strategy)
				}
			}
		}
	}
	shortestBidirectionalDisabled = false
}

func TestSelectedAnyStopsEarlyAndAgrees(t *testing.T) {
	db := randomGraph(t, 3, 12, 30)
	const q = "MATCH (a:N {i: 1}), (b:N {i: 7}), p = ANY 2 (a)-[:R|S]->{1,4}(b) RETURN length(p) AS l"
	early := runInternal(t, db, q, nil)
	selectedEarlyStopDisabled = true
	full := runInternal(t, db, q, nil)
	selectedEarlyStopDisabled = false
	if len(early) != len(full) {
		t.Fatalf("early stop returned %d rows, full enumeration %d", len(early), len(full))
	}
}

func TestSelectedMatchCap(t *testing.T) {
	db := randomGraph(t, 4, 12, 40)
	old := maxSelectedMatches
	maxSelectedMatches = 50
	defer func() { maxSelectedMatches = old }()
	st, err := syntax.Parse("MATCH p = ALL (a:N)-[:R|S]->{1,6}(b:N) RETURN count(p)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, _ := db.BeginExecTx(ctx)
	defer tx.Rollback()
	if _, err := RunWith(ctx, tx, st, nil, nil); err == nil || !strings.Contains(err.Error(), "more than 50 matches") {
		t.Fatalf("want a match-cap error, got %v", err)
	}
}

func TestSelectedIterativeDeepeningAgrees(t *testing.T) {
	patterns := []string{
		"(a)-[:R|S]->{1,6}(b)",
		"(a)-[:R|S*1..3]-(m)-[:R|S*1..3]-(b)",
		"(a)-[:R]->(m)-[:S|R*1..4]->(b)",
	}
	before := selectedDeepenRuns.Load()
	selectors := []string{"SHORTEST 1", "SHORTEST 3", "SHORTEST 2 GROUPS", "SHORTEST 1 GROUP", "ALL SHORTEST", "ANY SHORTEST"}
	for seed := int64(1); seed <= 4; seed++ {
		db := randomGraph(t, seed, 10, 24)
		for _, pat := range patterns {
			for _, sel := range selectors {
				for _, ends := range [][2]int{{1, 5}, {2, 9}, {3, 3}} {
					q := fmt.Sprintf("MATCH (a:N {i: %d}), (b:N {i: %d}), p = %s %s RETURN length(p) AS l", ends[0], ends[1], sel, pat)
					deepened := runInternal(t, db, q, nil)
					selectedEarlyStopDisabled = true
					full := runInternal(t, db, q, nil)
					selectedEarlyStopDisabled = false
					if strings.Join(deepened, ",") != strings.Join(full, ",") {
						t.Fatalf("seed %d %q\n deepened %v\n full     %v", seed, q, deepened, full)
					}
				}
			}
		}
	}
	if selectedDeepenRuns.Load() == before {
		t.Fatal("iterative deepening never ran")
	}
}

// skipHeavyTiming skips a wall-clock/perf-ratio assertion when
// GRAINGRAPH_SKIP_HEAVY is set to a non-zero, non-empty value. Such comparisons are
// meaningless under the -race detector (instrumentation inflates both sides of any
// ratio), and this test's full path enumeration over a dense graph pushes the interp
// package's -race binary past CI's 5-minute timeout on slower runners; CI sets the var
// to 1. Local and non-race runs leave it unset, so the ratio is still exercised there.
func skipHeavyTiming(t *testing.T) {
	if v := os.Getenv("GRAINGRAPH_SKIP_HEAVY"); v != "" && v != "0" {
		t.Skip("heavy timing assertion skipped (GRAINGRAPH_SKIP_HEAVY=", v, ")")
	}
}

func TestSelectedDeepeningIsFasterOnDenseGraphs(t *testing.T) {
	skipHeavyTiming(t)
	db := randomGraph(t, 5, 14, 90)
	const q = "MATCH (a:N {i: 1}), (b:N {i: 9}), p = SHORTEST 1 (a)-[:R|S]->{1,9}(b) RETURN length(p) AS l"
	start := time.Now()
	fast := runInternal(t, db, q, nil)
	fastT := time.Since(start)
	selectedEarlyStopDisabled = true
	start = time.Now()
	slow := runInternal(t, db, q, nil)
	slowT := time.Since(start)
	selectedEarlyStopDisabled = false
	t.Logf("deepening %v, full enumeration %v", fastT, slowT)
	if strings.Join(fast, ",") != strings.Join(slow, ",") {
		t.Fatalf("%v vs %v", fast, slow)
	}
	if fastT*3 > slowT {
		t.Errorf("deepening (%v) is not clearly faster than enumerating everything (%v)", fastT, slowT)
	}
}
