package interp

import (
	"github.com/graingraph/graingraph/cypher/proc"
	"github.com/graingraph/graingraph/cypher/spatial"
	"github.com/graingraph/graingraph/cypher/temporal"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/graingraph/graingraph/cypher/syntax"
)

// row is the set of variables visible to an expression.
type row map[string]any

// without returns a copy of r lacking name.
func (r row) without(name string) row {
	out := make(row, len(r))
	for k, v := range r {
		if k != name {
			out[k] = v
		}
	}
	return out
}

func (r row) with(name string, v any) row {
	out := make(row, len(r)+1)
	for k, x := range r {
		out[k] = x
	}
	out[name] = v
	return out
}

func (r row) clone() row {
	out := make(row, len(r))
	for k, x := range r {
		out[k] = x
	}
	return out
}

// exec is the state of one statement execution.
type exec struct {
	g      *graph
	params map[string]any
	// agg holds already-computed aggregate values while a projection item is
	// evaluated per group.
	agg map[*syntax.FuncCall]any
	// subst replaces whole sub-expressions by value (ORDER BY on projected
	// expressions).
	subst map[uint64]any
	ticks uint32 // calls to tick
	// hasher gives the structural hash that keys subst; memoised per node.
	hasher syntax.Hasher
	// regexCache avoids recompiling regular expressions.
	procs *proc.Set
	// clock is the instant temporal functions report for "now", fixed for the
	// whole statement.
	clock time.Time
	// pushed are WHERE equalities of the MATCH being executed, used to narrow
	// node scans.
	pushed     []pushedEq
	regexCache map[string]*regexp.Regexp
	// relLimit prunes pattern matching to paths of at most relLimit-1
	// relationships while a selector searches by increasing length; 0 means no
	// limit. relPruned records that the limit cut a search short.
	relLimit  int
	relPruned bool
	// streaming is positive while a pipeline runs (see stream.go); scanDepth
	// counts the chunked scans in progress.
	streaming int
	scanDepth int
}

// overBudget reports whether a path that already has n relationships may not be
// extended.
func (ex *exec) overBudget(n int) bool {
	if ex.relLimit > 0 && n+1 >= ex.relLimit {
		ex.relPruned = true
		return true
	}
	return false
}

// tick checks the statement's context every few thousand calls, so a cancelled
// or timed-out request stops burning CPU (and gives up its connection) instead
// of running to the end of an in-memory loop.
func (ex *exec) tick() error {
	ex.ticks++
	if ex.ticks&0x3ff == 0 {
		return ex.g.ctx.Err()
	}
	return nil
}

