package interp

import (
	"context"
	"time"

	"github.com/graingraph/graingraph/cypher/syntax"
)

// Result is the outcome of executing a statement.
type Result struct {
	Columns  []string
	Rows     [][]any
	Counters Counters
	// VectorDeltas are the changes the statement made to vector indexes. After
	// the transaction commits, pass them to Engine.ApplyVectorDeltas.
	VectorDeltas []VectorDelta
}

// Options tune how a statement runs.
type Options struct {
	// ReadOnly means the database handle cannot write: automatic index creation
	// is deferred to the caller.
	ReadOnly bool
	// VectorCache lets vector searches use the shared in-memory matrices. Set it
	// only for a statement that has no uncommitted writes of its own (no updating
	// clause, and not inside an explicit transaction).
	VectorCache bool
}

// RunWithOptions is RunWith with explicit Options.
func RunWithOptions(ctx context.Context, db DB, st *syntax.Statement, params map[string]any, eng *Engine, opts Options) (*Result, error) {
	return run(ctx, db, st, params, eng, opts)
}

// Run executes a parsed (and analysed) statement against db. Updates are
// applied through db as they happen, so the caller should run Run inside a
// transaction and roll back on error. params are the query parameters.
func Run(ctx context.Context, db DB, st *syntax.Statement, params map[string]any) (*Result, error) {
	return RunWith(ctx, db, st, params, nil)
}

// RunWith is Run using an Engine's registered procedures and index advisor.
func RunWith(ctx context.Context, db DB, st *syntax.Statement, params map[string]any, eng *Engine) (*Result, error) {
	return run(ctx, db, st, params, eng, Options{})
}

// RunReadOnly is RunWith for a db that cannot write (a read-only connection or
// transaction). The statement must not contain updating clauses. Automatic
// index creation is not attempted; keys worth indexing are queued on the Engine
// (see Engine.TakeWantedIndexes).
func RunReadOnly(ctx context.Context, db DB, st *syntax.Statement, params map[string]any, eng *Engine) (*Result, error) {
	return run(ctx, db, st, params, eng, Options{ReadOnly: true, VectorCache: true})
}

