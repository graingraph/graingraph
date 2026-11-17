package interp

import (
	"errors"
	"math"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/graingraph/graingraph/cypher/syntax"
)

// A streaming executor for read-only queries. Instead of materialising the
// whole result of every clause as a []row before the next clause starts, the
// clauses form a chain of stages: each row a clause produces is pushed to the
// next stage at once. A MATCH feeds a WHERE, a projection and a LIMIT without
// any of them holding more than the row in flight, and a LIMIT that has had
// enough stops the matcher that feeds it.
//
// Stages that need every row (an aggregation, ORDER BY, DISTINCT) buffer, as
// before, but an aggregation keeps one accumulator set per group rather than
// every row of the group, so its memory follows the number of groups, not the
// number of input rows. At the end of the input they finish the projection with
// the same code the clause-at-a-time executor uses (finishProjectionRows).
//
// Only queries built from MATCH, UNWIND, WITH, RETURN, FILTER, LET and FINISH
// stream: an updating clause must see all its input before it writes (a MATCH
// that kept scanning would see the nodes a CREATE had just added), so any query
// with another clause runs clause by clause.

// streamDisabled turns streaming off; tests use it to compare both executors on
// the same data.
var streamDisabled bool

// topNDisabled keeps every row of an ORDER BY ... LIMIT instead of the best few;
// tests use it to compare the two.
var topNDisabled bool

// topNUsed counts ORDER BY ... LIMIT stages that kept only the best rows (for tests).
var topNUsed atomic.Int64

// streamUsed counts queries run through the pipeline (for tests).
var streamUsed atomic.Int64

// errPipelineDone ends a pipeline early: a LIMIT has all the rows it wants. It is not
// an error to the caller.
var errPipelineDone = errors.New("pipeline satisfied")

// stage is one step of the pipeline. push consumes a row and passes results to
// the next stage; finish is called once the input has ended, flushes anything
// the stage buffered, and finishes the next stage.
type stage struct {
	push   func(r row) error
	finish func() error
}

// streamableAggregates are the aggregate functions with an incremental form.
var streamableAggregates = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true, "collect": true,
	"percentilecont": true, "percentiledisc": true, "stdev": true, "stdevp": true,
}

// streamable reports whether every clause can run in the pipeline.
func streamable(clauses []syntax.Clause) bool {
	for _, cl := range clauses {
		switch cl := cl.(type) {
		case *syntax.Match, *syntax.Unwind, *syntax.Filter, *syntax.Let, *syntax.Finish:
		case *syntax.With:
			if !streamableProjection(&cl.Projection) {
				return false
			}
		case *syntax.Return:
			if !streamableProjection(&cl.Projection) {
				return false
			}
		default:
			return false
		}
	}
	return len(clauses) > 0
}

func streamableProjection(p *syntax.Projection) bool {
	for _, it := range p.Items {
		for _, f := range aggregateCalls(it.Expr) {
			if !streamableAggregates[strings.ToLower(f.Name)] || (!f.Star && len(f.Args) == 0) {
				return false
			}
		}
	}
	return true
}

