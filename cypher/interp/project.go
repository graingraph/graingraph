package interp

import (
	"math"
	"sort"
	"strings"

	"github.com/graingraph/graingraph/cypher/syntax"
)

// projItem is one output column of a WITH/RETURN.
type projItem struct {
	name string
	expr syntax.Expr
}

func projItems(p *syntax.Projection, st *qstate) []projItem {
	var items []projItem
	if p.Star {
		for _, v := range st.vars {
			items = append(items, projItem{v, &syntax.Ident{Name: v}})
		}
	}
	for _, it := range p.Items {
		name := it.Alias
		if name == "" {
			if id, ok := it.Expr.(*syntax.Ident); ok {
				name = id.Name
			} else {
				name = it.Source
			}
		}
		items = append(items, projItem{name, it.Expr})
	}
	return items
}

func (ex *exec) projectionNames(p *syntax.Projection, st *qstate) ([]string, error) {
	items := projItems(p, st)
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	return names, nil
}

// aggregateCalls returns the aggregate calls in e, outermost first.
func aggregateCalls(e syntax.Expr) []*syntax.FuncCall {
	var out []*syntax.FuncCall
	walkExpr(e, func(n syntax.Expr) bool {
		if f, ok := n.(*syntax.FuncCall); ok && isAggregate(f) {
			out = append(out, f)
			return false
		}
		return true
	})
	return out
}