func run(ctx context.Context, db DB, st *syntax.Statement, params map[string]any, eng *Engine, opts Options) (res *Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(valueTooDeep); !ok {
				panic(r)
			}
			res, err = nil, errorf("ArgumentError", "ValueTooDeep", "a list or map is nested more than %d levels deep", maxValueDepth)
		}
	}()
	if st.Mode == syntax.ModeExplain {
		// EXPLAIN compiles the query but does not run it: no rows, no effects.
		return &Result{Columns: explainColumns(st.Body)}, nil
	}
	g := newGraph(ctx, db)
	g.readOnly = opts.ReadOnly
	g.vectorCache = opts.VectorCache
	g.evictable = opts.ReadOnly || readOnlyBody(st.Body)
	g.eng = eng
	ex := &exec{g: g, params: params, clock: time.Now()}
	if eng != nil {
		ex.procs = &eng.Procs
	}
	cols, rows, err := ex.runBody(st.Body, []row{{}})
	if err != nil {
		return nil, err
	}
	if err := g.finish(); err != nil {
		return nil, err
	}
	res = &Result{Columns: cols, Counters: g.counters, VectorDeltas: g.deltas}
	if len(cols) == 0 {
		return res, nil // a query without RETURN produces no rows
	}
	for _, r := range rows {
		vals := make([]any, len(cols))
		for i, c := range cols {
			vals[i] = r[c]
			if nestingExceeds(vals[i], maxValueDepth) {
				// callers and encoders recurse over a returned value
				return nil, errorf("ArgumentError", "ValueTooDeep", "a list or map is nested more than %d levels deep", maxValueDepth)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, nil
}

// runBody executes a statement body over the given input rows.
func (ex *exec) runBody(b syntax.Body, in []row) ([]string, []row, error) {
	switch b := b.(type) {
	case *syntax.SingleQuery:
		return ex.runQuery(b, in, nil)
	case *syntax.UnionQuery:
		var cols []string
		var out []row
		seen := map[string]bool{}
		for i, q := range b.Queries {
			qcols, qrows, err := ex.runQuery(q, in, nil)
			if err != nil {
				return nil, nil, err
			}
			if i == 0 {
				cols = qcols
			}
			for _, r := range qrows {
				nr := make(row, len(cols))
				for _, c := range cols {
					nr[c] = r[c]
				}
				if !b.All {
					key := rowKey(nr, cols)
					if seen[key] {
						continue
					}
					seen[key] = true
				}
				out = append(out, nr)
			}
		}
		return cols, out, nil
	case *syntax.CreateIndex:
		return nil, nil, ex.execCreateIndex(b)
	case *syntax.CreateConstraint:
		return nil, nil, ex.execCreateConstraint(b)
	case *syntax.DropSchema:
		return nil, nil, ex.execDropSchema(b)
	case *syntax.Show:
		return ex.execShow(b)
	case *syntax.Conditional:
		for _, w := range b.Branches {
			for _, r := range in {
				c, err := ex.eval(w.Cond, r)
				if err != nil {
					return nil, nil, err
				}
				if c == true {
					return ex.runBody(w.Body, in)
				}
			}
		}
		if b.Else != nil {
			return ex.runBody(b.Else, in)
		}
		return nil, in, nil
	}
	return nil, nil, unsupported("statement %T", b)
}

func rowKey(r row, cols []string) string {
	vals := make([]any, len(cols))
	for i, c := range cols {
		vals[i] = r[c]
	}
	return groupKey(vals)
}

// qstate is the evolving state of a query: the rows and the variables in scope
// (in declaration order, for RETURN *).
type qstate struct {
	rows []row
	vars []string
}

func (s *qstate) declare(names ...string) {
	for _, n := range names {
		if n == "" || containsStr(s.vars, n) {
			continue
		}
		s.vars = append(s.vars, n)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// runQuery runs the clauses of one query over the input rows and returns the
// output columns (those of the final RETURN) and rows.
func (ex *exec) runQuery(q *syntax.SingleQuery, in []row, scopeVars []string) ([]string, []row, error) {
	st := &qstate{rows: in}
	for _, r := range in {
		for k := range r {
			st.declare(k)
		}
		break
	}
	var cols []string
	for i := 0; i < len(q.Clauses); i++ {
		cl := q.Clauses[i]
		if i == 0 {
			if names, handled, err := ex.aggregatePushdown(q, st); err != nil {
				return nil, nil, err
			} else if handled {
				// The MATCH and the RETURN/WITH after it ran as one SQL aggregation.
				if _, isReturn := q.Clauses[1].(*syntax.Return); isReturn {
					cols = names
				}
				i++
				continue
			}
		}
		if rest := q.Clauses[i:]; !streamDisabled && streamable(rest) {
			// The rest of the query only reads: run it as a pipeline.
			cols, err := ex.runStream(rest, st, cols)
			if err != nil {
				return nil, nil, err
			}
			return cols, st.rows, nil
		}
		var err error
		cols, err = ex.clause(cl, st, cols)
		if err != nil {
			return nil, nil, err
		}
		if call, ok := cl.(*syntax.Call); ok && len(q.Clauses) == 1 {
			// A standalone CALL returns its yielded columns.
			cols = ex.callColumns(call)
		}
	}
	return cols, st.rows, nil
}

func (ex *exec) clause(cl syntax.Clause, st *qstate, cols []string) ([]string, error) {
	switch cl := cl.(type) {
	case *syntax.Match:
		return cols, ex.execMatch(cl, st)
	case *syntax.Unwind:
		return cols, ex.execUnwind(cl, st)
	case *syntax.With:
		return cols, ex.projection(&cl.Projection, cl.Where, st, false)
	case *syntax.Return:
		names, err := ex.projectionNames(&cl.Projection, st)
		if err != nil {
			return nil, err
		}
		if err := ex.projection(&cl.Projection, nil, st, true); err != nil {
			return nil, err
		}
		return names, nil
	case *syntax.Create:
		return cols, ex.execCreate(cl, st)
	case *syntax.Merge:
		return cols, ex.execMerge(cl, st)
	case *syntax.Set:
		return cols, ex.mapRows(st, func(r row) error { return ex.applySet(cl.Items, r) })
	case *syntax.Remove:
		return cols, ex.mapRows(st, func(r row) error { return ex.applyRemove(cl, r) })
	case *syntax.Delete:
		return cols, ex.mapRows(st, func(r row) error { return ex.applyDelete(cl, r) })
	case *syntax.Foreach:
		return cols, ex.mapRows(st, func(r row) error { return ex.applyForeach(cl, r) })
	case *syntax.Filter:
		var out []row
		for _, r := range st.rows {
			c, err := ex.eval(cl.Cond, r)
			if err != nil {
				return nil, err
			}
			if c == true {
				out = append(out, r)
			}
		}
		st.rows = out
		return cols, nil
	case *syntax.Let:
		for _, it := range cl.Items {
			st.declare(it.Var)
		}
		for i, r := range st.rows {
			for _, it := range cl.Items {
				v, err := ex.eval(it.Expr, r)
				if err != nil {
					return nil, err
				}
				r = r.with(it.Var, v)
			}
			st.rows[i] = r
		}
		return cols, nil
	case *syntax.LoadCSV:
		return cols, ex.execLoadCSV(cl, st)
	case *syntax.Finish:
		st.rows = nil
		return nil, nil
	case *syntax.Call:
		return cols, ex.execCall(cl, st)
	case *syntax.CallSubquery:
		return cols, ex.execCallSubquery(cl, st)
	}
	return nil, unsupported("clause %s", syntax.ClauseName(cl))
}

// mapRows applies an updating function to every row.
func (ex *exec) mapRows(st *qstate, fn func(row) error) error {
	for _, r := range st.rows {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// ─── MATCH, UNWIND ───────────────────────────────────────────────────────────

func patternVars(parts []*syntax.PatternPart) []string {
	var out []string
	var walk func(elems []syntax.PatternElem)
	walk = func(elems []syntax.PatternElem) {
		for _, el := range elems {
			switch el := el.(type) {
			case *syntax.NodePattern:
				if el.Var != "" {
					out = append(out, el.Var)
				}
			case *syntax.RelPattern:
				if el.Var != "" {
					out = append(out, el.Var)
				}
			case *syntax.GroupPattern:
				walk(el.Elems)
			}
		}
	}
	for _, p := range parts {
		walk(p.Elems)
		if p.Var != "" {
			out = append(out, p.Var)
		}
	}
	return out
}

func (ex *exec) execMatch(cl *syntax.Match, st *qstate) error {
	vars := patternVars(cl.Patterns)
	st.declare(vars...)
	defer func(saved []pushedEq) { ex.pushed = saved }(ex.pushed)
	ex.pushed = whereEqualities(cl.Where)

	// Fast path: collapse one start-node scan per input row into one
	// json_each IN query when the shape is a UNWIND-driven string-key bulk
	// lookup; on any other shape returns handled=false and we fall through to
	// the per-row scan unchanged.
	if handled, err := ex.tryExecMatchBatched(cl, vars, st); handled {
		return err
	}

	var out []row
	for _, r := range st.rows {
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
			out = append(out, r2)
			return nil
		})
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
			out = append(out, nr)
		}
	}
	st.rows = out
	return nil
}

func (ex *exec) execUnwind(cl *syntax.Unwind, st *qstate) error {
	st.declare(cl.Var)
	var out []row
	for _, r := range st.rows {
		v, err := ex.eval(cl.Expr, r)
		if err != nil {
			return err
		}
		switch x := v.(type) {
		case nil:
		case []any:
			for _, el := range x {
				out = append(out, r.with(cl.Var, el))
			}
		default:
			out = append(out, r.with(cl.Var, x))
		}
	}
	st.rows = out
	return nil
}

// ─── CALL ────────────────────────────────────────────────────────────────────

func (ex *exec) execCallSubquery(cl *syntax.CallSubquery, st *qstate) error {
	var out []row
	var outCols []string
	for _, r := range st.rows {
		in := r
		if cl.Scoped && !cl.ImportAll {
			in = row{}
			for _, name := range cl.Imports {
				in[name] = r[name]
			}
		}
		cols, rows, err := ex.runBody(cl.Body, []row{in})
		if err != nil {
			return err
		}
		outCols = cols
		if len(cols) == 0 { // unit subquery: the outer rows pass through
			out = append(out, r)
			continue
		}
		for _, sub := range rows {
			nr := r.clone()
			for _, c := range cols {
				nr[c] = sub[c]
			}
			out = append(out, nr)
		}
		if len(rows) == 0 && cl.Optional {
			nr := r.clone()
			for _, c := range cols {
				nr[c] = nil
			}
			out = append(out, nr)
		}
	}
	st.declare(outCols...)
	st.rows = out
	return nil
}

// explainColumns are the output columns of a query, read from its final RETURN
// without executing anything (`RETURN *` yields none).
func explainColumns(b syntax.Body) []string {
	var q *syntax.SingleQuery
	switch b := b.(type) {
	case *syntax.SingleQuery:
		q = b
	case *syntax.UnionQuery:
		if len(b.Queries) > 0 {
			q = b.Queries[0]
		}
	}
	if q == nil || len(q.Clauses) == 0 {
		return nil
	}
	ret, ok := q.Clauses[len(q.Clauses)-1].(*syntax.Return)
	if !ok {
		return nil
	}
	var cols []string
	for _, it := range projItems(&ret.Projection, &qstate{}) {
		cols = append(cols, it.name)
	}
	return cols
}