// runStream runs clauses (a streamable run) over st.rows through the pipeline
// and leaves the result in st. It returns the output columns, as the
// clause-at-a-time loop does.
func (ex *exec) runStream(clauses []syntax.Clause, st *qstate, cols []string) ([]string, error) {
	streamUsed.Add(1)
	ex.streaming++
	defer func() { ex.streaming-- }()

	// The variables in scope before each projection (front to back; only a
	// projection with * reads them), its output names, and the columns the
	// query ends with.
	scope := &qstate{vars: st.vars}
	scopes := make([]*qstate, len(clauses))
	for i, cl := range clauses {
		switch cl := cl.(type) {
		case *syntax.Match:
			scope.declare(patternVars(cl.Patterns)...)
		case *syntax.Unwind:
			scope.declare(cl.Var)
		case *syntax.Let:
			for _, it := range cl.Items {
				scope.declare(it.Var)
			}
		case *syntax.With:
			scopes[i] = &qstate{vars: scope.vars}
			names, _ := ex.projectionNames(&cl.Projection, scopes[i])
			scope = &qstate{vars: names}
		case *syntax.Return:
			scopes[i] = &qstate{vars: scope.vars}
			names, _ := ex.projectionNames(&cl.Projection, scopes[i])
			scope = &qstate{vars: names}
			cols = names
		case *syntax.Finish:
			cols = nil
		}
	}

	var out []row
	next := &stage{
		push:   func(r row) error { out = append(out, r); return nil },
		finish: func() error { return nil },
	}
	for i := len(clauses) - 1; i >= 0; i-- {
		switch cl := clauses[i].(type) {
		case *syntax.Match:
			next = ex.matchStage(cl, next)
		case *syntax.Unwind:
			next = ex.unwindStage(cl, next)
		case *syntax.Filter:
			next = ex.filterStage(cl, next)
		case *syntax.Let:
			next = ex.letStage(cl, next)
		case *syntax.Finish:
			next = &stage{push: func(row) error { return nil }, finish: next.finish}
		case *syntax.With:
			next = ex.projectionStage(&cl.Projection, cl.Where, scopes[i], next)
		case *syntax.Return:
			next = ex.projectionStage(&cl.Projection, nil, scopes[i], next)
		}
	}

	for _, r := range st.rows {
		if err := next.push(r); err != nil {
			if errors.Is(err, errPipelineDone) {
				break
			}
			return nil, err
		}
	}
	if err := next.finish(); err != nil && !errors.Is(err, errPipelineDone) {
		return nil, err
	}
	st.rows = out
	st.vars = scope.vars
	return cols, nil
}

// flush pushes buffered rows to next, stopping early when next is satisfied,
// and then finishes next.
func flush(rows []row, next *stage) error {
	for _, r := range rows {
		if err := next.push(r); err != nil {
			if errors.Is(err, errPipelineDone) {
				break
			}
			return err
		}
	}
	return next.finish()
}

func (ex *exec) matchStage(cl *syntax.Match, next *stage) *stage {
	vars := patternVars(cl.Patterns)
	mine := whereEqualities(cl.Where)
	return &stage{
		push: func(r row) error {
			// The equalities narrow this MATCH's scans only: they must not be
			// visible to the stages downstream, which run inside the matcher's
			// callback, so they are swapped out around each push.
			base := ex.pushed
			ex.pushed = mine
			matched := false
			err := ex.matchParts(cl.Patterns, r, func(r2 row) error {
				if cl.Where != nil {
					c, err := ex.eval(cl.Where, r2)
					if err != nil {
						return err
					}
					if c != true {
						return nil
					}
				}
				matched = true
				ex.pushed = base
				err := next.push(r2)
				ex.pushed = mine
				return err
			})
			ex.pushed = base
			if err != nil {
				return err
			}
			if !matched && cl.Optional {
				nr := r.clone()
				for _, v := range vars {
					if _, bound := nr[v]; !bound {
						nr[v] = nil
					}
				}
				return next.push(nr)
			}
			return nil
		},
		finish: next.finish,
	}
}

// lazyRange recognises `range(a, b[, c])` so UNWIND can walk it without
// building the list. ok is false for any other expression.
func (ex *exec) lazyRange(e syntax.Expr, r row) (start, end, step int64, ok bool, err error) {
	f, isCall := e.(*syntax.FuncCall)
	if !isCall || len(f.Namespace) != 0 || !strings.EqualFold(f.Name, "range") || f.Distinct || f.Star || len(f.Args) < 2 || len(f.Args) > 3 {
		return 0, 0, 0, false, nil
	}
	args := make([]any, len(f.Args))
	for i, a := range f.Args {
		if args[i], err = ex.eval(a, r); err != nil {
			return 0, 0, 0, false, err
		}
	}
	start, end, step, null, err := rangeSpec(args)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if null {
		return 1, 0, 1, true, nil // end < start with step 1: no rows
	}
	return start, end, step, true, nil
}

func (ex *exec) unwindStage(cl *syntax.Unwind, next *stage) *stage {
	return &stage{
		push: func(r row) error {
			if start, end, step, ok, err := ex.lazyRange(cl.Expr, r); err != nil {
				return err
			} else if ok {
				n := rangeLen(start, end, step)
				for i, k := start, uint64(0); k < n; i, k = i+step, k+1 {
					if k&1023 == 0 {
						if err := ex.g.ctx.Err(); err != nil {
							return err
						}
					}
					if err := next.push(r.with(cl.Var, i)); err != nil {
						return err
					}
				}
				return nil
			}
			v, err := ex.eval(cl.Expr, r)
			if err != nil {
				return err
			}
			switch x := v.(type) {
			case nil:
			case []any:
				for _, el := range x {
					if err := next.push(r.with(cl.Var, el)); err != nil {
						return err
					}
				}
			default:
				return next.push(r.with(cl.Var, x))
			}
			return nil
		},
		finish: next.finish,
	}
}

