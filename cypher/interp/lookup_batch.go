package interp

import (
	"sync/atomic"

	"github.com/graingraph/graingraph/cypher/syntax"
)

// lookupBatchDisabled turns off the batched UNWIND…MATCH start-node lookup so a
// query falls back to one json_extract = ? scan per input row; tests use it to
// compare both paths on the same data.
var lookupBatchDisabled bool

// batchedLookups counts MATCH clauses that ran through the batched json_each
// path (for tests, asserting the optimisation actually fired).
var batchedLookups atomic.Int64

// tryExecMatchBatched collapses one start-node scan per input row into a single
// IN-over-json_each query when the clause is exactly the bulk-lookup shape: a
// single-node MATCH whose inline property map names one pushable key, with at
// least two distinct string values across the input rows (the common
// UNWIND…MATCH (n:X {key: k}) case). It leaves st.rows untouched and reports
// handled=false for every other shape — a non-string value, a relationship or
// path var, multiple property keys, a single distinct value — so the per-row scan
// runs unchanged and no type-coercion or multi-part path is affected.
//
// On handle it sets st.rows to the same rows the per-row loop would emit, in the
// same order: input rows are visited in slice order, each row's matching nodes in
// id order (the batched scan is ORDER BY n.id and every node matches exactly one
// value), repeated keys re-emit their whole bucket (multiplicity), and an
// unmatched row with an OPTIONAL MATCH still gets its null-padded clone.
func (ex *exec) tryExecMatchBatched(cl *syntax.Match, vars []string, st *qstate) (bool, error) {
	if lookupBatchDisabled || len(st.rows) < 2 {
		return false, nil
	}
	if len(cl.Patterns) != 1 {
		return false, nil
	}
	part := cl.Patterns[0]
	// Only the plainest shape: one node, no selector/func/path var, a single-
	// entry inline property map with a pushable key.
	if part.Selector != nil || part.Func != syntax.FuncNone || part.Var != "" {
		return false, nil
	}
	if len(part.Elems) != 1 {
		return false, nil
	}
	np, ok := part.Elems[0].(*syntax.NodePattern)
	if !ok {
		return false, nil
	}
	m, ok := np.Props.(*syntax.MapLit)
	if m == nil || len(m.Entries) != 1 {
		return false, nil
	}
	en := m.Entries[0]
	if !pushableKey(en.Key) {
		return false, nil
	}

	// Every input row must yield a string key; a missing or non-string value
	// falls the whole clause back to per-row so no type-coercion path changes.
	keyText := make([]string, len(st.rows))
	distinct := make([]string, 0)
	seen := map[string]struct{}{}
	for i, r := range st.rows {
		v, ok := ex.hintValue(en.Value, r)
		if !ok || v == nil {
			return false, nil
		}
		s, ok := v.(string)
		if !ok {
			return false, nil
		}
		keyText[i] = s
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			distinct = append(distinct, s)
		}
	}
	if len(distinct) < 2 {
		return false, nil // one distinct value: a single scan is no faster than per-row
	}

	buckets, err := ex.g.scanNodesByStringKey(scanLabel(np.Labels), en.Key, distinct)
	if err != nil {
		return true, err
	}
	batchedLookups.Add(1)

	out := make([]row, 0, len(st.rows))
	for i, r := range st.rows {
		matched := false
		for _, n := range buckets[keyText[i]] {
			ok, err := ex.nodeMatches(np, n, r)
			if err != nil {
				st.rows = out
				return true, err
			}
			if !ok {
				continue
			}
			r2 := r
			if np.Var != "" {
				r2 = r.with(np.Var, n)
			}
			// The two WHERE gates run after the node binds, just as forEachStart's
			// visit (node-level WHERE) and the per-row emit (MATCH-level WHERE) do.
			if np.Where != nil {
				c, err := ex.eval(np.Where, r2)
				if err != nil {
					st.rows = out
					return true, err
				}
				if c != true {
					continue
				}
			}
			if cl.Where != nil {
				c, err := ex.eval(cl.Where, r2)
				if err != nil {
					st.rows = out
					return true, err
				}
				if c != true {
					continue
				}
			}
			matched = true
			out = append(out, r2)
		}
		if !matched && cl.Optional {
			nr := r.clone()
			for _, v := range vars {
				if _, bound := nr[v]; !bound {
					nr[v] = nil
				}
			}
			out = append(out, nr)
		}
	}
	st.rows = out
	return true, nil
}
