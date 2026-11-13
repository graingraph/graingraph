package interp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/graingraph/graingraph/cypher/spatial"
	"github.com/graingraph/graingraph/cypher/temporal"
	"github.com/graingraph/graingraph/cypher/vector"
)

// DB is the SQL surface the interpreter needs. *sql.DB, *sql.Tx and the
// store's executor types satisfy it.
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Counters are the side effects of a statement.
type Counters struct {
	NodesCreated         int
	NodesDeleted         int
	RelationshipsCreated int
	RelationshipsDeleted int
	PropertiesSet        int
	PropertiesRemoved    int
	LabelsAdded          int
	LabelsRemoved        int
	// NodeLabelsAdded and NodeLabelsRemoved count label changes on nodes the
	// way Neo4j's query statistics do: one per node and label, at the moment
	// the label is added or removed. Unlike LabelsAdded and LabelsRemoved
	// (which follow the openCypher TCK: a label counts when it first appears
	// in, or disappears from, the graph) they are not netted out over the
	// statement and deleting a node does not count.
	NodeLabelsAdded    int
	NodeLabelsRemoved  int
	IndexesAdded       int
	IndexesRemoved     int
	ConstraintsAdded   int
	ConstraintsRemoved int

	// Events tallies side effects the way Neo4j's query statistics do: as
	// they happen, not netted out over the statement.
	Events EventCounters
}

// EventCounters count each side effect when it happens. Unlike the net
// counters above (which follow the openCypher TCK and compare the graph
// before and after the statement) they do not cancel out: a node created and
// deleted in one statement counts as both, setting a property to the value it
// already has counts, and removing a property counts as a property set, as in
// Neo4j.
type EventCounters struct {
	NodesCreated         int
	NodesDeleted         int
	RelationshipsCreated int
	RelationshipsDeleted int
	// PropertiesSet counts every property assignment, including the removal of
	// an existing property. Deleting a node or relationship does not count its
	// properties.
	PropertiesSet int
}

// graph is the per-statement view of the database. Nodes and relationships are
// cached by id so every reference shares one value; writes go to SQLite
// immediately, deletions are applied when the statement finishes (so a node
// whose relationships are deleted later in the same statement can still be
// deleted).
// entSnap is an existing entity's state before the statement changed it.
// Properties are journalled key by key, so setting one property on each of
// many nodes records one small entry per node, not a copy of all its
// properties. Labels are copied only when a label actually changes.
type entSnap struct {
	changed []propOld      // first state of every property the statement touched
	index   map[string]int // key -> position in changed; built only for large journals
	// labels are the node's labels before its first label change; valid only
	// when labelsSnapped (otherwise the labels are unchanged).
	labels        []string
	labelsSnapped bool
}

// propOld is one property's state before the statement first changed it.
type propOld struct {
	key string
	val any
	had bool
}

// journalIndexAt is the journal length at which lookups switch from a linear
// scan to a map.
const journalIndexAt = 16

// note records key's state before its first change; later changes to the same
// key are ignored.
func (s *entSnap) note(key string, val any, had bool) {
	if s.index != nil {
		if _, ok := s.index[key]; ok {
			return
		}
		s.index[key] = len(s.changed)
	} else {
		for i := range s.changed {
			if s.changed[i].key == key {
				return
			}
		}
		if len(s.changed) >= journalIndexAt {
			s.index = make(map[string]int, 2*len(s.changed))
			for i, o := range s.changed {
				s.index[o.key] = i
			}
			s.index[key] = len(s.changed)
		}
	}
	s.changed = append(s.changed, propOld{key, val, had})
}

// diff counts the properties added and removed between the snapshot and the
// entity's current properties; a changed value counts as one of each.
func (s *entSnap) diff(after map[string]any) (added, removed int) {
	for _, o := range s.changed {
		av, has := after[o.key]
		switch {
		case !o.had && has:
			added++
		case o.had && !has:
			removed++
		case o.had && has && groupKey(o.val) != groupKey(av):
			added++
			removed++
		}
	}
	return
}

// origPropCount is how many properties the entity had before the statement,
// given its current properties.
func (s *entSnap) origPropCount(cur map[string]any) int {
	n := len(cur)
	for _, o := range s.changed {
		_, has := cur[o.key]
		switch {
		case o.had && !has:
			n++
		case !o.had && has:
			n--
		}
	}
	return n
}

// origLabels returns the node's labels before the statement, given its
// current ones.
func (s *entSnap) origLabels(cur []string) []string {
	if s.labelsSnapped {
		return s.labels
	}
	return cur
}