func (ex *exec) filterStage(cl *syntax.Filter, next *stage) *stage {
	return &stage{
		push: func(r row) error {
			c, err := ex.eval(cl.Cond, r)
			if err != nil {
				return err
			}
			if c == true {
				return next.push(r)
			}
			return nil
		},
		finish: next.finish,
	}
}

func (ex *exec) letStage(cl *syntax.Let, next *stage) *stage {
	return &stage{
		push: func(r row) error {
			for _, it := range cl.Items {
				v, err := ex.eval(it.Expr, r)
				if err != nil {
					return err
				}
				r = r.with(it.Var, v)
			}
			return next.push(r)
		},
		finish: next.finish,
	}
}

// projectionStage is a WITH or RETURN. Without aggregation, ORDER BY or
// DISTINCT it streams; otherwise it buffers (aggregation: one accumulator set
// per group) and finishes with the shared projection code.
func (ex *exec) projectionStage(p *syntax.Projection, where syntax.Expr, scope *qstate, next *stage) *stage {
	items := projItems(p, scope)
	names := make([]string, len(items))
	aggItem := make([]bool, len(items))
	aggregating := false
	for i, it := range items {
		names[i] = it.name
		aggItem[i] = len(aggregateCalls(it.expr)) > 0
		aggregating = aggregating || aggItem[i]
	}
	switch {
	case aggregating:
		return ex.aggregateStage(p, where, items, names, aggItem, next)
	case len(p.Order) > 0 || p.Distinct:
		return ex.bufferStage(p, where, items, names, next)
	}
	return ex.mapStage(p, where, items, next)
}

// project evaluates the items of a non-aggregating projection for one row.
func (ex *exec) project(items []projItem, r row) (row, error) {
	nr := make(row, len(items))
	for _, it := range items {
		v, err := ex.eval(it.expr, r)
		if err != nil {
			return nil, err
		}
		nr[it.name] = v
	}
	return nr, nil
}

// mapStage streams a projection with optional SKIP, LIMIT and WHERE, applied in
// that order (as finishProjectionRows does).
func (ex *exec) mapStage(p *syntax.Projection, where syntax.Expr, items []projItem, next *stage) *stage {
	var (
		inited      bool
		skip, limit int64
		seen        int64
	)
	// SKIP and LIMIT are evaluated when the first row arrives, or at the end of
	// an empty input, so their errors surface as they did before.
	init := func() error {
		if inited {
			return nil
		}
		inited = true
		var err error
		if skip, err = ex.nonNegInt(p.Skip, "SKIP"); err != nil {
			return err
		}
		if skip < 0 {
			skip = 0
		}
		limit, err = ex.nonNegInt(p.Limit, "LIMIT")
		return err
	}
	return &stage{
		push: func(r row) error {
			if err := init(); err != nil {
				return err
			}
			if limit == 0 {
				return errPipelineDone
			}
			nr, err := ex.project(items, r)
			if err != nil {
				return err
			}
			seen++
			if seen <= skip {
				return nil
			}
			pos := seen - skip // 1-based position among the rows that pass SKIP
			if where != nil {
				scopeRow := r.clone()
				for k, v := range nr {
					scopeRow[k] = v
				}
				c, err := ex.eval(where, scopeRow)
				if err != nil {
					return err
				}
				if c != true {
					if limit >= 0 && pos >= limit {
						return errPipelineDone
					}
					return nil
				}
			}
			if err := next.push(nr); err != nil {
				return err
			}
			if limit >= 0 && pos >= limit {
				return errPipelineDone
			}
			return nil
		},
		finish: func() error {
			if err := init(); err != nil {
				return err
			}
			return next.finish()
		},
	}
}

