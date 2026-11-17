package interp

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/graingraph/graingraph/cypher/spatial"
	"github.com/graingraph/graingraph/cypher/syntax"
	"github.com/graingraph/graingraph/cypher/temporal"
	"github.com/graingraph/graingraph/cypher/vector"
)

// aggregates are the aggregating function names (lower case).
var aggregates = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true, "collect": true,
	"percentilecont": true, "percentiledisc": true, "stdev": true, "stdevp": true,
}

func isAggregate(f *syntax.FuncCall) bool {
	return len(f.Namespace) == 0 && aggregates[strings.ToLower(f.Name)]
}

// call evaluates a function call. Aggregate calls are replaced by their
// precomputed value while a projection item is evaluated.
func (ex *exec) call(e *syntax.FuncCall, r row) (any, error) {
	if isAggregate(e) {
		if v, ok := ex.agg[e]; ok {
			return v, nil
		}
		return nil, errorf("SyntaxError", "InvalidAggregation", "invalid use of aggregate function %s", e.Name)
	}
	name := strings.ToLower(e.Name)
	if len(e.Namespace) == 0 && (name == "linenumber" || name == "file") && len(e.Args) == 0 {
		if v, ok := r[map[string]string{"linenumber": csvLineKey, "file": csvFileKey}[name]]; ok {
			return v, nil
		}
		return nil, nil
	}
	if len(e.Namespace) > 0 {
		args, err := ex.evalArgs(e, r)
		if err != nil {
			return nil, err
		}
		if len(e.Namespace) == 1 {
			if v, ok, err := ex.namespacedTemporal(e.Namespace[0], e.Name, args); ok {
				return v, err
			}
		}
		if v, ok, err := spatialFunction(e.Namespace, e.Name, args); ok {
			return v, err
		}
		if v, ok, err := vectorFunction(e.Namespace, e.Name, args); ok {
			return v, err
		}
		return nil, unsupported("function %s.%s", strings.Join(e.Namespace, "."), e.Name)
	}
	// exists() accepts a property or a pattern; evaluate its argument specially.
	if name == "exists" && len(e.Args) == 1 {
		v, err := ex.eval(e.Args[0], r)
		if err != nil {
			return nil, err
		}
		if _, isProp := e.Args[0].(*syntax.Property); isProp {
			return v != nil, nil
		}
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return v != nil, nil
	}
	args, err := ex.evalArgs(e, r)
	if err != nil {
		return nil, err
	}
	if v, ok, err := ex.temporalFunction(name, args); ok {
		return v, err
	}
	if v, ok, err := spatialFunction(nil, name, args); ok {
		return v, err
	}
	if v, ok, err := vectorFunction(nil, name, args); ok {
		return v, err
	}
	return ex.builtin(name, args)
}