func (ex *exec) eval(e syntax.Expr, r row) (any, error) {
	if err := ex.tick(); err != nil {
		return nil, err
	}
	if ex.subst != nil {
		if v, ok := ex.subst[ex.hasher.Hash(e)]; ok {
			return v, nil
		}
	}
	switch e := e.(type) {
	case *syntax.IntLit:
		return e.Value, nil
	case *syntax.FloatLit:
		return e.Value, nil
	case *syntax.StringLit:
		return e.Value, nil
	case *syntax.BoolLit:
		return e.Value, nil
	case *syntax.NullLit:
		return nil, nil
	case *syntax.Param:
		v, ok := ex.params[e.Name]
		if !ok {
			return nil, errorf("ParameterMissing", "MissingParameter", "expected a parameter named %s", e.Name)
		}
		return normalize(v), nil
	case *syntax.Ident:
		v, ok := r[e.Name]
		if !ok {
			return nil, errorf("SyntaxError", "UndefinedVariable", "variable `%s` not defined", e.Name)
		}
		return v, nil
	case *syntax.ListLit:
		out := make([]any, len(e.Elems))
		for i, el := range e.Elems {
			v, err := ex.eval(el, r)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case *syntax.MapLit:
		out := make(map[string]any, len(e.Entries))
		for _, en := range e.Entries {
			v, err := ex.eval(en.Value, r)
			if err != nil {
				return nil, err
			}
			out[en.Key] = v
		}
		return out, nil
	case *syntax.TypePredicate:
		v, err := ex.eval(e.X, r)
		if err != nil {
			return nil, err
		}
		return matchesVTypes(v, parseVTypes(e.Type)) != e.Negated, nil
	case *syntax.Normalized:
		v, err := ex.eval(e.X, r)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		s, ok := v.(string)
		if !ok {
			return nil, typeErr("IS NORMALIZED expects a string, got %s", typeName(v))
		}
		form, err := normForm(e.Form)
		if err != nil {
			return nil, err
		}
		return form.IsNormalString(s) != e.Negated, nil
	case *syntax.Property:
		subject, err := ex.eval(e.Subject, r)
		if err != nil {
			return nil, err
		}
		return ex.property(subject, e.Key)
	case *syntax.Subscript:
		return ex.subscript(e, r)
	case *syntax.Slice:
		return ex.slice(e, r)
	case *syntax.Unary:
		return ex.unary(e, r)
	case *syntax.Binary:
		return ex.binary(e, r)
	case *syntax.Comparison:
		return ex.comparison(e, r)
	case *syntax.IsNull:
		v, err := ex.eval(e.X, r)
		if err != nil {
			return nil, err
		}
		return (v == nil) != e.Negated, nil
	case *syntax.HasLabels:
		return ex.hasLabels(e, r)
	case *syntax.FuncCall:
		return ex.call(e, r)
	case *syntax.Case:
		return ex.caseExpr(e, r)
	case *syntax.ListComp:
		return ex.listComp(e, r)
	case *syntax.Quantifier:
		return ex.quantifier(e, r)
	case *syntax.Reduce:
		return ex.reduce(e, r)
	case *syntax.PatternComp:
		return ex.patternComp(e, r)
	case *syntax.PatternExpr:
		return ex.patternExpr(e, r)
	case *syntax.SubqueryExpr:
		return ex.subquery(e, r)
	case *syntax.MapProjection:
		return ex.mapProjection(e, r)
	}
	return nil, unsupported("expression %T", e)
}

// ─── property access, subscripts ─────────────────────────────────────────────

func (ex *exec) property(subject any, key string) (any, error) {
	switch x := subject.(type) {
	case nil:
		return nil, nil
	case *Node:
		if x.Deleted {
			return nil, deletedAccess()
		}
		return x.Props[key], nil
	case *Rel:
		if x.Deleted {
			return nil, deletedAccess()
		}
		return x.Props[key], nil
	case map[string]any:
		return x[key], nil
	case temporal.Value:
		if v, ok := temporal.Property(x, key); ok {
			return v, nil
		}
		return nil, argErr("%s has no component `%s`", temporalTypeName(x), key)
	case spatial.Point:
		if v, ok := x.Property(key); ok {
			return v, nil
		}
		return nil, argErr("a point has no component `%s`", key)
	}
	return nil, typeErr("cannot access property `%s` of %s", key, typeName(subject))
}

func (ex *exec) subscript(e *syntax.Subscript, r row) (any, error) {
	subject, err := ex.eval(e.Subject, r)
	if err != nil {
		return nil, err
	}
	index, err := ex.eval(e.Index, r)
	if err != nil {
		return nil, err
	}
	switch x := subject.(type) {
	case nil:
		return nil, nil
	case []any:
		if index == nil {
			return nil, nil
		}
		i, ok := index.(int64)
		if !ok {
			return nil, typeErr("list index must be an integer, got %s", typeName(index))
		}
		if i < 0 {
			i += int64(len(x))
		}
		if i < 0 || i >= int64(len(x)) {
			return nil, nil
		}
		return x[i], nil
	case map[string]any:
		if index == nil {
			return nil, nil
		}
		k, ok := index.(string)
		if !ok {
			return nil, errorf("TypeError", "MapElementAccessByNonString", "map keys must be strings, got %s", typeName(index))
		}
		return x[k], nil
	case *Node, *Rel:
		if index == nil {
			return nil, nil
		}
		k, ok := index.(string)
		if !ok {
			return nil, errorf("TypeError", "MapElementAccessByNonString", "property keys must be strings, got %s", typeName(index))
		}
		return ex.property(subject, k)
	}
	return nil, typeErr("cannot index into %s", typeName(subject))
}

func (ex *exec) slice(e *syntax.Slice, r row) (any, error) {
	subject, err := ex.eval(e.Subject, r)
	if err != nil {
		return nil, err
	}
	var from, to any
	if e.From != nil {
		if from, err = ex.eval(e.From, r); err != nil {
			return nil, err
		}
	}
	if e.To != nil {
		if to, err = ex.eval(e.To, r); err != nil {
			return nil, err
		}
	}
	if subject == nil || (e.From != nil && from == nil) || (e.To != nil && to == nil) {
		return nil, nil
	}
	list, ok := subject.([]any)
	if !ok {
		return nil, typeErr("cannot slice %s", typeName(subject))
	}
	n := int64(len(list))
	lo, hi := int64(0), n
	if e.From != nil {
		i, ok := sliceBound(from)
		if !ok {
			return nil, typeErr("slice bounds must be integers")
		}
		lo = i
	}
	if e.To != nil {
		i, ok := sliceBound(to)
		if !ok {
			return nil, typeErr("slice bounds must be integers")
		}
		hi = i
	}
	if lo < 0 {
		lo += n
	}
	if hi < 0 {
		hi += n
	}
	lo, hi = clamp(lo, 0, n), clamp(hi, 0, n)
	if lo >= hi {
		return []any{}, nil
	}
	return append([]any{}, list[lo:hi]...), nil
}

// sliceBound is a slice bound as an integer. A whole-valued float is accepted:
// HTTP clients that cannot tell an integer from a float (Neo4j Bloom through
// Neo4j Desktop sends its item limit as a Float) pass such values as parameters.
func sliceBound(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), true
		}
	}
	return 0, false
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ─── operators ───────────────────────────────────────────────────────────────