// walkExpr visits e and its sub-expressions that share its scope.
func walkExpr(e syntax.Expr, fn func(syntax.Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	var kids []syntax.Expr
	switch e := e.(type) {
	case *syntax.Binary:
		kids = []syntax.Expr{e.L, e.R}
	case *syntax.Unary:
		kids = []syntax.Expr{e.X}
	case *syntax.Comparison:
		kids = e.Operands
	case *syntax.IsNull:
		kids = []syntax.Expr{e.X}
	case *syntax.HasLabels:
		kids = []syntax.Expr{e.X}
	case *syntax.Property:
		kids = []syntax.Expr{e.Subject}
	case *syntax.Subscript:
		kids = []syntax.Expr{e.Subject, e.Index}
	case *syntax.Slice:
		kids = []syntax.Expr{e.Subject}
		if e.From != nil {
			kids = append(kids, e.From)
		}
		if e.To != nil {
			kids = append(kids, e.To)
		}
	case *syntax.FuncCall:
		kids = e.Args
	case *syntax.Case:
		if e.Subject != nil {
			kids = append(kids, e.Subject)
		}
		for _, w := range e.Whens {
			kids = append(kids, w.Cond, w.Then)
		}
		if e.Else != nil {
			kids = append(kids, e.Else)
		}
	case *syntax.ListLit:
		kids = e.Elems
	case *syntax.MapLit:
		for _, en := range e.Entries {
			kids = append(kids, en.Value)
		}
	case *syntax.ListComp:
		kids = []syntax.Expr{e.In}
	case *syntax.Quantifier:
		kids = []syntax.Expr{e.In}
	case *syntax.Reduce:
		kids = []syntax.Expr{e.Init, e.In}
	case *syntax.MapProjection:
		kids = []syntax.Expr{e.Subject}
		for _, it := range e.Items {
			if it.Value != nil {
				kids = append(kids, it.Value)
			}
		}
	}
	for _, k := range kids {
		walkExpr(k, fn)
	}
}

type group struct {
	rows []row
	key  []any
}

// projection executes a WITH or RETURN over st.rows, replacing them and the
// scope with the projected rows.
func (ex *exec) projection(p *syntax.Projection, where syntax.Expr, st *qstate, isReturn bool) error {
	items := projItems(p, st)
	names := make([]string, len(items))
	aggItem := make([]bool, len(items))
	aggregating := false
	for i, it := range items {
		names[i] = it.name
		aggItem[i] = len(aggregateCalls(it.expr)) > 0
		aggregating = aggregating || aggItem[i]
	}

	var out, origs []row
	var substs []map[uint64]any
	if !aggregating {
		for _, r := range st.rows {
			nr := make(row, len(items))
			for _, it := range items {
				v, err := ex.eval(it.expr, r)
				if err != nil {
					return err
				}
				nr[it.name] = v
			}
			out = append(out, nr)
			origs = append(origs, r)
		}
	} else {
		groups, err := ex.groupRows(items, aggItem, st.rows)
		if err != nil {
			return err
		}
		for _, g := range groups {
			nr := make(row, len(items))
			first := row{}
			if len(g.rows) > 0 {
				first = g.rows[0]
			}
			ex.agg = map[*syntax.FuncCall]any{}
			for i, it := range items {
				if !aggItem[i] {
					continue
				}
				for _, f := range aggregateCalls(it.expr) {
					v, err := ex.aggregate(f, g.rows)
					if err != nil {
						ex.agg = nil
						return err
					}
					ex.agg[f] = v
				}
			}
			subst := map[uint64]any{}
			for i, it := range items {
				var v any
				var err error
				if aggItem[i] {
					v, err = ex.eval(it.expr, first)
				} else {
					v, err = ex.eval(it.expr, first)
				}
				if err != nil {
					ex.agg = nil
					return err
				}
				nr[it.name] = v
				subst[ex.hasher.Hash(it.expr)] = v
			}
			ex.agg = nil
			out = append(out, nr)
			origs = append(origs, first)
			substs = append(substs, subst)
		}
	}

	return ex.finishProjection(p, where, st, names, aggregating, out, origs, substs)
}

// finishProjection applies DISTINCT, ORDER BY, SKIP/LIMIT and the WITH's WHERE to
// the projected rows and installs them as the query state.
func (ex *exec) finishProjection(p *syntax.Projection, where syntax.Expr, st *qstate, names []string, aggregating bool, out, origs []row, substs []map[uint64]any) error {
	out, err := ex.finishProjectionRows(p, where, names, aggregating, out, origs, substs)
	if err != nil {
		return err
	}
	st.rows = out
	st.vars = names
	return nil
}

// finishProjectionRows applies DISTINCT, ORDER BY, SKIP/LIMIT and the WITH's
// WHERE to projected rows and returns the result.
func (ex *exec) finishProjectionRows(p *syntax.Projection, where syntax.Expr, names []string, aggregating bool, out, origs []row, substs []map[uint64]any) ([]row, error) {
	if p.Distinct {
		seen := map[string]bool{}
		var o2, g2 []row
		var s2 []map[uint64]any
		for i, r := range out {
			k := rowKey(r, names)
			if seen[k] {
				continue
			}
			seen[k] = true
			o2 = append(o2, r)
			g2 = append(g2, origs[i])
			if substs != nil {
				s2 = append(s2, substs[i])
			}
		}
		out, origs, substs = o2, g2, s2
	}

	if len(p.Order) > 0 {
		if err := ex.sortRows(p, out, origs, substs, aggregating, p.Distinct); err != nil {
			return nil, err
		}
	}
	// sortRows permutes out/origs together via an index permutation stored in
	// the slices it was given.

	if p.Skip != nil || p.Limit != nil {
		skip, err := ex.nonNegInt(p.Skip, "SKIP")
		if err != nil {
			return nil, err
		}
		limit, err := ex.nonNegInt(p.Limit, "LIMIT")
		if err != nil {
			return nil, err
		}
		if skip > 0 {
			if skip >= int64(len(out)) {
				out, origs = nil, nil
			} else {
				out, origs = out[skip:], origs[skip:]
			}
		}
		if limit >= 0 && limit < int64(len(out)) {
			out, origs = out[:limit], origs[:limit]
		}
	}

	if where != nil {
		var o2 []row
		for i, r := range out {
			scopeRow := r
			if !aggregating {
				scopeRow = origs[i].clone()
				for k, v := range r {
					scopeRow[k] = v
				}
			}
			c, err := ex.eval(where, scopeRow)
			if err != nil {
				return nil, err
			}
			if c == true {
				o2 = append(o2, r)
			}
		}
		out = o2
	}

	return out, nil
}

// nonNegInt evaluates a SKIP/LIMIT expression; nil gives -1 (absent).
func (ex *exec) nonNegInt(e syntax.Expr, what string) (int64, error) {
	if e == nil {
		return -1, nil
	}
	v, err := ex.eval(e, row{})
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, errorf("SyntaxError", "InvalidArgumentType", "%s must be an integer, got %s", what, typeName(v))
	}
	if n < 0 {
		return 0, errorf("SyntaxError", "NegativeIntegerArgument", "%s must not be negative", what)
	}
	return n, nil
}