// bufferStage holds the projected rows of a non-aggregating projection that
// needs all of them (ORDER BY, DISTINCT). With DISTINCT only the first row of
// each distinct value is kept, so memory follows the distinct values. With
// ORDER BY and a LIMIT only the best SKIP+LIMIT rows are kept (see topN).
func (ex *exec) bufferStage(p *syntax.Projection, where syntax.Expr, items []projItem, names []string, next *stage) *stage {
	var out, origs []row
	var seen map[string]bool
	if p.Distinct {
		seen = map[string]bool{}
	}
	var top *topN
	topChecked := false
	return &stage{
		push: func(r row) error {
			if !topChecked {
				topChecked = true
				top = ex.newTopN(p)
			}
			nr, err := ex.project(items, r)
			if err != nil {
				return err
			}
			if seen != nil {
				k := rowKey(nr, names)
				if seen[k] {
					return nil
				}
				seen[k] = true
			}
			if top != nil {
				top.add(ex, nr, r)
				return nil
			}
			out = append(out, nr)
			origs = append(origs, r)
			return nil
		},
		finish: func() error {
			if top != nil {
				if top.err != nil {
					return top.err
				}
				out, origs = top.sorted()
			}
			rows, err := ex.finishProjectionRows(p, where, names, false, out, origs, nil)
			if err != nil {
				return err
			}
			out, origs, top = nil, nil, nil
			return flush(rows, next)
		},
	}
}

// topN keeps the best n rows of an ORDER BY ... SKIP s LIMIT l (n = s+l) as
// they arrive, in a heap whose root is the worst row kept, so memory follows
// the limit, not the input. Rows that tie on every ORDER BY key are ordered by
// arrival, which is what the stable sort of the whole input does, so the rows
// kept are exactly the first n of that sort. A failure evaluating a key is held
// back until the input is complete, as the sort of the whole input reports it
// only then.
type topN struct {
	order []syntax.SortItem
	n     int
	heap  []topEntry
	seq   int
	err   error
}

type topEntry struct {
	keys      []any
	seq       int
	out, orig row
}

// newTopN returns a topN for the projection, or nil when it does not apply: no
// ORDER BY, no LIMIT, or a SKIP/LIMIT that cannot be evaluated or leaves nothing
// to keep (the whole-input path then reports the error).
func (ex *exec) newTopN(p *syntax.Projection) *topN {
	if len(p.Order) == 0 || p.Limit == nil || topNDisabled {
		return nil
	}
	skip, err := ex.nonNegInt(p.Skip, "SKIP")
	if err != nil {
		return nil
	}
	limit, err := ex.nonNegInt(p.Limit, "LIMIT")
	if err != nil || limit <= 0 {
		return nil
	}
	skip = max(skip, 0)
	if skip > math.MaxInt32 || limit > math.MaxInt32 || skip+limit > 1<<20 {
		return nil // keep memory bounded by the input, not by an absurd limit
	}
	topNUsed.Add(1)
	return &topN{order: p.Order, n: int(skip + limit)}
}

// before reports whether a sorts ahead of b.
func (t *topN) before(a, b *topEntry) bool {
	for j, s := range t.order {
		c := orderCompare(a.keys[j], b.keys[j])
		if s.Desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
	}
	return a.seq < b.seq
}

func (t *topN) add(ex *exec, out, orig row) {
	if t.err != nil {
		return
	}
	scope := orig.clone()
	for k, v := range out {
		scope[k] = v
	}
	keys := make([]any, len(t.order))
	for j, s := range t.order {
		v, err := ex.eval(s.Expr, scope)
		if err != nil {
			t.err = err
			t.heap = nil
			return
		}
		keys[j] = v
	}
	e := topEntry{keys: keys, seq: t.seq, out: out, orig: orig}
	t.seq++
	if len(t.heap) < t.n {
		t.heap = append(t.heap, e)
		// sift up: the worst row at the root
		for i := len(t.heap) - 1; i > 0; {
			p := (i - 1) / 2
			if !t.before(&t.heap[p], &t.heap[i]) {
				break
			}
			t.heap[p], t.heap[i] = t.heap[i], t.heap[p]
			i = p
		}
		return
	}
	if !t.before(&e, &t.heap[0]) {
		return
	}
	t.heap[0] = e
	for i := 0; ; {
		l, r, w := 2*i+1, 2*i+2, i
		if l < len(t.heap) && t.before(&t.heap[w], &t.heap[l]) {
			w = l
		}
		if r < len(t.heap) && t.before(&t.heap[w], &t.heap[r]) {
			w = r
		}
		if w == i {
			return
		}
		t.heap[i], t.heap[w] = t.heap[w], t.heap[i]
		i = w
	}
}