func asTri(v any, op string) (tri, error) {
	switch x := v.(type) {
	case nil:
		return triNull, nil
	case bool:
		return triOf(x), nil
	}
	return triNull, typeErr("%s expects a boolean, got %s", op, typeName(v))
}

func (ex *exec) unary(e *syntax.Unary, r row) (any, error) {
	v, err := ex.eval(e.X, r)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case syntax.OpNot:
		t, err := asTri(v, "NOT")
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return false, nil
		case triFalse:
			return true, nil
		}
		return nil, nil
	case syntax.OpPlus:
		if v == nil || isNumber(v) {
			return v, nil
		}
		return nil, typeErr("unary + expects a number, got %s", typeName(v))
	case syntax.OpMinus:
		switch x := v.(type) {
		case nil:
			return nil, nil
		case int64:
			if x == math.MinInt64 {
				return nil, intOverflow()
			}
			return -x, nil
		case float64:
			return -x, nil
		}
		return nil, typeErr("unary - expects a number, got %s", typeName(v))
	}
	return nil, unsupported("unary %s", e.Op)
}

func intOverflow() error {
	return errorf("ArithmeticError", "IntegerOverflow", "integer overflow")
}

func (ex *exec) binary(e *syntax.Binary, r row) (any, error) {
	switch e.Op {
	case syntax.OpAnd, syntax.OpOr:
		return ex.logic(e, r)
	case syntax.OpXor:
		l, err := ex.eval(e.L, r)
		if err != nil {
			return nil, err
		}
		rv, err := ex.eval(e.R, r)
		if err != nil {
			return nil, err
		}
		lt, err := asTri(l, "XOR")
		if err != nil {
			return nil, err
		}
		rt, err := asTri(rv, "XOR")
		if err != nil {
			return nil, err
		}
		if lt == triNull || rt == triNull {
			return nil, nil
		}
		return lt != rt, nil
	}
	l, err := ex.eval(e.L, r)
	if err != nil {
		return nil, err
	}
	rv, err := ex.eval(e.R, r)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case syntax.OpAdd:
		return add(l, rv)
	case syntax.OpSub, syntax.OpMul, syntax.OpDiv, syntax.OpMod, syntax.OpPow:
		return arith(e.Op, l, rv)
	case syntax.OpConcat:
		if l == nil || rv == nil {
			return nil, nil
		}
		if err := checkConcat(l, rv); err != nil {
			return nil, err
		}
		if ls, ok := l.(string); ok {
			if rs, ok := rv.(string); ok {
				return ls + rs, nil
			}
		}
		if ll, ok := l.([]any); ok {
			if rl, ok := rv.([]any); ok {
				return append(append([]any{}, ll...), rl...), nil
			}
		}
		return nil, typeErr("|| expects two strings or two lists")
	case syntax.OpIn:
		return in(l, rv)
	case syntax.OpStartsWith, syntax.OpEndsWith, syntax.OpContains:
		if l == nil || rv == nil {
			return nil, nil
		}
		ls, lok := l.(string)
		rs, rok := rv.(string)
		if !lok || !rok {
			return nil, nil
		}
		switch e.Op {
		case syntax.OpStartsWith:
			return strings.HasPrefix(ls, rs), nil
		case syntax.OpEndsWith:
			return strings.HasSuffix(ls, rs), nil
		}
		return strings.Contains(ls, rs), nil
	case syntax.OpRegexMatch:
		if l == nil || rv == nil {
			return nil, nil
		}
		ls, lok := l.(string)
		rs, rok := rv.(string)
		if !lok || !rok {
			return nil, typeErr("=~ expects strings")
		}
		re, err := ex.regex(rs)
		if err != nil {
			return nil, err
		}
		return re.MatchString(ls), nil
	}
	return nil, unsupported("operator %s", e.Op)
}