// groupRows groups the rows by the values of the non-aggregating items.
func (ex *exec) groupRows(items []projItem, aggItem []bool, rows []row) ([]*group, error) {
	var keyIdx []int
	for i := range items {
		if !aggItem[i] {
			keyIdx = append(keyIdx, i)
		}
	}
	if len(keyIdx) == 0 {
		return []*group{{rows: rows}}, nil
	}
	index := map[string]*group{}
	var groups []*group
	for _, r := range rows {
		key := make([]any, len(keyIdx))
		for j, i := range keyIdx {
			v, err := ex.eval(items[i].expr, r)
			if err != nil {
				return nil, err
			}
			key[j] = v
		}
		k := groupKey(key)
		g, ok := index[k]
		if !ok {
			g = &group{key: key}
			index[k] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	return groups, nil
}

// sortRows orders out (and the parallel origs/substs) by the ORDER BY items.
func (ex *exec) sortRows(p *syntax.Projection, out, origs []row, substs []map[uint64]any, aggregating, distinct bool) error {
	n := len(out)
	keys := make([][]any, n)
	for i := 0; i < n; i++ {
		scope := out[i]
		if !aggregating && !distinct {
			scope = origs[i].clone()
			for k, v := range out[i] {
				scope[k] = v
			}
		} else if distinct && !aggregating {
			scope = origs[i].clone()
			for k, v := range out[i] {
				scope[k] = v
			}
		}
		if substs != nil {
			ex.subst = substs[i]
		}
		ks := make([]any, len(p.Order))
		for j, s := range p.Order {
			v, err := ex.eval(s.Expr, scope)
			if err != nil {
				ex.subst = nil
				return err
			}
			ks[j] = v
		}
		ex.subst = nil
		keys[i] = ks
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		for j, s := range p.Order {
			c := orderCompare(keys[idx[a]][j], keys[idx[b]][j])
			if s.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	o2, g2 := make([]row, n), make([]row, n)
	for i, k := range idx {
		o2[i], g2[i] = out[k], origs[k]
	}
	copy(out, o2)
	copy(origs, g2)
	return nil
}

// ─── aggregate functions ─────────────────────────────────────────────────────

func (ex *exec) aggregate(f *syntax.FuncCall, rows []row) (any, error) {
	name := strings.ToLower(f.Name)
	if f.Star {
		return int64(len(rows)), nil
	}
	if len(f.Args) == 0 {
		return nil, errorf("SyntaxError", "InvalidNumberOfArguments", "%s() requires an argument", f.Name)
	}
	var vals []any
	seen := map[string]bool{}
	for _, r := range rows {
		v, err := ex.eval(f.Args[0], r)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		if f.Distinct {
			k := groupKey(v)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		if name == "collect" && len(vals) >= maxListLen {
			return nil, errListTooLong()
		}
		vals = append(vals, v)
	}
	switch name {
	case "count":
		return int64(len(vals)), nil
	case "collect":
		if vals == nil {
			return []any{}, nil
		}
		return vals, nil
	case "sum":
		return sumValues(vals)
	case "avg":
		if len(vals) == 0 {
			return nil, nil
		}
		var total float64
		for _, v := range vals {
			if !isNumber(v) {
				return nil, typeErr("avg() expects numbers, got %s", typeName(v))
			}
			total += toFloat(v)
		}
		return total / float64(len(vals)), nil
	case "min", "max":
		if len(vals) == 0 {
			return nil, nil
		}
		best := vals[0]
		for _, v := range vals[1:] {
			c := orderCompare(v, best)
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best = v
			}
		}
		return best, nil
	case "percentilecont", "percentiledisc":
		return ex.percentile(name, f, rows, vals)
	case "stdev", "stdevp":
		return stdev(name == "stdevp", vals)
	}
	return nil, unsupported("aggregate %s()", f.Name)
}

func sumValues(vals []any) (any, error) {
	var isum int64
	var fsum float64
	useFloat := false
	for _, v := range vals {
		switch x := v.(type) {
		case int64:
			if !useFloat {
				s := isum + x
				if (isum > 0 && x > 0 && s < 0) || (isum < 0 && x < 0 && s >= 0) {
					return nil, intOverflow()
				}
				isum = s
			} else {
				fsum += float64(x)
			}
		case float64:
			if !useFloat {
				useFloat = true
				fsum = float64(isum)
			}
			fsum += x
		default:
			return nil, typeErr("sum() expects numbers, got %s", typeName(v))
		}
	}
	if useFloat {
		return fsum, nil
	}
	return isum, nil
}

func (ex *exec) percentile(name string, f *syntax.FuncCall, rows []row, vals []any) (any, error) {
	if len(f.Args) != 2 {
		return nil, errorf("SyntaxError", "InvalidNumberOfArguments", "%s() requires two arguments", f.Name)
	}
	first := row{}
	if len(rows) > 0 {
		first = rows[0]
	}
	pv, err := ex.eval(f.Args[1], first)
	if err != nil {
		return nil, err
	}
	if !isNumber(pv) {
		return nil, typeErr("percentile must be a number")
	}
	p := toFloat(pv)
	if p < 0 || p > 1 || math.IsNaN(p) {
		return nil, errorf("ArgumentError", "NumberOutOfRange", "percentile must be between 0.0 and 1.0")
	}
	if len(vals) == 0 {
		return nil, nil
	}
	nums := make([]float64, len(vals))
	for i, v := range vals {
		if !isNumber(v) {
			return nil, typeErr("%s() expects numbers, got %s", f.Name, typeName(v))
		}
		nums[i] = toFloat(v)
	}
	sort.Float64s(nums)
	if name == "percentiledisc" {
		idx := int(math.Ceil(p*float64(len(nums)))) - 1
		if idx < 0 {
			idx = 0
		}
		// Keep integers integral.
		for _, v := range vals {
			if _, isInt := v.(int64); !isInt {
				return nums[idx], nil
			}
		}
		return int64(nums[idx]), nil
	}
	rank := p * float64(len(nums)-1)
	lo, hi := int(math.Floor(rank)), int(math.Ceil(rank))
	return nums[lo] + (nums[hi]-nums[lo])*(rank-float64(lo)), nil
}

func stdev(population bool, vals []any) (any, error) {
	n := float64(len(vals))
	if len(vals) == 0 {
		return 0.0, nil
	}
	var sum float64
	nums := make([]float64, len(vals))
	for i, v := range vals {
		if !isNumber(v) {
			return nil, typeErr("stDev() expects numbers, got %s", typeName(v))
		}
		nums[i] = toFloat(v)
		sum += nums[i]
	}
	mean := sum / n
	var sq float64
	for _, x := range nums {
		sq += (x - mean) * (x - mean)
	}
	if population {
		return math.Sqrt(sq / n), nil
	}
	if len(vals) < 2 {
		return 0.0, nil
	}
	return math.Sqrt(sq / (n - 1)), nil
}