func (ex *exec) evalArgs(e *syntax.FuncCall, r row) ([]any, error) {
	args := make([]any, len(e.Args))
	for i, a := range e.Args {
		v, err := ex.eval(a, r)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return args, nil
}

func argc(name string, args []any, min, max int) error {
	if len(args) < min || (max >= 0 && len(args) > max) {
		return errorf("SyntaxError", "InvalidNumberOfArguments", "function %s() was called with %d arguments", name, len(args))
	}
	return nil
}

func anyNil(args []any) bool {
	for _, a := range args {
		if a == nil {
			return true
		}
	}
	return false
}

func (ex *exec) builtin(name string, args []any) (any, error) {
	switch name {
	case "coalesce":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "nullif":
		if err := argc(name, args, 2, 2); err != nil {
			return nil, err
		}
		if equals(args[0], args[1]) == triTrue {
			return nil, nil
		}
		return args[0], nil
	case "timestamp":
		return time.Now().UnixMilli(), nil
	case "rand":
		return rand01(), nil
	case "e":
		return math.E, nil
	case "pi":
		return math.Pi, nil
	case "randomuuid":
		return randomUUID(), nil
	}
	if needsArg[name] && len(args) == 0 {
		return nil, errorf("SyntaxError", "InvalidNumberOfArguments", "%s() requires an argument", name)
	}
	// All remaining functions return null for a null argument.
	if len(args) > 0 && args[0] == nil {
		switch name {
		case "isempty", "valuetype":
		default:
			return nil, nil
		}
	}
	switch name {
	case "size":
		if err := argc(name, args, 1, 1); err != nil {
			return nil, err
		}
		switch x := args[0].(type) {
		case []any:
			return int64(len(x)), nil
		case string:
			return int64(utf8.RuneCountInString(x)), nil
		}
		return nil, typeErr("size() expects a list or string, got %s", typeName(args[0]))
	case "length":
		if err := argc(name, args, 1, 1); err != nil {
			return nil, err
		}
		if p, ok := args[0].(*Path); ok {
			return int64(len(p.Rels)), nil
		}
		return nil, typeErr("length() expects a path, got %s", typeName(args[0]))
	case "char_length", "character_length":
		if s, ok := args[0].(string); ok {
			return int64(utf8.RuneCountInString(s)), nil
		}
		return nil, typeErr("%s() expects a string, got %s", name, typeName(args[0]))
	case "head", "last", "tail":
		l, ok := args[0].([]any)
		if !ok {
			return nil, typeErr("%s() expects a list, got %s", name, typeName(args[0]))
		}
		switch name {
		case "head":
			if len(l) == 0 {
				return nil, nil
			}
			return l[0], nil
		case "last":
			if len(l) == 0 {
				return nil, nil
			}
			return l[len(l)-1], nil
		}
		if len(l) == 0 {
			return []any{}, nil
		}
		return append([]any{}, l[1:]...), nil
	case "reverse":
		switch x := args[0].(type) {
		case string:
			rs := []rune(x)
			for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
				rs[i], rs[j] = rs[j], rs[i]
			}
			return string(rs), nil
		case []any:
			out := make([]any, len(x))
			for i, v := range x {
				out[len(x)-1-i] = v
			}
			return out, nil
		}
		return nil, typeErr("reverse() expects a list or string, got %s", typeName(args[0]))
	case "range":
		return fnRange(args)
	case "isempty":
		switch x := args[0].(type) {
		case nil:
			return nil, nil
		case string:
			return x == "", nil
		case []any:
			return len(x) == 0, nil
		case map[string]any:
			return len(x) == 0, nil
		}
		return nil, typeErr("isEmpty() expects a list, map or string, got %s", typeName(args[0]))
	case "keys":
		switch x := args[0].(type) {
		case *Node:
			if x.Deleted {
				return nil, deletedAccess()
			}
			return keysOf(x.Props), nil
		case *Rel:
			if x.Deleted {
				return nil, deletedAccess()
			}
			return keysOf(x.Props), nil
		case map[string]any:
			return keysOf(x), nil
		}
		return nil, typeErr("keys() expects a node, relationship or map, got %s", typeName(args[0]))
	case "properties":
		switch x := args[0].(type) {
		case *Node:
			if x.Deleted {
				return nil, deletedAccess()
			}
			return copyMap(x.Props), nil
		case *Rel:
			if x.Deleted {
				return nil, deletedAccess()
			}
			return copyMap(x.Props), nil
		case map[string]any:
			return copyMap(x), nil
		}
		return nil, typeErr("properties() expects a node, relationship or map, got %s", typeName(args[0]))
	case "labels":
		n, ok := args[0].(*Node)
		if !ok {
			return nil, typeErr("labels() expects a node, got %s", typeName(args[0]))
		}
		if n.Deleted {
			return nil, deletedAccess()
		}
		out := make([]any, len(n.Labels))
		for i, l := range n.Labels {
			out[i] = l
		}
		return out, nil
	case "type":
		r, ok := args[0].(*Rel)
		if !ok {
			return nil, typeErr("type() expects a relationship, got %s", typeName(args[0]))
		}
		return r.Type, nil
	case "id":
		switch x := args[0].(type) {
		case *Node:
			return x.ID, nil
		case *Rel:
			return x.ID, nil
		}
		return nil, typeErr("id() expects a node or relationship, got %s", typeName(args[0]))
	case "elementid":
		switch x := args[0].(type) {
		case *Node:
			return strconv.FormatInt(x.ID, 10), nil
		case *Rel:
			return strconv.FormatInt(x.ID, 10), nil
		}
		return nil, typeErr("elementId() expects a node or relationship, got %s", typeName(args[0]))
	case "nodes":
		p, ok := args[0].(*Path)
		if !ok {
			return nil, typeErr("nodes() expects a path, got %s", typeName(args[0]))
		}
		out := make([]any, len(p.Nodes))
		for i, n := range p.Nodes {
			out[i] = n
		}
		return out, nil
	case "relationships", "rels":
		p, ok := args[0].(*Path)
		if !ok {
			return nil, typeErr("relationships() expects a path, got %s", typeName(args[0]))
		}
		out := make([]any, len(p.Rels))
		for i, rel := range p.Rels {
			out[i] = rel
		}
		return out, nil
	case "startnode", "endnode":
		rel, ok := args[0].(*Rel)
		if !ok {
			return nil, typeErr("%s() expects a relationship, got %s", name, typeName(args[0]))
		}
		id := rel.Start
		if name == "endnode" {
			id = rel.End
		}
		return ex.g.node(id)
	case "tointeger", "tointegerornull":
		return toInteger(args[0], strings.HasSuffix(name, "ornull"))
	case "tofloat", "tofloatornull":
		return toFloatFn(args[0], strings.HasSuffix(name, "ornull"))
	case "toboolean", "tobooleanornull":
		return toBooleanFn(args[0], strings.HasSuffix(name, "ornull"))
	case "tostring", "tostringornull":
		return toStringFn(args[0], strings.HasSuffix(name, "ornull"))
	case "tointegerlist", "tofloatlist", "tobooleanlist", "tostringlist":
		l, ok := args[0].([]any)
		if !ok {
			return nil, typeErr("%s() expects a list, got %s", name, typeName(args[0]))
		}
		inner := strings.TrimSuffix(name, "list")
		out := make([]any, len(l))
		for i, el := range l {
			v, err := ex.builtin(inner+"ornull", []any{el})
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil

	// mathematical
	case "abs":
		switch x := args[0].(type) {
		case int64:
			if x == math.MinInt64 {
				return nil, intOverflow()
			}
			if x < 0 {
				return -x, nil
			}
			return x, nil
		case float64:
			return math.Abs(x), nil
		}
		return nil, typeErr("abs() expects a number, got %s", typeName(args[0]))
	case "sign":
		switch x := args[0].(type) {
		case int64:
			return int64(cmpInt(x, 0)), nil
		case float64:
			switch {
			case math.IsNaN(x):
				return nil, nil
			case x > 0:
				return int64(1), nil
			case x < 0:
				return int64(-1), nil
			}
			return int64(0), nil
		}
		return nil, typeErr("sign() expects a number, got %s", typeName(args[0]))
	case "ceil", "floor", "sqrt", "exp", "log", "log10", "sin", "cos", "tan", "cot", "asin", "acos", "atan",
		"degrees", "radians", "haversin", "sinh", "cosh", "tanh", "coth":
		if !isNumber(args[0]) {
			return nil, typeErr("%s() expects a number, got %s", name, typeName(args[0]))
		}
		return mathFn(name, toFloat(args[0])), nil
	case "round":
		return fnRound(args)
	case "atan2":
		if err := argc(name, args, 2, 2); err != nil {
			return nil, err
		}
		if args[1] == nil {
			return nil, nil
		}
		if !isNumber(args[0]) || !isNumber(args[1]) {
			return nil, typeErr("atan2() expects numbers")
		}
		return math.Atan2(toFloat(args[0]), toFloat(args[1])), nil

	// string
	case "tolower", "lower":
		return stringArg1(name, args, strings.ToLower)
	case "toupper", "upper":
		return stringArg1(name, args, strings.ToUpper)
	case "trim", "btrim", "ltrim", "rtrim":
		return fnTrim(name, args)
	case "normalize":
		if err := argc(name, args, 1, 2); err != nil {
			return nil, err
		}
		if args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, typeErr("normalize() expects a string, got %s", typeName(args[0]))
		}
		formName := "NFC"
		if len(args) == 2 {
			f, ok := args[1].(string)
			if !ok {
				return nil, typeErr("normalize() form must be NFC, NFD, NFKC or NFKD")
			}
			formName = f
		}
		form, err := normForm(formName)
		if err != nil {
			return nil, err
		}
		return form.String(s), nil
	case "isnan":
		if err := argc(name, args, 1, 1); err != nil {
			return nil, err
		}
		switch x := args[0].(type) {
		case nil:
			return nil, nil
		case float64:
			return math.IsNaN(x), nil
		case int64:
			return false, nil
		}
		return nil, typeErr("isNaN() expects a number, got %s", typeName(args[0]))
	case "valuetype":
		if err := argc(name, args, 1, 1); err != nil {
			return nil, err
		}
		return valueTypeName(args[0], args[0] != nil), nil
	case "replace":
		if err := argc(name, args, 3, 3); err != nil {
			return nil, err
		}
		if args[1] == nil || args[2] == nil {
			return nil, nil
		}
		s, ok1 := args[0].(string)
		a, ok2 := args[1].(string)
		b, ok3 := args[2].(string)
		if !ok1 || !ok2 || !ok3 {
			return nil, typeErr("replace() expects strings")
		}
		n := strings.Count(s, a) // for an empty a: runes + 1
		if int64(len(s))+int64(n)*int64(len(b)-len(a)) > maxStringBytes {
			return nil, errorf("ArgumentError", "ValueTooLarge", "replace() would produce a string longer than the limit of %d bytes", maxStringBytes)
		}
		return strings.ReplaceAll(s, a, b), nil
	case "substring":
		return fnSubstring(args)
	case "left", "right":
		if err := argc(name, args, 2, 2); err != nil {
			return nil, err
		}
		if args[1] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		n, ok2 := args[1].(int64)
		if !ok || !ok2 {
			return nil, typeErr("%s() expects a string and an integer", name)
		}
		if n < 0 {
			return nil, errorf("SyntaxError", "NegativeIntegerArgument", "%s() length must not be negative", name)
		}
		rs := []rune(s)
		if n > int64(len(rs)) {
			n = int64(len(rs))
		}
		if name == "left" {
			return string(rs[:n]), nil
		}
		return string(rs[int64(len(rs))-n:]), nil
	case "split":
		if err := argc(name, args, 2, 2); err != nil {
			return nil, err
		}
		if args[1] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, typeErr("split() expects a string, got %s", typeName(args[0]))
		}
		var delims []string
		switch d := args[1].(type) {
		case string:
			delims = []string{d}
		case []any:
			for _, x := range d {
				ds, ok := x.(string)
				if !ok {
					return nil, typeErr("split() delimiters must be strings")
				}
				delims = append(delims, ds)
			}
		default:
			return nil, typeErr("split() expects a string delimiter, got %s", typeName(args[1]))
		}
		return splitMulti(s, delims), nil
	}
	return nil, unsupported("function %s()", name)
}