// logic evaluates AND/OR with three-valued logic. An operand that decides the
// result short-circuits the other.
func (ex *exec) logic(e *syntax.Binary, r row) (any, error) {
	isAnd := e.Op == syntax.OpAnd
	name := string(e.Op)
	l, err := ex.eval(e.L, r)
	if err != nil {
		return nil, err
	}
	lt, err := asTri(l, name)
	if err != nil {
		return nil, err
	}
	if isAnd && lt == triFalse {
		return false, nil
	}
	if !isAnd && lt == triTrue {
		return true, nil
	}
	rv, err := ex.eval(e.R, r)
	if err != nil {
		return nil, err
	}
	rt, err := asTri(rv, name)
	if err != nil {
		return nil, err
	}
	if isAnd {
		switch {
		case rt == triFalse:
			return false, nil
		case lt == triNull || rt == triNull:
			return nil, nil
		}
		return true, nil
	}
	switch {
	case rt == triTrue:
		return true, nil
	case lt == triNull || rt == triNull:
		return nil, nil
	}
	return false, nil
}

// maxStringBytes bounds a string the interpreter builds by concatenation or
// replacement; with maxListLen it keeps a statement that doubles a value in a
// loop (reduce(s = 'x', i IN range(1, 40) | s + s)) from exhausting memory,
// which is fatal rather than an error.
const maxStringBytes = 64 << 20

// checkConcat fails when joining l and r would exceed the size limits.
func checkConcat(l, r any) error {
	size := func(v any) (strBytes, listLen int) {
		switch x := v.(type) {
		case string:
			return len(x), 0
		case []any:
			return 0, len(x)
		}
		return 0, 0
	}
	ls, ll := size(l)
	rs, rl := size(r)
	if ls+rs > maxStringBytes {
		return errorf("ArgumentError", "ValueTooLarge", "string longer than the limit of %d bytes", maxStringBytes)
	}
	if ll+rl > maxListLen {
		return errListTooLong()
	}
	return nil
}

func add(l, r any) (any, error) {
	if err := checkConcat(l, r); err != nil {
		return nil, err
	}
	if l == nil || r == nil {
		return nil, nil
	}
	if v, ok, err := temporalArith("+", l, r); ok {
		return v, err
	}
	switch x := l.(type) {
	case int64:
		switch y := r.(type) {
		case int64:
			s := x + y
			if (x > 0 && y > 0 && s < 0) || (x < 0 && y < 0 && s >= 0) {
				return nil, intOverflow()
			}
			return s, nil
		case float64:
			return float64(x) + y, nil
		case string:
			return strconv.FormatInt(x, 10) + y, nil
		case []any:
			return append([]any{x}, y...), nil
		}
	case float64:
		switch y := r.(type) {
		case int64:
			return x + float64(y), nil
		case float64:
			return x + y, nil
		case string:
			return formatFloat(x) + y, nil
		case []any:
			return append([]any{x}, y...), nil
		}
	case string:
		switch y := r.(type) {
		case string:
			return x + y, nil
		case int64:
			return x + strconv.FormatInt(y, 10), nil
		case float64:
			return x + formatFloat(y), nil
		case bool:
			return x + strconv.FormatBool(y), nil
		case []any:
			return append([]any{x}, y...), nil
		}
	case bool:
		if y, ok := r.(string); ok {
			return strconv.FormatBool(x) + y, nil
		}
		if y, ok := r.([]any); ok {
			return append([]any{x}, y...), nil
		}
	case []any:
		switch y := r.(type) {
		case []any:
			return append(append([]any{}, x...), y...), nil
		default:
			return append(append([]any{}, x...), r), nil
		}
	}
	if y, ok := r.([]any); ok {
		return append([]any{l}, y...), nil
	}
	return nil, typeErr("cannot add %s and %s", typeName(l), typeName(r))
}