// sorted returns the rows kept, best first.
func (t *topN) sorted() (out, origs []row) {
	sort.Slice(t.heap, func(a, b int) bool { return t.before(&t.heap[a], &t.heap[b]) })
	for _, e := range t.heap {
		out = append(out, e.out)
		origs = append(origs, e.orig)
	}
	return out, origs
}

// aggGroup is the state of one group: its grouping key values, its first row
// (kept only when an item needs it) and one accumulator per aggregate call.
type aggGroup struct {
	key   []any
	first row
	accs  map[*syntax.FuncCall]*accumulator
}

// aggregateStage aggregates incrementally: a row updates its group's
// accumulators and is then dropped.
func (ex *exec) aggregateStage(p *syntax.Projection, where syntax.Expr, items []projItem, names []string, aggItem []bool, next *stage) *stage {
	var keyIdx []int
	var calls []*syntax.FuncCall
	for i, it := range items {
		if !aggItem[i] {
			keyIdx = append(keyIdx, i)
		} else {
			calls = append(calls, aggregateCalls(it.expr)...)
		}
	}
	// When every aggregate item is just an aggregate call (count(x) AS n) the
	// first row of a group is not needed: the grouping items are already
	// evaluated as the key, and an aggregate call reads nothing from the row. The
	// group then holds no node or row, only its key and accumulators. An item
	// that mixes an aggregate with other variables, and the percentile functions
	// (which read an argument from the first row), keep it.
	slim := true
	for i, it := range items {
		if !aggItem[i] {
			continue
		}
		f, ok := it.expr.(*syntax.FuncCall)
		if !ok || !isAggregate(f) || strings.HasPrefix(strings.ToLower(f.Name), "percentile") {
			slim = false
		}
	}
	keyPos := make([]int, len(items)) // item index -> position in the key
	for j, i := range keyIdx {
		keyPos[i] = j
	}
	newGroup := func() *aggGroup {
		g := &aggGroup{accs: make(map[*syntax.FuncCall]*accumulator, len(calls))}
		for _, f := range calls {
			g.accs[f] = newAccumulator(f)
		}
		return g
	}
	var (
		index  = map[string]*aggGroup{}
		groups []*aggGroup
	)
	global := len(keyIdx) == 0
	if global {
		// An aggregation without grouping keys yields one row even for no input.
		groups = append(groups, newGroup())
	}
	return &stage{
		push: func(r row) error {
			var g *aggGroup
			if global {
				g = groups[0]
			} else {
				key := make([]any, len(keyIdx))
				for j, i := range keyIdx {
					v, err := ex.eval(items[i].expr, r)
					if err != nil {
						return err
					}
					key[j] = v
				}
				k := groupKey(key)
				var ok bool
				if g, ok = index[k]; !ok {
					g = newGroup()
					g.key = key
					index[k] = g
					groups = append(groups, g)
				}
			}
			if g.first == nil && !slim {
				g.first = r
			}
			for _, f := range calls {
				if err := g.accs[f].add(ex, r); err != nil {
					return err
				}
			}
			return nil
		},
		finish: func() error {
			var out, origs []row
			var substs []map[uint64]any
			for _, g := range groups {
				first := g.first
				if first == nil {
					first = row{}
				}
				ex.agg = map[*syntax.FuncCall]any{}
				for _, f := range calls {
					v, err := g.accs[f].result(ex, first)
					if err != nil {
						ex.agg = nil
						return err
					}
					ex.agg[f] = v
				}
				nr := make(row, len(items))
				subst := map[uint64]any{}
				for i, it := range items {
					var v any
					if slim && !aggItem[i] {
						v = g.key[keyPos[i]] // evaluated when the row arrived
					} else {
						var err error
						if v, err = ex.eval(it.expr, first); err != nil {
							ex.agg = nil
							return err
						}
					}
					nr[it.name] = v
					subst[ex.hasher.Hash(it.expr)] = v
				}
				ex.agg = nil
				out = append(out, nr)
				origs = append(origs, first)
				substs = append(substs, subst)
			}
			groups, index = nil, nil
			rows, err := ex.finishProjectionRows(p, where, names, true, out, origs, substs)
			if err != nil {
				return err
			}
			return flush(rows, next)
		},
	}
}