func keysOf(m map[string]any) []any {
	out := make([]any, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k)
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func stringArg1(name string, args []any, f func(string) string) (any, error) {
	if err := argc(name, args, 1, 1); err != nil {
		return nil, err
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, typeErr("%s() expects a string, got %s", name, typeName(args[0]))
	}
	return f(s), nil
}

func mathFn(name string, x float64) any {
	switch name {
	case "ceil":
		return math.Ceil(x)
	case "floor":
		return math.Floor(x)
	case "sqrt":
		return math.Sqrt(x)
	case "exp":
		return math.Exp(x)
	case "log":
		return math.Log(x)
	case "log10":
		return math.Log10(x)
	case "sin":
		return math.Sin(x)
	case "cos":
		return math.Cos(x)
	case "tan":
		return math.Tan(x)
	case "cot":
		return 1 / math.Tan(x)
	case "asin":
		return math.Asin(x)
	case "acos":
		return math.Acos(x)
	case "atan":
		return math.Atan(x)
	case "degrees":
		return x * 180 / math.Pi
	case "radians":
		return x * math.Pi / 180
	case "haversin":
		return (1 - math.Cos(x)) / 2
	case "sinh":
		return math.Sinh(x)
	case "cosh":
		return math.Cosh(x)
	case "tanh":
		return math.Tanh(x)
	case "coth":
		return 1 / math.Tanh(x)
	}
	return math.NaN()
}

// fnRound implements round(x), round(x, precision) and round(x, precision, mode).
func fnRound(args []any) (any, error) {
	if err := argc("round", args, 1, 3); err != nil {
		return nil, err
	}
	if !isNumber(args[0]) {
		return nil, typeErr("round() expects a number, got %s", typeName(args[0]))
	}
	x := toFloat(args[0])
	if len(args) == 1 {
		return roundHalfUp(x), nil
	}
	if args[1] == nil {
		return nil, nil
	}
	p, ok := args[1].(int64)
	if !ok {
		return nil, typeErr("round() precision must be an integer")
	}
	scale := math.Pow(10, float64(p))
	return roundHalfUp(x*scale) / scale, nil
}

// roundHalfUp rounds half away from zero, as Cypher's round() does.
func roundHalfUp(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	return math.Round(x)
}

// maxListLen bounds a list the interpreter materialises from range(); longer
// ranges can still be walked lazily by UNWIND in a read-only pipeline. It is a
// var so tests can lower it instead of allocating millions of elements.
var maxListLen = 10_000_000

// errListTooLong is the one error every list-building path returns when it
// would exceed maxListLen.
func errListTooLong() error {
	return errorf("ArgumentError", "ValueTooLarge", "list longer than the limit of %d elements", maxListLen)
}

// rangeSpec checks range()'s arguments. null is true when an argument is null.
func rangeSpec(args []any) (start, end, step int64, null bool, err error) {
	if err := argc("range", args, 2, 3); err != nil {
		return 0, 0, 0, false, err
	}
	if anyNil(args) {
		return 0, 0, 0, true, nil
	}
	nums := make([]int64, len(args))
	for i, a := range args {
		n, ok := a.(int64)
		if !ok {
			return 0, 0, 0, false, errorf("ArgumentError", "InvalidArgumentType", "range() expects integers, got %s", typeName(a))
		}
		nums[i] = n
	}
	step = 1
	if len(nums) == 3 {
		step = nums[2]
	}
	if step == 0 {
		return 0, 0, 0, false, errorf("ArgumentError", "NumberOutOfRange", "range() step must not be zero")
	}
	return nums[0], nums[1], step, false, nil
}

// rangeLen is the number of elements range(start, end, step) produces,
// saturating at MaxUint64.
func rangeLen(start, end, step int64) uint64 {
	var diff, st uint64
	switch {
	case step > 0 && end >= start:
		diff, st = uint64(end)-uint64(start), uint64(step)
	case step < 0 && end <= start:
		diff, st = uint64(start)-uint64(end), uint64(-step) // -MinInt64 wraps to 2^63, still right as uint64
	default:
		return 0
	}
	n := diff/st + 1
	if n == 0 {
		return math.MaxUint64
	}
	return n
}

func fnRange(args []any) (any, error) {
	start, end, step, null, err := rangeSpec(args)
	if err != nil || null {
		return nil, err
	}
	n := rangeLen(start, end, step)
	if n > uint64(maxListLen) {
		return nil, errorf("ArgumentError", "NumberOutOfRange", "range() would produce %d elements; the limit is %d", n, maxListLen)
	}
	out := make([]any, 0, n)
	for i, k := start, uint64(0); k < n; i, k = i+step, k+1 {
		out = append(out, i)
	}
	return out, nil
}

// needsArg lists the functions that read args[0] without any other arity check.
var needsArg = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{"char_length", "character_length", "head", "last", "tail", "reverse", "isempty",
		"keys", "properties", "labels", "type", "id", "elementid", "nodes", "relationships", "rels",
		"startnode", "endnode", "tointeger", "tointegerornull", "tofloat", "tofloatornull", "toboolean",
		"tobooleanornull", "tostring", "tostringornull", "tointegerlist", "tofloatlist", "tobooleanlist",
		"tostringlist", "abs", "sign", "ceil", "floor", "sqrt", "exp", "log", "log10", "sin", "cos", "tan",
		"cot", "asin", "acos", "atan", "degrees", "radians", "haversin", "sinh", "cosh", "tanh", "coth"} {
		m[n] = true
	}
	return m
}()