type graph struct {
	eng *Engine // per-database state; may be nil
	ctx context.Context
	// db is what the interpreter reads and writes through; it writes the
	// buffered creates (see batch.go) before every call. raw is the DB itself.
	db  DB
	raw DB

	pendNodes []pendingNode
	pendRels  []pendingRel

	// pendNodeProps / pendRelProps hold entities whose property maps changed and
	// have not yet been written. setEntityProp records the change here instead of
	// an immediate UPDATE, and flushWrites (called on every read and at statement
	// end) writes each entity once as a batched row; see flushNodePropUpdates /
	// flushRelPropUpdates below.
	// The seen sets stop one node with many SET items from being listed twice in
	// the same flush; they are reset when the pending slices are drained, so an
	// entity changed again after that is queued afresh.
	pendNodeProps []*Node
	pendRelProps  []*Rel
	nodePropSeen  map[*Node]bool
	relPropSeen   map[*Rel]bool

	nodes map[int64]*Node
	rels  map[int64]*Rel
	out   map[int64][]*Rel // outgoing relationships by node id, once loaded
	in    map[int64][]*Rel

	delNodes []*Node
	delRels  []*Rel
	detach   map[int64]bool // nodes deleted with DETACH DELETE

	// Net property/label counters are computed when the statement finishes:
	// created entities contribute their final state, existing entities the
	// difference from a snapshot taken before their first change.
	createdNodes map[*Node]bool
	createdRels  map[*Rel]bool
	nodeSnap     map[*Node]*entSnap
	relSnap      map[*Rel]*entSnap

	counters Counters

	// readOnly is set when the statement runs on a read-only connection: it
	// must not write, so schema-dependent maintenance is deferred.
	readOnly bool
	// evictable is set when the whole statement only reads. Nodes and
	// relationships are then never modified, and nothing compares them by
	// pointer (equality, ordering and grouping all go by id), so the caches may
	// be dropped and refilled without changing any result.
	evictable bool
	// labelSets caches the split form of each distinct stored label list.
	labelSets map[string][]string

	// constraints are the schema constraints, loaded on first use.
	constraints       []schemaDef
	constraintsLoaded bool
	vectors           []schemaDef
	vectorLoaded      bool
	vectorCache       bool          // may search the shared vector matrices
	deltas            []VectorDelta // vector index changes for the commit hook
}

func newGraph(ctx context.Context, db DB) *graph {
	g := &graph{
		ctx: ctx, raw: db,
		nodes: map[int64]*Node{}, rels: map[int64]*Rel{},
		out: map[int64][]*Rel{}, in: map[int64][]*Rel{},
		detach:       map[int64]bool{},
		createdNodes: map[*Node]bool{}, createdRels: map[*Rel]bool{},
		nodeSnap: map[*Node]*entSnap{}, relSnap: map[*Rel]*entSnap{},
		nodePropSeen: map[*Node]bool{}, relPropSeen: map[*Rel]bool{},
	}
	g.db = &flushDB{g: g, raw: db}
	return g
}

// ─── property encoding ───────────────────────────────────────────────────────

// checkPropValue verifies that v can be stored as a property: a boolean,
// number or string, or a list of one such type.
func checkPropValue(key string, v any) error {
	switch x := v.(type) {
	case nil, bool, int64, string, temporal.Value, spatial.Point, vector.Vector:
		return nil
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
		return nil
	case []any:
		var first any
		for i, e := range x {
			switch e.(type) {
			case bool, int64, float64, string, temporal.Value, spatial.Point, vector.Vector:
			default:
				return errorf("TypeError", "InvalidPropertyType", "property `%s`: collections containing %s cannot be stored as properties", key, typeName(e))
			}
			if i == 0 {
				first = e
			} else if typeName(first) != typeName(e) && !(isNumber(first) && isNumber(e)) {
				return errorf("TypeError", "InvalidPropertyType", "property `%s`: a collection property must hold values of one type", key)
			}
		}
		return nil
	}
	return errorf("TypeError", "InvalidPropertyType", "property `%s`: %s values cannot be stored as properties", key, typeName(v))
}

func encodeProps(props map[string]any) (string, error) {
	var sb strings.Builder
	sb.Grow(2 + 24*len(props))
	sb.WriteByte('{')
	var small [16]string
	keys := small[:0]
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		writeJSONString(&sb, k)
		sb.WriteByte(':')
		if err := encodeValue(&sb, props[k]); err != nil {
			return "", err
		}
	}
	sb.WriteByte('}')
	return sb.String(), nil
}

// writeJSONString writes s as a JSON string, byte for byte what json.Marshal
// produces. Plain printable ASCII needs no escaping and is written directly;
// anything else (quotes, backslashes, control characters, <, >, &, non-ASCII)
// goes through json.Marshal, which also handles HTML escaping, U+2028/2029 and
// invalid UTF-8.
func writeJSONString(sb *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b, _ := json.Marshal(s)
			sb.Write(b)
			return
		}
	}
	sb.WriteByte('"')
	sb.WriteString(s)
	sb.WriteByte('"')
}

// encodeValue writes v as JSON, keeping floats distinguishable from integers
// (an integral float is written with ".0").
func encodeValue(sb *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		sb.WriteString(strconv.FormatBool(x))
	case int64:
		sb.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errorf("TypeError", "InvalidPropertyType", "NaN and infinity cannot be stored as properties")
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		sb.WriteString(s)
	case string:
		writeJSONString(sb, x)
	case []any:
		sb.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			if err := encodeValue(sb, e); err != nil {
				return err
			}
		}
		sb.WriteByte(']')
	case temporal.Value:
		// A nested object is never a user property value (maps cannot be
		// stored), so {"$t":kind,"v":text} unambiguously marks a temporal.
		b, _ := json.Marshal(x.String())
		sb.WriteString(`{"$t":` + strconv.Itoa(int(x.Kind())) + `,"v":`)
		sb.Write(b)
		sb.WriteByte('}')
	case spatial.Point:
		for _, c := range [...]float64{x.X, x.Y, x.Z} {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				return errorf("TypeError", "InvalidPropertyType", "NaN and infinity cannot be stored in a point")
			}
		}
		sb.WriteString(`{"$p":` + strconv.Itoa(x.SRID) + `,"c":[` +
			strconv.FormatFloat(x.X, 'g', -1, 64) + `,` + strconv.FormatFloat(x.Y, 'g', -1, 64) + `,` +
			strconv.FormatFloat(x.Z, 'g', -1, 64) + `]}`)
	case vector.Vector:
		for _, c := range x.Floats {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				return errorf("TypeError", "InvalidPropertyType", "NaN and infinity cannot be stored in a vector")
			}
		}
		sb.WriteString(`{"$v":"` + string(x.Type) + `","c":[`)
		for i := 0; i < x.Dimension(); i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			if x.Type.IsInteger() {
				sb.WriteString(strconv.FormatInt(x.Ints[i], 10))
			} else {
				sb.WriteString(strconv.FormatFloat(x.Floats[i], 'g', -1, 64))
			}
		}
		sb.WriteString(`]}`)
	default:
		return errorf("TypeError", "InvalidPropertyType", "%s values cannot be stored as properties", typeName(v))
	}
	return nil
}