func arith(op syntax.BinaryOp, l, r any) (any, error) {
	if l == nil || r == nil {
		return nil, nil
	}
	if v, ok, err := temporalArith(string(op), l, r); ok {
		return v, err
	}
	if !isNumber(l) || !isNumber(r) {
		return nil, typeErr("operator %s expects numbers, got %s and %s", op, typeName(l), typeName(r))
	}
	xi, lInt := l.(int64)
	yi, rInt := r.(int64)
	if lInt && rInt && op != syntax.OpPow {
		switch op {
		case syntax.OpSub:
			d := xi - yi
			if (xi >= 0 && yi < 0 && d < 0) || (xi < 0 && yi > 0 && d >= 0) {
				return nil, intOverflow()
			}
			return d, nil
		case syntax.OpMul:
			if xi == 0 || yi == 0 {
				return int64(0), nil
			}
			p := xi * yi
			if p/yi != xi || (xi == math.MinInt64 && yi == -1) || (yi == math.MinInt64 && xi == -1) {
				return nil, intOverflow()
			}
			return p, nil
		case syntax.OpDiv:
			if yi == 0 {
				return nil, errorf("ArithmeticError", "DivisionByZero", "/ by zero")
			}
			if xi == math.MinInt64 && yi == -1 {
				return nil, intOverflow()
			}
			return xi / yi, nil
		case syntax.OpMod:
			if yi == 0 {
				return nil, errorf("ArithmeticError", "DivisionByZero", "/ by zero")
			}
			if yi == -1 {
				return int64(0), nil
			}
			return xi % yi, nil
		}
	}
	x, y := toFloat(l), toFloat(r)
	switch op {
	case syntax.OpSub:
		return x - y, nil
	case syntax.OpMul:
		return x * y, nil
	case syntax.OpDiv:
		return x / y, nil
	case syntax.OpMod:
		return math.Mod(x, y), nil
	case syntax.OpPow:
		return math.Pow(x, y), nil
	}
	return nil, unsupported("operator %s", op)
}

func in(l, r any) (any, error) {
	if r == nil {
		return nil, nil
	}
	list, ok := r.([]any)
	if !ok {
		return nil, typeErr("IN expects a list, got %s", typeName(r))
	}
	sawNull := false
	for _, el := range list {
		switch equals(l, el) {
		case triTrue:
			return true, nil
		case triNull:
			sawNull = true
		}
	}
	if sawNull {
		return nil, nil
	}
	return false, nil
}