func fnSubstring(args []any) (any, error) {
	if err := argc("substring", args, 2, 3); err != nil {
		return nil, err
	}
	if anyNil(args) {
		return nil, nil
	}
	s, ok := args[0].(string)
	start, ok2 := args[1].(int64)
	if !ok || !ok2 {
		return nil, typeErr("substring() expects a string and integers")
	}
	if start < 0 {
		return nil, errorf("SyntaxError", "NegativeIntegerArgument", "substring() start must not be negative")
	}
	rs := []rune(s)
	if start > int64(len(rs)) {
		return "", nil
	}
	end := int64(len(rs))
	if len(args) == 3 {
		n, ok := args[2].(int64)
		if !ok {
			return nil, typeErr("substring() length must be an integer")
		}
		if n < 0 {
			return nil, errorf("SyntaxError", "NegativeIntegerArgument", "substring() length must not be negative")
		}
		if n < end-start { // compare against the remaining runes: start+n could overflow
			end = start + n
		}
	}
	return string(rs[start:end]), nil
}

func splitMulti(s string, delims []string) []any {
	parts := []string{s}
	for _, d := range delims {
		var next []string
		for _, p := range parts {
			if d == "" {
				next = append(next, strings.Split(p, "")...)
			} else {
				next = append(next, strings.Split(p, d)...)
			}
		}
		parts = next
	}
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out
}