func decodeProps(s string) (map[string]any, error) {
	if s == "" || s == "{}" {
		return map[string]any{}, nil
	}
	m, ok := fastDecodeObject(s)
	if !ok {
		var err error
		if m, err = decodePropsSlow(s); err != nil {
			return nil, err
		}
	}
	// Only a map or a list can hold a stored temporal, point or vector; a scalar
	// comes back unchanged, so it is not written back (a hash and an assignment
	// for every property of every node scanned).
	for k, v := range m {
		switch v.(type) {
		case map[string]any, []any:
			m[k] = reviveTemporal(v)
		}
	}
	return m, nil
}

// reviveTemporal turns the stored form of a temporal value back into one,
// including inside lists.
func reviveTemporal(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if tname, ok := x["$v"].(string); ok {
			if t, ok := vector.ParseType(tname); ok {
				if c, ok := x["c"].([]any); ok {
					vals := make([]any, len(c))
					for i, e := range c {
						switch n := e.(type) {
						case int64:
							vals[i] = n
						case float64:
							if t.IsInteger() {
								vals[i] = int64(n)
							} else {
								vals[i] = n
							}
						}
					}
					if vec, err := vector.New(vals, len(vals), t); err == nil {
						return vec
					}
				}
			}
		}
		if srid, ok := x["$p"].(int64); ok {
			if c, ok := x["c"].([]any); ok && len(c) == 3 {
				f := func(i int) float64 {
					switch n := c[i].(type) {
					case float64:
						return n
					case int64:
						return float64(n)
					}
					return 0
				}
				return spatial.Point{SRID: int(srid), X: f(0), Y: f(1), Z: f(2)}
			}
		}
		kind, hasKind := x["$t"].(int64)
		text, hasText := x["v"].(string)
		if hasKind && hasText {
			if tv, err := temporal.Parse(temporal.Kind(kind), text); err == nil {
				return tv
			}
		}
	case []any:
		for i, e := range x {
			x[i] = reviveTemporal(e)
		}
	}
	return v
}

// decodePropsSlow is the encoding/json fallback for anything the fast decoder
// declines.
func decodePropsSlow(s string) (map[string]any, error) {
	props := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode properties %q: %w", s, err)
	}
	for k, v := range raw {
		props[k] = fromJSON(v)
	}
	return props, nil
}

func fromJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fromJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = fromJSON(e)
		}
		return out
	}
	return v
}

func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// labelsOf returns the labels in a stored label list. A graph reads the same few
// lists many times (a million nodes with the label "Company"), so each distinct
// text is split once and the slice shared. The shared slices have no spare
// capacity, so adding a label to one node (append) copies instead of writing
// into the others'; removing builds a new slice.
func (g *graph) labelsOf(s string) []string {
	if s == "" {
		return nil
	}
	if l, ok := g.labelSets[s]; ok {
		return l
	}
	l := strings.Split(s, ",")
	l = l[:len(l):len(l)]
	if g.labelSets == nil {
		g.labelSets = map[string][]string{}
	}
	if len(g.labelSets) < 1024 { // bound the table against pathological label lists
		g.labelSets[s] = l
	}
	return l
}

// ─── reads ───────────────────────────────────────────────────────────────────

// readNode is internNode without remembering a node that is not already cached:
// the cached node when there is one, otherwise a freshly decoded one that is
// left out of the cache.
func (g *graph) readNode(id int64, labels, props string) (*Node, error) {
	if n, ok := g.nodes[id]; ok {
		return n, nil
	}
	p, err := decodeProps(props)
	if err != nil {
		return nil, err
	}
	return &Node{ID: id, Labels: g.labelsOf(labels), Props: p}, nil
}

// readRel is internRel without remembering a relationship that is not already
// cached (see readNode).
func (g *graph) readRel(id int64, typ string, start, end int64, props string) (*Rel, error) {
	if r, ok := g.rels[id]; ok {
		return r, nil
	}
	p, err := decodeProps(props)
	if err != nil {
		return nil, err
	}
	return &Rel{ID: id, Type: typ, Start: start, End: end, Props: p}, nil
}

func (g *graph) internNode(id int64, labels, props string) (*Node, error) {
	if n, ok := g.nodes[id]; ok {
		return n, nil
	}
	p, err := decodeProps(props)
	if err != nil {
		return nil, err
	}
	n := &Node{ID: id, Labels: g.labelsOf(labels), Props: p}
	g.nodes[id] = n
	return n, nil
}

func (g *graph) internRel(id int64, typ string, start, end int64, props string) (*Rel, error) {
	if r, ok := g.rels[id]; ok {
		return r, nil
	}
	p, err := decodeProps(props)
	if err != nil {
		return nil, err
	}
	r := &Rel{ID: id, Type: typ, Start: start, End: end, Props: p}
	g.rels[id] = r
	return r, nil
}