func (ex *exec) comparison(e *syntax.Comparison, r row) (any, error) {
	vals := make([]any, len(e.Operands))
	for i, o := range e.Operands {
		v, err := ex.eval(o, r)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	result := triTrue
	for i, op := range e.Ops {
		t := compareOp(op, vals[i], vals[i+1])
		switch t {
		case triFalse:
			return false, nil
		case triNull:
			result = triNull
		}
	}
	return result.value(), nil
}

func compareOp(op syntax.CompareOp, a, b any) tri {
	switch op {
	case syntax.CmpEq:
		return equals(a, b)
	case syntax.CmpNeq:
		switch equals(a, b) {
		case triTrue:
			return triFalse
		case triFalse:
			return triTrue
		}
		return triNull
	}
	if isNaNValue(a) && isNumber(b) || isNaNValue(b) && isNumber(a) {
		return triFalse // NaN is unordered: every ordering comparison is false
	}
	c, ok := compare(a, b)
	if !ok {
		return triNull
	}
	switch op {
	case syntax.CmpLt:
		return triOf(c < 0)
	case syntax.CmpLte:
		return triOf(c <= 0)
	case syntax.CmpGt:
		return triOf(c > 0)
	case syntax.CmpGte:
		return triOf(c >= 0)
	}
	return triNull
}

func isNaNValue(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

// maxRegexCache bounds the compiled patterns kept for one statement.
const maxRegexCache = 256

func (ex *exec) regex(pattern string) (*regexp.Regexp, error) {
	if re, ok := ex.regexCache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return nil, errorf("ArgumentError", "InvalidArgumentValue", "invalid regular expression %q: %v", pattern, err)
	}
	if len(ex.regexCache) >= maxRegexCache {
		return re, nil // a pattern that changes per row is not worth keeping
	}
	if ex.regexCache == nil {
		ex.regexCache = map[string]*regexp.Regexp{}
	}
	ex.regexCache[pattern] = re
	return re, nil
}

func (ex *exec) hasLabels(e *syntax.HasLabels, r row) (any, error) {
	v, err := ex.eval(e.X, r)
	if err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case *Node:
		return ex.matchLabelExpr(e.Labels, x.Labels, r)
	case *Rel:
		return ex.matchLabelExpr(e.Labels, []string{x.Type}, r)
	}
	return nil, typeErr("cannot check labels of %s", typeName(v))
}

// matchLabelExpr evaluates a label/type expression against a set of labels.
func (ex *exec) matchLabelExpr(le syntax.LabelExpr, labels []string, r row) (bool, error) {
	switch le := le.(type) {
	case nil:
		return true, nil
	case *syntax.LabelName:
		name := le.Name
		if le.Dynamic != nil {
			v, err := ex.eval(le.Dynamic, r)
			if err != nil {
				return false, err
			}
			switch x := v.(type) {
			case string:
				name = x
			case []any:
				for _, el := range x {
					s, ok := el.(string)
					if !ok {
						return false, typeErr("dynamic labels must be strings")
					}
					if !contains(labels, s) {
						return false, nil
					}
				}
				return true, nil
			default:
				return false, typeErr("dynamic labels must be strings")
			}
		}
		return contains(labels, name), nil
	case *syntax.LabelWildcard:
		return len(labels) > 0, nil
	case *syntax.LabelNot:
		ok, err := ex.matchLabelExpr(le.X, labels, r)
		return !ok, err
	case *syntax.LabelAnd:
		ok, err := ex.matchLabelExpr(le.L, labels, r)
		if err != nil || !ok {
			return false, err
		}
		return ex.matchLabelExpr(le.R, labels, r)
	case *syntax.LabelOr:
		ok, err := ex.matchLabelExpr(le.L, labels, r)
		if err != nil || ok {
			return ok, err
		}
		return ex.matchLabelExpr(le.R, labels, r)
	}
	return false, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ─── CASE, comprehensions, quantifiers ───────────────────────────────────────

func (ex *exec) caseExpr(e *syntax.Case, r row) (any, error) {
	var subject any
	if e.Subject != nil {
		var err error
		if subject, err = ex.eval(e.Subject, r); err != nil {
			return nil, err
		}
	}
	for _, w := range e.Whens {
		cond, err := ex.eval(w.Cond, r)
		if err != nil {
			return nil, err
		}
		matched := false
		if e.Subject != nil {
			matched = equals(subject, cond) == triTrue
		} else {
			matched = cond == true
		}
		if matched {
			return ex.eval(w.Then, r)
		}
	}
	if e.Else != nil {
		return ex.eval(e.Else, r)
	}
	return nil, nil
}

// list evaluates e and requires a list (or null).
func (ex *exec) list(e syntax.Expr, r row, what string) ([]any, bool, error) {
	v, err := ex.eval(e, r)
	if err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, false, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, false, typeErr("%s expects a list, got %s", what, typeName(v))
	}
	return l, true, nil
}