// ─── conversions ─────────────────────────────────────────────────────────────

func toInteger(v any, orNull bool) (any, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || x >= 9.223372036854775807e18 || x < -9.223372036854775808e18 {
			return nil, nil
		}
		return int64(x), nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return int64(f), nil
		}
		return nil, nil
	}
	if orNull {
		return nil, nil
	}
	return nil, argTypeErr("toInteger", v)
}

func toFloatFn(v any, orNull bool) (any, error) {
	switch x := v.(type) {
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f, nil
		}
		return nil, nil
	}
	if orNull {
		return nil, nil
	}
	return nil, argTypeErr("toFloat", v)
}

func toBooleanFn(v any, orNull bool) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, nil
	case int64:
		return x != 0, nil
	}
	if orNull {
		return nil, nil
	}
	return nil, argTypeErr("toBoolean", v)
}

func toStringFn(v any, orNull bool) (any, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return formatFloat(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case temporal.Value:
		return x.String(), nil
	case spatial.Point:
		return x.String(), nil
	case vector.Vector:
		return x.String(), nil
	}
	if orNull {
		return nil, nil
	}
	return nil, argTypeErr("toString", v)
}

func argTypeErr(fn string, v any) error {
	return errorf("TypeError", "InvalidArgumentValue", "%s() does not accept %s", fn, typeName(v))
}