// scanNodes returns every live node, restricted to those with the given label
// when it is non-empty, in id order.
//
// hints are property equalities the caller will re-check in Go; they only let
// SQLite skip nodes that cannot match, so each must hold for every true match.
func (g *graph) scanNodes(label string, hints []propHint) ([]*Node, error) {
	var sb strings.Builder
	var args []any
	order := "n.id"
	switch {
	case label != "" && len(hints) == 0:
		// A whole-label scan walks the (label, node_id) index and fetches each
		// node, which is about 40% faster than probing node_labels once per node.
		sb.WriteString(`SELECT n.id, n.labels, n.props FROM node_labels l JOIN nodes n ON n.id = l.node_id WHERE l.label = ?`)
		args = append(args, label)
		order = "l.node_id"
	default:
		sb.WriteString(`SELECT n.id, n.labels, n.props FROM nodes n WHERE 1`)
		if label != "" {
			sb.WriteString(` AND EXISTS (SELECT 1 FROM node_labels l WHERE l.node_id = n.id AND l.label = ?)`)
			args = append(args, label)
		}
	}
	for _, h := range hints {
		// The path is inlined (h.key is a plain identifier) so an expression
		// index on json_extract(props, '$."key"') can serve the lookup.
		sb.WriteString(` AND json_extract(n.props, '$."` + h.key + `"') = ?`)
		args = append(args, h.val)
		g.eng.noteScan(g.ctx, g.db, h.key, g.readOnly)
	}
	sb.WriteString(` ORDER BY ` + order)
	rows, err := g.db.QueryContext(g.ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	// Nodes are decoded as the rows arrive, so the raw text of a large scan is
	// never held alongside the decoded nodes. (Decoding runs no other statement,
	// so the cursor can stay open.)
	var out []*Node
	for rows.Next() {
		var id int64
		var labels, props string
		if err := rows.Scan(&id, &labels, &props); err != nil {
			rows.Close()
			return nil, err
		}
		n, err := g.internNode(id, labels, props)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if !n.Deleted {
			out = append(out, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	scanRowsRead.Add(int64(len(out)))
	return out, nil
}

// scanPage returns up to limit nodes in id order whose id is greater than
// after, that carry label (any node when label is empty) and match the
// property equalities in hints. raw is how many rows the page held before
// deleted nodes were dropped, so a short page means the end; last is the id of
// the page's final row.
func (g *graph) scanPage(label string, hints []propHint, after int64, limit int) (nodes []*Node, last int64, raw int, err error) {
	var sb strings.Builder
	var args []any
	order := "n.id"
	if label != "" && len(hints) == 0 {
		// The (label, node_id) index serves the scan, as in scanNodes.
		sb.WriteString(`SELECT n.id, n.labels, n.props FROM node_labels l JOIN nodes n ON n.id = l.node_id WHERE l.label = ? AND l.node_id > ?`)
		args = append(args, label, after)
		order = "l.node_id"
	} else {
		sb.WriteString(`SELECT n.id, n.labels, n.props FROM nodes n WHERE n.id > ?`)
		args = append(args, after)
		if label != "" {
			sb.WriteString(` AND EXISTS (SELECT 1 FROM node_labels l WHERE l.node_id = n.id AND l.label = ?)`)
			args = append(args, label)
		}
		for _, h := range hints {
			// The path is inlined (h.key is a plain identifier) so an expression
			// index on json_extract(props, '$."key"') can serve the lookup.
			sb.WriteString(` AND json_extract(n.props, '$."` + h.key + `"') = ?`)
			args = append(args, h.val)
		}
	}
	sb.WriteString(` ORDER BY ` + order + ` LIMIT ?`)
	args = append(args, limit)
	rows, err := g.db.QueryContext(g.ctx, sb.String(), args...)
	if err != nil {
		return nil, 0, 0, err
	}
	nodes = make([]*Node, 0, limit)
	for rows.Next() {
		var id int64
		var labels, props string
		if err := rows.Scan(&id, &labels, &props); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		raw++
		last = id
		var n *Node
		if g.evictable {
			// A statement that only reads never modifies a node and compares
			// them by id, so a scanned node need not be remembered.
			n, err = g.readNode(id, labels, props)
		} else {
			n, err = g.internNode(id, labels, props)
		}
		if err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		if !n.Deleted {
			nodes = append(nodes, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	scanRowsRead.Add(int64(raw))
	return nodes, last, raw, nil
}

// scanRowsRead counts the node rows label and node scans have read (for tests).
var scanRowsRead atomic.Int64

// scanNodesByStringKey returns every live node whose property `key` equals one of
// vals, keyed by that value so bucket[sv] holds the matching nodes in id order.
// It runs one batched IN-over-json_each query instead of one json_extract = ?
// scan per distinct value:
//
//	INSERT ... JSON (see bulk loader) → UNWIND list AS k MATCH (n:X {key: k}) …
//
// which is the common bulk-lookup shape. Only string values are batched; a
// non-string property value on a returned node is skipped so no type-coercion
// path silently widens, and nodes arrive in id order (ORDER BY n.id) so every
// bucket sub-slice is also id-ordered — preserving the per-row scan result and
// sequence exactly.
func (g *graph) scanNodesByStringKey(label, key string, vals []string) (map[string][]*Node, error) {
	out := make(map[string][]*Node, len(vals))
	if len(vals) == 0 {
		return out, nil
	}
	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT n.id, n.labels, n.props FROM nodes n WHERE `)
	if label != "" {
		sb.WriteString(`EXISTS (SELECT 1 FROM node_labels l WHERE l.node_id = n.id AND l.label = ?) AND `)
		args = append(args, label)
	}
	// Inlined JSON path so an expression index on json_extract(props,'$."key"')
	// serves the lookup; distinct string values ride in a single bound array.
	sb.WriteString(`json_extract(n.props, '$."` + key + `"') IN (SELECT value FROM json_each(?)) ORDER BY n.id`)
	args = append(args, stringsJSON(vals))
	g.eng.noteScan(g.ctx, g.db, key, g.readOnly)
	rows, err := g.db.QueryContext(g.ctx, sb.String(), args...)
	if err != nil {
		return out, err
	}
	var raw int
	for rows.Next() {
		raw++
		var id int64
		var labels, props string
		if err := rows.Scan(&id, &labels, &props); err != nil {
			rows.Close()
			return out, err
		}
		var n *Node
		if g.evictable {
			n, err = g.readNode(id, labels, props)
		} else {
			n, err = g.internNode(id, labels, props)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		if n.Deleted {
			continue
		}
		sv, ok := n.Props[key].(string)
		if !ok {
			continue // not a string-property node for a string key; the per-row path would skip it too
		}
		out[sv] = append(out[sv], n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	scanRowsRead.Add(int64(raw))
	return out, nil
}

// stringsJSON renders string values as a JSON array (via json.Marshal) — used as
// the bound arg of IN-over-json_each so one query fetches every distinct key at
// once. json.Marshal on []string never fails for plain Go strings.
func stringsJSON(vals []string) string {
	b, _ := json.Marshal(vals)
	return string(b)
}

// cacheLimit is how many cached nodes, relationships and adjacency lists a
// read-only statement keeps before trimCaches drops them.
var cacheLimit = 50000

// trimCaches drops the node, relationship and adjacency caches once they have
// grown past cacheLimit. It does nothing unless the statement only reads (see
// evictable), and callers must not hold a chunk of prefetched work across it.
func (g *graph) trimCaches() {
	if !g.evictable {
		return
	}
	if len(g.nodes)+len(g.rels)+len(g.out)+len(g.in) > cacheLimit {
		clear(g.nodes)
		clear(g.rels)
		clear(g.out)
		clear(g.in)
	}
}

func (g *graph) node(id int64) (*Node, error) {
	if n, ok := g.nodes[id]; ok {
		return n, nil
	}
	var labels, props string
	err := g.queryRow(`SELECT labels, props FROM nodes WHERE id = ?`, []any{id}, &labels, &props)
	if err != nil {
		return nil, err
	}
	return g.internNode(id, labels, props)
}

func (g *graph) queryRow(q string, args []any, dest ...any) error {
	rows, err := g.db.QueryContext(g.ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}

// loadAdj loads the relationships on one side of a node.
func (g *graph) loadAdj(id int64, outgoing bool) ([]*Rel, error) {
	cache := g.in
	col := "end_id"
	if outgoing {
		cache, col = g.out, "start_id"
	}
	if rs, ok := cache[id]; ok {
		return rs, nil
	}
	rows, err := g.db.QueryContext(g.ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE `+col+` = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	type rec struct {
		id         int64
		typ        string
		start, end int64
		props      string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.typ, &r.start, &r.end, &r.props); err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rs := make([]*Rel, 0, len(recs))
	for _, r := range recs {
		rel, err := g.internRel(r.id, r.typ, r.start, r.end, r.props)
		if err != nil {
			return nil, err
		}
		rs = append(rs, rel)
	}
	cache[id] = rs
	return rs, nil
}

// idsJSON renders ids as a JSON array, so a batch lookup is one short constant
// statement (WHERE id IN (SELECT value FROM json_each(?))) instead of one with
// a placeholder per id.
func idsJSON(ids []int64) string {
	b := make([]byte, 0, len(ids)*8+2)
	b = append(b, '[')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, id, 10)
	}
	return string(append(b, ']'))
}

// prefetchAdj loads, with one query per chunk instead of one per node, the
// adjacency of every listed node not already cached.
func (g *graph) prefetchAdj(ids []int64, outgoing bool) error {
	cache := g.in
	col := "end_id"
	if outgoing {
		cache, col = g.out, "start_id"
	}
	var missing []int64
	for _, id := range ids {
		if _, ok := cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	const chunk = 5000
	for len(missing) > 0 {
		n := min(chunk, len(missing))
		part := missing[:n]
		missing = missing[n:]
		rows, err := g.db.QueryContext(g.ctx,
			`SELECT id, type, start_id, end_id, props FROM edges WHERE `+col+` IN (SELECT value FROM json_each(?)) ORDER BY id`, idsJSON(part))
		if err != nil {
			return err
		}
		type rec struct {
			id         int64
			typ        string
			start, end int64
			props      string
		}
		var recs []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.typ, &r.start, &r.end, &r.props); err != nil {
				rows.Close()
				return err
			}
			recs = append(recs, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		got := make(map[int64][]*Rel, n)
		for _, r := range recs {
			rel, err := g.internRel(r.id, r.typ, r.start, r.end, r.props)
			if err != nil {
				return err
			}
			key := r.end
			if outgoing {
				key = r.start
			}
			got[key] = append(got[key], rel)
		}
		for _, id := range part {
			if _, ok := cache[id]; !ok {
				rs := got[id]
				if rs == nil {
					rs = []*Rel{}
				}
				cache[id] = rs
			}
		}
	}
	return nil
}

// prefetchNodes loads the listed nodes that are not cached yet in batches.
func (g *graph) prefetchNodes(ids []int64) error {
	var missing []int64
	for _, id := range ids {
		if _, ok := g.nodes[id]; !ok {
			missing = append(missing, id)
		}
	}
	const chunk = 5000
	for len(missing) > 0 {
		n := min(chunk, len(missing))
		part := missing[:n]
		missing = missing[n:]
		rows, err := g.db.QueryContext(g.ctx,
			`SELECT id, labels, props FROM nodes WHERE id IN (SELECT value FROM json_each(?))`, idsJSON(part))
		if err != nil {
			return err
		}
		type rec struct {
			id            int64
			labels, props string
		}
		var recs []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.labels, &r.props); err != nil {
				rows.Close()
				return err
			}
			recs = append(recs, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range recs {
			if _, err := g.internNode(r.id, r.labels, r.props); err != nil {
				return err
			}
		}
	}
	return nil
}

// outRels and inRels return the live relationships leaving and entering a node.
func (g *graph) outRels(id int64) ([]*Rel, error) { return g.liveRels(id, true) }
func (g *graph) inRels(id int64) ([]*Rel, error)  { return g.liveRels(id, false) }

func (g *graph) liveRels(id int64, outgoing bool) ([]*Rel, error) {
	rs, err := g.loadAdj(id, outgoing)
	if err != nil {
		return nil, err
	}
	live := rs[:0:0]
	for _, r := range rs {
		if !r.Deleted {
			live = append(live, r)
		}
	}
	return live, nil
}

// allRels returns every live relationship in id order.
func (g *graph) allRels() ([]*Rel, error) {
	rows, err := g.db.QueryContext(g.ctx, `SELECT id, type, start_id, end_id, props FROM edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type rec struct {
		id         int64
		typ        string
		start, end int64
		props      string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.typ, &r.start, &r.end, &r.props); err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	var out []*Rel
	for _, r := range recs {
		rel, err := g.internRel(r.id, r.typ, r.start, r.end, r.props)
		if err != nil {
			return nil, err
		}
		if !rel.Deleted {
			out = append(out, rel)
		}
	}
	return out, nil
}

// ─── writes ──────────────────────────────────────────────────────────────────

func cleanProps(props map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(props))
	for k, v := range props {
		if v == nil {
			continue // a null property is no property
		}
		if err := checkPropValue(k, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func (g *graph) createNode(labels []string, props map[string]any) (*Node, error) {
	props, err := cleanProps(props)
	if err != nil {
		return nil, err
	}
	enc, err := encodeProps(props)
	if err != nil {
		return nil, err
	}
	labels = dedupe(labels)
	var id int64
	if batchDisabled {
		res, err := g.db.ExecContext(g.ctx, `INSERT INTO nodes (labels, props) VALUES (?, ?)`, strings.Join(labels, ","), enc)
		if err != nil {
			return nil, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
		if err := g.writeNodeLabels(id, labels); err != nil {
			return nil, err
		}
	} else if id, err = g.bufferNode(labels, enc); err != nil {
		return nil, err
	}
	n := &Node{ID: id, Labels: labels, Props: props}
	g.nodes[id] = n
	g.createdNodes[n] = true
	g.counters.NodesCreated++
	g.counters.Events.NodesCreated++
	g.counters.Events.PropertiesSet += len(props)
	g.counters.NodeLabelsAdded += len(labels)
	return n, nil
}

func (g *graph) createRel(typ string, start, end *Node, props map[string]any) (*Rel, error) {
	props, err := cleanProps(props)
	if err != nil {
		return nil, err
	}
	enc, err := encodeProps(props)
	if err != nil {
		return nil, err
	}
	var id int64
	if batchDisabled {
		res, err := g.db.ExecContext(g.ctx, `INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, ?)`, typ, start.ID, end.ID, enc)
		if err != nil {
			return nil, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
	} else if id, err = g.bufferRel(typ, start.ID, end.ID, enc); err != nil {
		return nil, err
	}
	r := &Rel{ID: id, Type: typ, Start: start.ID, End: end.ID, Props: props}
	g.rels[id] = r
	// Keep any loaded adjacency lists current.
	if rs, ok := g.out[start.ID]; ok {
		g.out[start.ID] = append(rs, r)
	}
	if rs, ok := g.in[end.ID]; ok {
		g.in[end.ID] = append(rs, r)
	}
	g.createdRels[r] = true
	g.counters.RelationshipsCreated++
	g.counters.Events.RelationshipsCreated++
	g.counters.Events.PropertiesSet += len(props)
	return r, nil
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// setProps replaces an entity's property map and persists it. set and removed
// are the numbers of properties assigned and removed, for the counters.
func (g *graph) persistNodeProps(n *Node) error {
	enc, err := encodeProps(n.Props)
	if err != nil {
		return err
	}
	_, err = g.db.ExecContext(g.ctx, `UPDATE nodes SET props = ? WHERE id = ?`, enc, n.ID)
	return err
}

func (g *graph) persistRelProps(r *Rel) error {
	enc, err := encodeProps(r.Props)
	if err != nil {
		return err
	}
	_, err = g.db.ExecContext(g.ctx, `UPDATE edges SET props = ? WHERE id = ?`, enc, r.ID)
	return err
}

func (g *graph) persistLabels(n *Node) error {
	if _, err := g.db.ExecContext(g.ctx, `UPDATE nodes SET labels = ? WHERE id = ?`, strings.Join(n.Labels, ","), n.ID); err != nil {
		return err
	}
	if _, err := g.db.ExecContext(g.ctx, `DELETE FROM node_labels WHERE node_id = ?`, n.ID); err != nil {
		return err
	}
	return g.writeNodeLabels(n.ID, dedupe(n.Labels))
}

// writeNodeLabels records a node's (distinct) labels in node_labels, which label
// lookups join against. Every write of nodes.labels must keep it in step.
func (g *graph) writeNodeLabels(id int64, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	var sb strings.Builder
	args := make([]any, 0, 2*len(labels))
	for i, l := range labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(?,?)")
		args = append(args, id, l)
	}
	_, err := g.db.ExecContext(g.ctx, `INSERT INTO node_labels (node_id, label) VALUES `+sb.String(), args...)
	return err
}

// setNodeProp sets (or, for nil, removes) one property. The change is applied to
// the in-memory node immediately (so later reads in this statement see it) but,
// unless setBehindDisabled is on, persisted as a batched UPDATE by the next
// flushWrites rather than per item; net counters are computed from the snapshot
// at finish, not from how many UPDATEs ran.
func (g *graph) setNodeProp(n *Node, key string, v any) error {
	if n.Deleted {
		return deletedAccess()
	}
	if v == nil {
		if _, had := n.Props[key]; had {
			g.snapNodeProp(n, key)
			g.counters.Events.PropertiesSet++
			delete(n.Props, key)
			return g.markNodePropDirty(n)
		}
		return nil
	}
	if err := checkPropValue(key, v); err != nil {
		return err
	}
	g.snapNodeProp(n, key)
	g.counters.Events.PropertiesSet++
	n.Props[key] = v
	return g.markNodePropDirty(n)
}

func (g *graph) setRelProp(r *Rel, key string, v any) error {
	if r.Deleted {
		return deletedAccess()
	}
	if v == nil {
		if _, had := r.Props[key]; had {
			g.snapRelProp(r, key)
			g.counters.Events.PropertiesSet++
			delete(r.Props, key)
			return g.markRelPropDirty(r)
		}
		return nil
	}
	if err := checkPropValue(key, v); err != nil {
		return err
	}
	g.snapRelProp(r, key)
	g.counters.Events.PropertiesSet++
	r.Props[key] = v
	return g.markRelPropDirty(r)
}

// markNodePropDirty queues a node for its pending property UPDATE. The change is
// already in n.Props; this only records that a write is outstanding, once per flush.
func (g *graph) markNodePropDirty(n *Node) error {
	if setBehindDisabled || g.nodePropSeen[n] {
		if setBehindDisabled {
			return g.persistNodeProps(n)
		}
		return nil
	}
	g.nodePropSeen[n] = true
	g.pendNodeProps = append(g.pendNodeProps, n)
	return nil
}

func (g *graph) markRelPropDirty(r *Rel) error {
	if setBehindDisabled || g.relPropSeen[r] {
		if setBehindDisabled {
			return g.persistRelProps(r)
		}
		return nil
	}
	g.relPropSeen[r] = true
	g.pendRelProps = append(g.pendRelProps, r)
	return nil
}

func (g *graph) addLabels(n *Node, labels []string) error {
	if n.Deleted {
		return deletedAccess()
	}
	changed := false
	for _, l := range labels {
		if !n.hasLabel(l) {
			g.snapNodeLabels(n)
			n.Labels = append(n.Labels, l)
			g.counters.NodeLabelsAdded++
			changed = true
		}
	}
	if changed {
		return g.persistLabels(n)
	}
	return nil
}

func (g *graph) removeLabels(n *Node, labels []string) error {
	if n.Deleted {
		return deletedAccess()
	}
	changed := false
	for _, l := range labels {
		for i, x := range n.Labels {
			if x == l {
				g.snapNodeLabels(n)
				n.Labels = append(append([]string{}, n.Labels[:i]...), n.Labels[i+1:]...)
				g.counters.NodeLabelsRemoved++
				changed = true
				break
			}
		}
	}
	if changed {
		return g.persistLabels(n)
	}
	return nil
}

// snapNodeProp records a property's state before the statement first changes
// it. Nodes the statement created have no earlier state.
func (g *graph) snapNodeProp(n *Node, key string) {
	if g.createdNodes[n] {
		return
	}
	s := g.nodeSnap[n]
	if s == nil {
		s = &entSnap{}
		g.nodeSnap[n] = s
	}
	v, had := n.Props[key]
	s.note(key, v, had)
}

// snapNodeLabels records the node's labels before its first label change.
func (g *graph) snapNodeLabels(n *Node) {
	if g.createdNodes[n] {
		return
	}
	s := g.nodeSnap[n]
	if s == nil {
		s = &entSnap{}
		g.nodeSnap[n] = s
	}
	if !s.labelsSnapped {
		s.labels = append([]string(nil), n.Labels...)
		s.labelsSnapped = true
	}
}

// snapRelProp is snapNodeProp for a relationship.
func (g *graph) snapRelProp(r *Rel, key string) {
	if g.createdRels[r] {
		return
	}
	s := g.relSnap[r]
	if s == nil {
		s = &entSnap{}
		g.relSnap[r] = s
	}
	v, had := r.Props[key]
	s.note(key, v, had)
}

// netCounters fills in the property and label counters from the final state.
func (g *graph) netCounters() error {
	// The label counters report labels that came into or went out of use in
	// the database, not label assignments: the number of nodes holding a label
	// is compared before and after the statement.
	gained, lost := map[string]int{}, map[string]int{}
	for n := range g.createdNodes {
		if n.Deleted {
			continue
		}
		g.counters.PropertiesSet += len(n.Props)
		for _, l := range n.Labels {
			gained[l]++
		}
	}
	for r := range g.createdRels {
		if !r.Deleted {
			g.counters.PropertiesSet += len(r.Props)
		}
	}
	for n, s := range g.nodeSnap {
		if n.Deleted {
			continue
		}
		a, r := s.diff(n.Props)
		g.counters.PropertiesSet += a
		g.counters.PropertiesRemoved += r
		was := s.origLabels(n.Labels)
		for _, l := range n.Labels {
			if !containsStr(was, l) {
				gained[l]++
			}
		}
		for _, l := range was {
			if !n.hasLabel(l) {
				lost[l]++
			}
		}
	}
	for r, s := range g.relSnap {
		if r.Deleted {
			continue
		}
		a, rm := s.diff(r.Props)
		g.counters.PropertiesSet += a
		g.counters.PropertiesRemoved += rm
	}
	// Deleting an entity that existed before the statement removes its
	// properties (and a node's labels); one created and deleted within the
	// statement leaves no trace.
	for _, n := range g.delNodes {
		if g.createdNodes[n] {
			continue
		}
		nprops, labels := len(n.Props), n.Labels
		if s := g.nodeSnap[n]; s != nil {
			nprops, labels = s.origPropCount(n.Props), s.origLabels(n.Labels)
		}
		g.counters.PropertiesRemoved += nprops
		for _, l := range labels {
			lost[l]++
		}
	}
	for _, r := range g.delRels {
		if g.createdRels[r] {
			continue
		}
		nprops := len(r.Props)
		if s := g.relSnap[r]; s != nil {
			nprops = s.origPropCount(r.Props)
		}
		g.counters.PropertiesRemoved += nprops
	}
	affected := map[string]bool{}
	for l := range gained {
		affected[l] = true
	}
	for l := range lost {
		affected[l] = true
	}
	for l := range affected {
		// Only whether the label was or is in use matters, so the count stops
		// one past the number of assignments this statement made: a larger
		// count changes neither answer, and counting a popular label in full
		// would make every bulk-load statement cost the size of the database.
		var after int
		if err := g.queryRow(`SELECT count(*) FROM (SELECT 1 FROM node_labels WHERE label = ? LIMIT ?)`, []any{l, gained[l] + lost[l] + 1}, &after); err != nil {
			return err
		}
		before := after - gained[l] + lost[l]
		switch {
		case before <= 0 && after > 0:
			g.counters.LabelsAdded++
		case before > 0 && after <= 0:
			g.counters.LabelsRemoved++
		}
	}
	return nil
}

func deletedAccess() error {
	return errorf("EntityNotFound", "DeletedEntityAccess", "the entity has been deleted in this statement")
}

// deleteRel marks a relationship deleted; it is removed when the statement ends.
func (g *graph) deleteRel(r *Rel) {
	if r.Deleted {
		return
	}
	r.Deleted = true
	g.delRels = append(g.delRels, r)
}

// deleteNode marks a node deleted. With detach, its relationships are deleted
// with it.
func (g *graph) deleteNode(n *Node, detach bool) error {
	if n.Deleted {
		return nil
	}
	if detach {
		out, err := g.outRels(n.ID)
		if err != nil {
			return err
		}
		in, err := g.inRels(n.ID)
		if err != nil {
			return err
		}
		for _, r := range append(out, in...) {
			g.deleteRel(r)
		}
	}
	n.Deleted = true
	g.delNodes = append(g.delNodes, n)
	return nil
}

// finish applies the deletions. A node that still has a live relationship at
// this point cannot be deleted.
func (g *graph) finish() error {
	if err := g.flushWrites(); err != nil {
		return err
	}
	for _, n := range g.delNodes {
		out, err := g.outRels(n.ID)
		if err != nil {
			return err
		}
		in, err := g.inRels(n.ID)
		if err != nil {
			return err
		}
		if len(out)+len(in) > 0 {
			return errorf("ConstraintVerificationFailed", "DeleteConnectedNode", "cannot delete node<%d>, because it still has relationships", n.ID)
		}
	}
	for _, r := range g.delRels {
		if _, err := g.db.ExecContext(g.ctx, `DELETE FROM edges WHERE id = ?`, r.ID); err != nil {
			return err
		}
		g.counters.Events.RelationshipsDeleted++
		if g.createdRels[r] {
			g.counters.RelationshipsCreated-- // created and deleted within the statement
		} else {
			g.counters.RelationshipsDeleted++
		}
	}
	for _, n := range g.delNodes {
		if _, err := g.db.ExecContext(g.ctx, `DELETE FROM nodes WHERE id = ?`, n.ID); err != nil {
			return err
		}
		g.counters.Events.NodesDeleted++
		if g.createdNodes[n] {
			g.counters.NodesCreated--
		} else {
			g.counters.NodesDeleted++
		}
	}
	if err := g.netCounters(); err != nil {
		return err
	}
	if err := g.checkConstraints(); err != nil {
		return err
	}
	return g.checkVectorIndexes()
}

// preloadNodes loads the given nodes into the cache with one query, so that a
// following run of node(id) calls does not cost a round trip each.
func (g *graph) preloadNodes(ids []int64) error {
	var missing []any
	for _, id := range ids {
		if _, ok := g.nodes[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	q := `SELECT id, labels, props FROM nodes WHERE id IN (?` + strings.Repeat(",?", len(missing)-1) + `)`
	rows, err := g.db.QueryContext(g.ctx, q, missing...)
	if err != nil {
		return err
	}
	type rec struct {
		id            int64
		labels, props string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.labels, &r.props); err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range recs {
		if _, err := g.internNode(r.id, r.labels, r.props); err != nil {
			return err
		}
	}
	return nil
}