func (ex *exec) listComp(e *syntax.ListComp, r row) (any, error) {
	list, ok, err := ex.list(e.In, r, "list comprehension")
	if err != nil || !ok {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, el := range list {
		inner := r.with(e.Var, el)
		if e.Where != nil {
			c, err := ex.eval(e.Where, inner)
			if err != nil {
				return nil, err
			}
			if c != true {
				continue
			}
		}
		if e.Proj == nil {
			out = append(out, el)
			continue
		}
		v, err := ex.eval(e.Proj, inner)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (ex *exec) quantifier(e *syntax.Quantifier, r row) (any, error) {
	list, ok, err := ex.list(e.In, r, string(e.Kind)+"()")
	if err != nil || !ok {
		return nil, err
	}
	var trues, falses, nulls int
	for _, el := range list {
		inner := r.with(e.Var, el)
		if e.Where == nil {
			trues++
			continue
		}
		c, err := ex.eval(e.Where, inner)
		if err != nil {
			return nil, err
		}
		switch c {
		case true:
			trues++
		case false:
			falses++
		case nil:
			nulls++
		default:
			return nil, typeErr("%s() predicate must be a boolean, got %s", e.Kind, typeName(c))
		}
	}
	switch e.Kind {
	case syntax.QuantAll:
		switch {
		case falses > 0:
			return false, nil
		case nulls > 0:
			return nil, nil
		}
		return true, nil
	case syntax.QuantAny:
		switch {
		case trues > 0:
			return true, nil
		case nulls > 0:
			return nil, nil
		}
		return false, nil
	case syntax.QuantNone:
		switch {
		case trues > 0:
			return false, nil
		case nulls > 0:
			return nil, nil
		}
		return true, nil
	case syntax.QuantSingle:
		switch {
		case trues > 1:
			return false, nil
		case trues == 1 && nulls == 0:
			return true, nil
		case nulls > 0:
			return nil, nil
		}
		return false, nil
	}
	return nil, unsupported("quantifier %s", e.Kind)
}

func (ex *exec) reduce(e *syntax.Reduce, r row) (any, error) {
	acc, err := ex.eval(e.Init, r)
	if err != nil {
		return nil, err
	}
	list, ok, err := ex.list(e.In, r, "reduce()")
	if err != nil || !ok {
		return nil, err
	}
	for _, el := range list {
		inner := r.with(e.Acc, acc).with(e.Var, el)
		if acc, err = ex.eval(e.Expr, inner); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func (ex *exec) mapProjection(e *syntax.MapProjection, r row) (any, error) {
	subject, err := ex.eval(e.Subject, r)
	if err != nil {
		return nil, err
	}
	if subject == nil {
		return nil, nil
	}
	out := map[string]any{}
	for _, it := range e.Items {
		switch it.Kind {
		case syntax.ProjProperty:
			v, err := ex.property(subject, it.Key)
			if err != nil {
				return nil, err
			}
			out[it.Key] = v
		case syntax.ProjLiteral:
			v, err := ex.eval(it.Value, r)
			if err != nil {
				return nil, err
			}
			out[it.Key] = v
		case syntax.ProjVariable:
			v, ok := r[it.Key]
			if !ok {
				return nil, errorf("SyntaxError", "UndefinedVariable", "variable `%s` not defined", it.Key)
			}
			out[it.Key] = v
		case syntax.ProjAll:
			switch x := subject.(type) {
			case *Node:
				for k, v := range x.Props {
					out[k] = v
				}
			case *Rel:
				for k, v := range x.Props {
					out[k] = v
				}
			case map[string]any:
				for k, v := range x {
					out[k] = v
				}
			default:
				return nil, typeErr("cannot project %s", typeName(subject))
			}
		}
	}
	return out, nil
}

// formatFloat renders a float the way Cypher (Java) does: 1.0, 0.5, 1.0E10.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	abs := math.Abs(f)
	if abs >= 1e-3 && abs < 1e7 {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	s := strconv.FormatFloat(f, 'E', -1, 64) // 1.5E+10
	mant, exp, _ := strings.Cut(s, "E")
	if !strings.Contains(mant, ".") {
		mant += ".0"
	}
	exp = strings.TrimPrefix(exp, "+")
	neg := strings.HasPrefix(exp, "-")
	exp = strings.TrimLeft(strings.TrimPrefix(exp, "-"), "0")
	if exp == "" {
		exp = "0"
	}
	if neg {
		exp = "-" + exp
	}
	return mant + "E" + exp
}