// accumulator is the incremental form of one aggregate call. It reproduces
// exec.aggregate: null values are skipped, DISTINCT drops repeated values, and
// the reduction is the same, but each value is folded in as it arrives instead
// of being kept with its row until the end.
type accumulator struct {
	f    *syntax.FuncCall
	name string
	seen map[string]bool // DISTINCT

	n    int64 // count, and how many values avg has folded in
	vals []any // collect, percentiles and stdev need the values themselves

	// sum
	isum     int64
	fsum     float64
	useFloat bool

	total   float64 // avg
	best    any     // min, max
	hasBest bool
}

func newAccumulator(f *syntax.FuncCall) *accumulator {
	a := &accumulator{f: f, name: strings.ToLower(f.Name)}
	if f.Distinct {
		a.seen = map[string]bool{}
	}
	return a
}

func (a *accumulator) add(ex *exec, r row) error {
	if a.f.Star {
		a.n++
		return nil
	}
	v, err := ex.eval(a.f.Args[0], r)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	if a.f.Distinct {
		k := groupKey(v)
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	switch a.name {
	case "count":
		a.n++
	case "collect", "percentilecont", "percentiledisc", "stdev", "stdevp":
		if a.name == "collect" && len(a.vals) >= maxListLen {
			return errListTooLong()
		}
		a.vals = append(a.vals, v)
	case "sum":
		switch x := v.(type) {
		case int64:
			if !a.useFloat {
				s := a.isum + x
				if (a.isum > 0 && x > 0 && s < 0) || (a.isum < 0 && x < 0 && s >= 0) {
					return intOverflow()
				}
				a.isum = s
			} else {
				a.fsum += float64(x)
			}
		case float64:
			if !a.useFloat {
				a.useFloat = true
				a.fsum = float64(a.isum)
			}
			a.fsum += x
		default:
			return typeErr("sum() expects numbers, got %s", typeName(v))
		}
	case "avg":
		if !isNumber(v) {
			return typeErr("avg() expects numbers, got %s", typeName(v))
		}
		a.total += toFloat(v)
		a.n++
	case "min", "max":
		if !a.hasBest {
			a.best, a.hasBest = v, true
			return nil
		}
		c := orderCompare(v, a.best)
		if (a.name == "min" && c < 0) || (a.name == "max" && c > 0) {
			a.best = v
		}
	}
	return nil
}

// result is the aggregate's value; first is the group's first row, which the
// percentile functions read their percentile argument from.
func (a *accumulator) result(ex *exec, first row) (any, error) {
	switch a.name {
	case "count":
		return a.n, nil
	case "collect":
		if a.vals == nil {
			return []any{}, nil
		}
		return a.vals, nil
	case "sum":
		if a.useFloat {
			return a.fsum, nil
		}
		return a.isum, nil
	case "avg":
		if a.n == 0 {
			return nil, nil
		}
		return a.total / float64(a.n), nil
	case "min", "max":
		if !a.hasBest {
			return nil, nil
		}
		return a.best, nil
	case "percentilecont", "percentiledisc":
		return ex.percentile(a.name, a.f, []row{first}, a.vals)
	case "stdev", "stdevp":
		return stdev(a.name == "stdevp", a.vals)
	}
	return nil, unsupported("aggregate %s()", a.f.Name)
}

// readOnlyBody reports whether a statement body only reads the graph: no
// updating clause, no procedure call (which may write), at any depth.
func readOnlyBody(b syntax.Body) bool {
	switch b := b.(type) {
	case *syntax.SingleQuery:
		for _, cl := range b.Clauses {
			switch cl := cl.(type) {
			case *syntax.Match, *syntax.Unwind, *syntax.With, *syntax.Return,
				*syntax.Filter, *syntax.Let, *syntax.Finish:
			case *syntax.CallSubquery:
				if !readOnlyBody(cl.Body) {
					return false
				}
			default:
				return false
			}
		}
		return true
	case *syntax.UnionQuery:
		for _, q := range b.Queries {
			if !readOnlyBody(q) {
				return false
			}
		}
		return true
	case *syntax.Conditional:
		for _, br := range b.Branches {
			if !readOnlyBody(br.Body) {
				return false
			}
		}
		return b.Else == nil || readOnlyBody(b.Else)
	}
	return false
}
