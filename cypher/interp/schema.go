package interp

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/graingraph/graingraph/cypher/syntax"
	"github.com/graingraph/graingraph/cypher/vector"
)

// Schema commands (CREATE/DROP INDEX|CONSTRAINT, SHOW INDEXES|CONSTRAINTS).
//
// Definitions live in the charta_schema table. A RANGE/TEXT index and the
// lookup index behind a uniqueness constraint are SQLite expression indexes on
// json_extract(props, '$."key"'); the other index kinds are recorded so SHOW
// reports them, but queries do not need them to be correct. Constraints are
// enforced by the interpreter when a statement finishes, over the entities the
// statement created or changed.

// schemaTable keeps the project's former name: it is stored in database files,
// and renaming it would make an existing database lose its definitions.
const schemaTable = "charta_schema"

// schemaDef is one index or constraint.
type schemaDef struct {
	invalid    bool     // failed validate(); only DROP sees such rows
	ID         int64    `json:"-"`
	Name       string   `json:"-"`
	Constraint bool     `json:"constraint"`
	Kind       string   `json:"kind"`   // RANGE, TEXT, POINT, LOOKUP, FULLTEXT, VECTOR / UNIQUENESS, KEY, EXISTENCE, TYPE
	Entity     string   `json:"entity"` // NODE or RELATIONSHIP
	Targets    []string `json:"targets"`
	Props      []string `json:"props"`
	ValueType  string   `json:"valueType,omitempty"`
	Options    string   `json:"options,omitempty"`
	BackingIdx string   `json:"backingIndex,omitempty"`

	// Vector indexes: the validated indexConfig.
	VectorDims int    `json:"vectorDimensions,omitempty"`
	VectorSim  string `json:"vectorSimilarity,omitempty"` // "cosine" or "euclidean"
}

func (ex *exec) schemaExists() (bool, error) {
	var n int
	err := ex.g.queryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, []any{schemaTable}, &n)
	return n > 0, err
}

func (ex *exec) ensureSchemaTable() error {
	_, err := ex.g.db.ExecContext(ex.g.ctx, `CREATE TABLE IF NOT EXISTS `+schemaTable+` (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		def  TEXT NOT NULL)`)
	return err
}

// loadSchema reads every definition, in creation order.
func (g *graph) loadSchema() ([]schemaDef, error) { return g.loadSchemaRows(false) }

var (
	backingIdxRe = regexp.MustCompile(`^gl_idx_[0-9]+$`)
	valueTypeRe  = regexp.MustCompile(`^[A-Za-z0-9 <>,|_]{1,512}$`)
)

// validate checks the invariants every consumer of a definition relies on. The
// definitions are read from the database file, which may not have been written
// by graingraph, so none of them is trusted: a row that fails is skipped by
// loadSchema (so it cannot panic a write or be spliced into SQL or Cypher) but
// can still be dropped by name.
func (d *schemaDef) validate() error {
	switch d.Entity {
	case "NODE", "RELATIONSHIP":
	default:
		return fmt.Errorf("unknown entity %q", d.Entity)
	}
	if d.Constraint {
		switch d.Kind {
		case "UNIQUENESS", "KEY", "EXISTENCE", "TYPE":
		default:
			return fmt.Errorf("unknown constraint kind %q", d.Kind)
		}
		if len(d.Targets) < 1 || len(d.Props) < 1 {
			return fmt.Errorf("constraint needs a target and a property")
		}
	} else {
		switch d.Kind {
		case "RANGE", "TEXT", "POINT", "VECTOR", "FULLTEXT":
			if len(d.Targets) < 1 || len(d.Props) < 1 {
				return fmt.Errorf("index needs a target and a property")
			}
		case "LOOKUP":
		default:
			return fmt.Errorf("unknown index kind %q", d.Kind)
		}
	}
	if d.Kind == "VECTOR" {
		if d.VectorDims < 1 || d.VectorDims > vector.MaxDimension {
			return fmt.Errorf("vector dimension %d out of range", d.VectorDims)
		}
		if d.VectorSim != "cosine" && d.VectorSim != "euclidean" {
			return fmt.Errorf("unknown similarity function %q", d.VectorSim)
		}
	}
	if d.BackingIdx != "" && !backingIdxRe.MatchString(d.BackingIdx) {
		return fmt.Errorf("bad backing index name")
	}
	if d.ValueType != "" && !valueTypeRe.MatchString(d.ValueType) {
		return fmt.Errorf("bad value type")
	}
	return nil
}

// loadSchemaRows reads the definitions; invalid ones are included only when
// withInvalid is set (DROP), and are then marked by a nil-safe empty Targets.
func (g *graph) loadSchemaRows(withInvalid bool) ([]schemaDef, error) {
	var exists int
	if err := g.queryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, []any{schemaTable}, &exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, nil
	}
	rows, err := g.db.QueryContext(g.ctx, `SELECT id, name, def FROM `+schemaTable+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schemaDef
	for rows.Next() {
		var d schemaDef
		var raw string
		if err := rows.Scan(&d.ID, &d.Name, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, fmt.Errorf("corrupt schema definition %q: %w", d.Name, err)
		}
		if err := d.validate(); err != nil {
			if !withInvalid {
				continue
			}
			d.invalid = true
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// constraintsFor returns the constraints, loaded once per statement.
func (g *graph) constraintsFor() ([]schemaDef, error) {
	if g.constraintsLoaded {
		return g.constraints, nil
	}
	all, err := g.loadSchema()
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Constraint {
			g.constraints = append(g.constraints, d)
		}
	}
	g.constraintsLoaded = true
	return g.constraints, nil
}

func schemaError(code, format string, args ...any) error {
	return errorf("SchemaError", code, format, args...)
}

// ─── CREATE ─────────────────────────────────────────────────────────────────

// target extracts the entity kind, label/type name and bound variable of the
// pattern an index or constraint is FOR.
func schemaTarget(t syntax.PatternElem) (entity, name, variable string, err error) {
	var le syntax.LabelExpr
	switch t := t.(type) {
	case *syntax.NodePattern:
		entity, variable, le = "NODE", t.Var, t.Labels
	case *syntax.RelPattern:
		entity, variable, le = "RELATIONSHIP", t.Var, t.Types
	default:
		return "", "", "", unsupported("schema target %T", t)
	}
	if le == nil {
		return entity, "", variable, nil // LOOKUP indexes cover every label/type
	}
	ln, ok := le.(*syntax.LabelName)
	if !ok || ln.Dynamic != nil {
		return "", "", "", unsupported("a schema command over more than one plain label or type")
	}
	return entity, ln.Name, variable, nil
}

func schemaProps(exprs []syntax.Expr, variable string) ([]string, error) {
	var out []string
	for _, e := range exprs {
		p, ok := e.(*syntax.Property)
		if !ok {
			return nil, unsupported("an index or constraint over %T (only n.property is supported)", e)
		}
		id, ok := p.Subject.(*syntax.Ident)
		if !ok || id.Name != variable {
			return nil, schemaError("InvalidSchemaTarget", "properties must be accessed on the pattern variable `%s`", variable)
		}
		out = append(out, p.Key)
	}
	return out, nil
}

func (ex *exec) optionsText(e syntax.Expr) (string, error) {
	if e == nil {
		return "", nil
	}
	v, err := ex.eval(e, row{})
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", schemaError("InvalidOptions", "OPTIONS must be a map of plain values")
	}
	return string(b), nil
}

func (ex *exec) execCreateIndex(ci *syntax.CreateIndex) error {
	entity, name, variable, err := schemaTarget(ci.Target)
	if err != nil {
		return err
	}
	kind := ci.Kind
	if kind == "" {
		kind = "RANGE"
	}
	var props []string
	if kind != "LOOKUP" {
		if props, err = schemaProps(ci.Properties, variable); err != nil {
			return err
		}
		if len(props) == 0 {
			return schemaError("InvalidSchemaTarget", "an index needs at least one property")
		}
		if name == "" {
			return schemaError("InvalidSchemaTarget", "an index needs a label or relationship type")
		}
	}
	opts, err := ex.optionsText(ci.Options)
	if err != nil {
		return err
	}
	def := schemaDef{Kind: kind, Entity: entity, Props: props, Options: opts}
	if name != "" {
		def.Targets = []string{name}
	}
	if kind == "VECTOR" {
		if len(props) != 1 {
			return schemaError("InvalidSchemaTarget", "a vector index takes exactly one property")
		}
		if def.VectorDims, def.VectorSim, err = ex.vectorConfig(ci.Options); err != nil {
			return err
		}
	}
	return ex.addSchema(ci.Name, ci.IfNotExists, "index", def)
}

func (ex *exec) execCreateConstraint(cc *syntax.CreateConstraint) error {
	entity, name, variable, err := schemaTarget(cc.Target)
	if err != nil {
		return err
	}
	if name == "" {
		return schemaError("InvalidSchemaTarget", "a constraint needs a label or relationship type")
	}
	props, err := schemaProps(cc.Properties, variable)
	if err != nil {
		return err
	}
	opts, err := ex.optionsText(cc.Options)
	if err != nil {
		return err
	}
	def := schemaDef{Name: cc.Name, Constraint: true, Entity: entity, Targets: []string{name}, Props: props, Options: opts}
	switch cc.Kind {
	case syntax.ConstraintUnique:
		def.Kind = "UNIQUENESS"
	case syntax.ConstraintKey:
		def.Kind = "KEY"
	case syntax.ConstraintNotNull:
		def.Kind = "EXISTENCE"
	case syntax.ConstraintType:
		def.Kind = "TYPE"
		def.ValueType = cc.ValueType
	}
	if len(props) > 1 && (def.Kind == "EXISTENCE" || def.Kind == "TYPE") {
		return schemaError("InvalidSchemaTarget", "this kind of constraint takes a single property")
	}
	// A new constraint must hold for the data already stored.
	if err := ex.validateExisting(def); err != nil {
		return err
	}
	return ex.addSchema(cc.Name, cc.IfNotExists, "constraint", def)
}

// addSchema records a definition and creates its backing SQLite index.
func (ex *exec) addSchema(name string, ifNotExists bool, what string, def schemaDef) error {
	if err := ex.ensureSchemaTable(); err != nil {
		return err
	}
	existing, err := ex.g.loadSchema()
	if err != nil {
		return err
	}
	for _, d := range existing {
		same := d.Constraint == def.Constraint && d.Kind == def.Kind && d.Entity == def.Entity &&
			strings.Join(d.Targets, ",") == strings.Join(def.Targets, ",") && strings.Join(d.Props, ",") == strings.Join(def.Props, ",")
		if name != "" && d.Name == name || (name == "" && same) {
			if ifNotExists {
				return nil
			}
			if name != "" && d.Name == name {
				return schemaError("EquivalentSchemaRuleAlreadyExists", "a %s named `%s` already exists", what, name)
			}
			return schemaError("EquivalentSchemaRuleAlreadyExists", "an equivalent %s already exists (`%s`)", what, d.Name)
		}
		if same && name != "" {
			if ifNotExists {
				return nil
			}
			return schemaError("EquivalentSchemaRuleAlreadyExists", "an equivalent %s already exists (`%s`)", what, d.Name)
		}
	}
	// Reserve the row to learn the id, then fill in the generated name.
	if name == "" {
		name = "pending" + strconv.FormatInt(int64(len(existing))+1, 10)
	}
	raw, _ := json.Marshal(def)
	res, err := ex.g.db.ExecContext(ex.g.ctx, `INSERT INTO `+schemaTable+` (name, def) VALUES (?, ?)`, name, string(raw))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if strings.HasPrefix(name, "pending") {
		name = fmt.Sprintf("%s_%d", map[bool]string{false: "index", true: "constraint"}[def.Constraint], id)
		if _, err := ex.g.db.ExecContext(ex.g.ctx, `UPDATE `+schemaTable+` SET name = ? WHERE id = ?`, name, id); err != nil {
			return err
		}
	}
	if idx := backingIndexSQL(id, def); idx != "" && (def.Kind == "RANGE" || def.Kind == "TEXT" || def.Kind == "UNIQUENESS" || def.Kind == "KEY") {
		if _, err := ex.g.db.ExecContext(ex.g.ctx, idx); err != nil {
			return err
		}
		def.BackingIdx = backingIndexName(id)
		raw, _ := json.Marshal(def)
		if _, err := ex.g.db.ExecContext(ex.g.ctx, `UPDATE `+schemaTable+` SET def = ? WHERE id = ?`, string(raw), id); err != nil {
			return err
		}
	}
	if def.Kind == "VECTOR" {
		ex.g.eng.dropVectorMatrix(id)
	}
	if def.Constraint {
		ex.g.counters.ConstraintsAdded++
		ex.g.constraintsLoaded = false
		ex.g.constraints = nil
	} else {
		ex.g.counters.IndexesAdded++
	}
	return nil
}

func backingIndexName(id int64) string { return "gl_idx_" + strconv.FormatInt(id, 10) }

// backingIndexSQL is the CREATE INDEX statement for a definition, or "" when
// a property key is not a plain identifier (the definition is still recorded).
//
// The index is partial: an entity keeps an entry only while every one of its
// indexed properties is non-null, so a node of any unrelated label (which lacks
// all of them) writes nothing here. That is what stops the importer's other-label
// constraints from polluting every insert, and a query on `expr = <value>` lets
// SQLite use it: an equality against a known value proves `expr IS NOT NULL`.
// For a composite index the admission test is the AND of all expressions, so only
// rows that set every indexed property are admitted; a prefix lookup that
// constrains only the first column falls back to a scan rather than misusing it.
func backingIndexSQL(id int64, d schemaDef) string {
	var exprs []string
	for _, p := range d.Props {
		if !pushableKey(p) {
			return ""
		}
		exprs = append(exprs, `json_extract(props, '$."`+p+`"')`)
	}
	if len(exprs) == 0 {
		return ""
	}
	table := "nodes"
	if d.Entity == "RELATIONSHIP" {
		table = "edges"
	}
	where := make([]string, 0, len(exprs))
	for _, e := range exprs {
		where = append(where, e+` IS NOT NULL`)
	}
	sql := `CREATE INDEX IF NOT EXISTS ` + backingIndexName(id) + ` ON ` + table + `(` + strings.Join(exprs, ", ") + `)`
	if len(where) == 1 {
		return sql + " WHERE " + where[0]
	}
	return sql + " WHERE " + strings.Join(where, " AND ")
}

// ─── DROP ───────────────────────────────────────────────────────────────────

func (ex *exec) execDropSchema(ds *syntax.DropSchema) error {
	defs, err := ex.g.loadSchemaRows(true)
	if err != nil {
		return err
	}
	for _, d := range defs {
		if d.Name != ds.Name {
			continue
		}
		if d.Constraint != ds.Constraint {
			what := "an index"
			if d.Constraint {
				what = "a constraint"
			}
			return schemaError("SchemaRuleNotFound", "`%s` is %s, not a %s", ds.Name, what, map[bool]string{false: "index", true: "constraint"}[ds.Constraint])
		}
		if d.Kind == "VECTOR" {
			ex.g.eng.dropVectorMatrix(d.ID)
		}
		if d.BackingIdx != "" { // always the derived name, never the stored text
			if _, err := ex.g.db.ExecContext(ex.g.ctx, `DROP INDEX IF EXISTS `+backingIndexName(d.ID)); err != nil {
				return err
			}
		}
		if _, err := ex.g.db.ExecContext(ex.g.ctx, `DELETE FROM `+schemaTable+` WHERE id = ?`, d.ID); err != nil {
			return err
		}
		if d.Constraint {
			ex.g.counters.ConstraintsRemoved++
			ex.g.constraintsLoaded = false
			ex.g.constraints = nil
		} else {
			ex.g.counters.IndexesRemoved++
		}
		return nil
	}
	if ds.IfExists {
		return nil
	}
	what := "index"
	if ds.Constraint {
		what = "constraint"
	}
	return schemaError("SchemaRuleNotFound", "no %s named `%s`", what, ds.Name)
}

// ─── enforcement ────────────────────────────────────────────────────────────

func describeEntity(entity string, id int64) string {
	if entity == "NODE" {
		return fmt.Sprintf("Node(%d)", id)
	}
	return fmt.Sprintf("Relationship(%d)", id)
}

func constraintViolation(d schemaDef, format string, args ...any) error {
	e := errorf("ConstraintVerificationFailed", "ConstraintValidationFailed", format, args...)
	e.Schema = &SchemaRef{Name: d.Name, Kind: d.Kind, Entity: d.Entity, Properties: d.Props}
	if len(d.Targets) > 0 {
		e.Schema.Target = d.Targets[0]
	}
	return e
}

// checkOne verifies a single entity against one constraint (uniqueness is
// checked separately because it needs the rest of the graph).
func checkOne(d schemaDef, id int64, props map[string]any) error {
	who := describeEntity(d.Entity, id)
	target := d.Targets[0]
	switch d.Kind {
	case "EXISTENCE", "KEY":
		for _, p := range d.Props {
			if props[p] == nil {
				return constraintViolation(d, "%s with %s `%s` must have the property `%s`", who, map[string]string{"NODE": "label", "RELATIONSHIP": "type"}[d.Entity], target, p)
			}
		}
	case "TYPE":
		p := d.Props[0]
		if v := props[p]; v != nil && !matchesVTypes(v, parseVTypes(d.ValueType)) {
			return constraintViolation(d, "%s with %s `%s` requires the property `%s` to be of type %s, but it was %s",
				who, map[string]string{"NODE": "label", "RELATIONSHIP": "type"}[d.Entity], target, p, d.ValueType, valueTypeName(v, true))
		}
	}
	return nil
}

// tupleOf returns the constrained property values of an entity, or ok=false if
// any is null (a null never conflicts).
func tupleOf(d schemaDef, props map[string]any) ([]any, bool) {
	out := make([]any, len(d.Props))
	for i, p := range d.Props {
		if props[p] == nil {
			return nil, false
		}
		out[i] = props[p]
	}
	return out, true
}

// findDuplicate looks for another entity with the same constrained values.
func (g *graph) findDuplicate(d schemaDef, id int64, tuple []any) (int64, error) {
	table := "nodes"
	label := `EXISTS (SELECT 1 FROM node_labels l WHERE l.node_id = e.id AND l.label = ?)`
	if d.Entity == "RELATIONSHIP" {
		table, label = "edges", `e.type = ?`
	}
	var sb strings.Builder
	args := []any{id, d.Targets[0]}
	sb.WriteString(`SELECT e.id, e.props FROM ` + table + ` e WHERE e.id <> ? AND ` + label)
	scalar := true
	for i, p := range d.Props {
		switch tuple[i].(type) {
		case string, int64, float64:
			if pushableKey(p) {
				sb.WriteString(` AND json_extract(e.props, '$."` + p + `"') = ?`)
				args = append(args, tuple[i])
				continue
			}
		}
		scalar = false
	}
	rows, err := g.db.QueryContext(g.ctx, sb.String(), args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var other int64
		var raw string
		if err := rows.Scan(&other, &raw); err != nil {
			return 0, err
		}
		if scalar {
			return other, nil
		}
		props, err := decodeProps(raw)
		if err != nil {
			return 0, err
		}
		same := true
		for i, p := range d.Props {
			if equals(props[p], tuple[i]) != triTrue {
				same = false
				break
			}
		}
		if same {
			return other, nil
		}
	}
	return 0, rows.Err()
}

// checkConstraints validates the entities the statement created or changed.
func (g *graph) checkConstraints() error {
	if len(g.createdNodes)+len(g.nodeSnap)+len(g.createdRels)+len(g.relSnap) == 0 {
		return nil
	}
	cons, err := g.constraintsFor()
	if err != nil || len(cons) == 0 {
		return err
	}
	check := func(entity string, id int64, labels []string, props map[string]any) error {
		for _, d := range cons {
			if d.Entity != entity || !containsStr(labels, d.Targets[0]) {
				continue
			}
			if err := checkOne(d, id, props); err != nil {
				return err
			}
			if d.Kind == "UNIQUENESS" || d.Kind == "KEY" {
				tuple, ok := tupleOf(d, props)
				if !ok {
					continue
				}
				other, err := g.findDuplicate(d, id, tuple)
				if err != nil {
					return err
				}
				if other != 0 {
					vals := make([]string, len(tuple))
					for i, v := range tuple {
						vals[i] = fmt.Sprintf("`%s` = %s", d.Props[i], renderForError(v))
					}
					return constraintViolation(d, "%s already exists with %s `%s` and property %s",
						describeEntity(entity, other), map[string]string{"NODE": "label", "RELATIONSHIP": "type"}[entity], d.Targets[0], strings.Join(vals, ", "))
				}
			}
		}
		return nil
	}
	touchedNodes := map[*Node]bool{}
	for n := range g.createdNodes {
		touchedNodes[n] = true
	}
	for n := range g.nodeSnap {
		touchedNodes[n] = true
	}
	var nodes []*Node
	for n := range touchedNodes {
		if !n.Deleted {
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		if err := check("NODE", n.ID, n.Labels, n.Props); err != nil {
			return err
		}
	}
	touchedRels := map[*Rel]bool{}
	for r := range g.createdRels {
		touchedRels[r] = true
	}
	for r := range g.relSnap {
		touchedRels[r] = true
	}
	var rels []*Rel
	for r := range touchedRels {
		if !r.Deleted {
			rels = append(rels, r)
		}
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].ID < rels[j].ID })
	for _, r := range rels {
		if err := check("RELATIONSHIP", r.ID, []string{r.Type}, r.Props); err != nil {
			return err
		}
	}
	return nil
}

func renderForError(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return fmt.Sprint(v)
}

// validateExisting checks a new constraint against the stored data.
func (ex *exec) validateExisting(d schemaDef) error {
	seen := map[string]int64{}
	visit := func(id int64, props map[string]any) error {
		if err := checkOne(d, id, props); err != nil {
			return err
		}
		if d.Kind == "UNIQUENESS" || d.Kind == "KEY" {
			tuple, ok := tupleOf(d, props)
			if !ok {
				return nil
			}
			key := groupKey(tuple)
			if other, dup := seen[key]; dup {
				return constraintViolation(d, "cannot create the constraint: %s and %s both have the same values for %s",
					describeEntity(d.Entity, other), describeEntity(d.Entity, id), strings.Join(d.Props, ", "))
			}
			seen[key] = id
		}
		return nil
	}
	if d.Entity == "NODE" {
		nodes, err := ex.g.scanNodes(d.Targets[0], nil)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if err := visit(n.ID, n.Props); err != nil {
				return err
			}
		}
		return nil
	}
	rels, err := ex.g.allRels()
	if err != nil {
		return err
	}
	for _, r := range rels {
		if r.Type == d.Targets[0] {
			if err := visit(r.ID, r.Props); err != nil {
				return err
			}
		}
	}
	return nil
}

// vectorConfig validates the OPTIONS of CREATE VECTOR INDEX:
//
//	OPTIONS {indexConfig: {`vector.dimensions`: 384, `vector.similarity_function`: 'cosine'}}
//
// The dimension is required (1 to vector.MaxDimension); the similarity function
// is cosine (default) or euclidean. The HNSW tuning keys Neo4j accepts are
// accepted and ignored, since graingraph searches exhaustively.
func (ex *exec) vectorConfig(opts syntax.Expr) (dims int, sim string, err error) {
	bad := func(format string, args ...any) (int, string, error) {
		return 0, "", schemaError("InvalidOptions", format, args...)
	}
	if opts == nil {
		return bad("a vector index needs OPTIONS {indexConfig: {`vector.dimensions`: n}}")
	}
	v, err := ex.eval(opts, row{})
	if err != nil {
		return 0, "", err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return bad("OPTIONS must be a map")
	}
	for k := range m {
		if k != "indexConfig" && k != "indexProvider" {
			return bad("unknown option %q (expected indexConfig)", k)
		}
	}
	cfg, ok := m["indexConfig"].(map[string]any)
	if !ok {
		return bad("a vector index needs OPTIONS {indexConfig: {`vector.dimensions`: n}}")
	}
	sim = "cosine"
	haveDims := false
	for k, val := range cfg {
		switch k {
		case "vector.dimensions":
			n, isInt := val.(int64)
			if !isInt || n < 1 || n > int64(vector.MaxDimension) {
				return bad("vector.dimensions must be an integer between 1 and %d, got %v", vector.MaxDimension, val)
			}
			dims, haveDims = int(n), true
		case "vector.similarity_function":
			s, isStr := val.(string)
			if !isStr || (!strings.EqualFold(s, "cosine") && !strings.EqualFold(s, "euclidean")) {
				return bad("vector.similarity_function must be 'cosine' or 'euclidean', got %v", val)
			}
			sim = strings.ToLower(s)
		case "vector.hnsw.m", "vector.hnsw.ef_construction", "vector.quantization.enabled":
			// accepted for compatibility; the search is exhaustive
		default:
			return bad("unknown index setting %q", k)
		}
	}
	if !haveDims {
		return bad("vector.dimensions is required")
	}
	return dims, sim, nil
}

// vectorIndexes returns the vector index definitions, loaded once per statement.
func (g *graph) vectorIndexes() ([]schemaDef, error) {
	if !g.vectorLoaded {
		all, err := g.loadSchema()
		if err != nil {
			return nil, err
		}
		for _, d := range all {
			if !d.Constraint && d.Kind == "VECTOR" {
				g.vectors = append(g.vectors, d)
			}
		}
		g.vectorLoaded = true
	}
	return g.vectors, nil
}

// vectorValue reports whether v is acceptable for a vector index of the given
// dimension: a VECTOR value or a list of numbers of that length.
func vectorValue(v any, dims int) bool {
	switch x := v.(type) {
	case vector.Vector:
		return x.Dimension() == dims
	case []any:
		if len(x) != dims {
			return false
		}
		for _, e := range x {
			switch n := e.(type) {
			case int64:
			case float64:
				if math.IsNaN(n) || math.IsInf(n, 0) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return false
}

// checkVectorIndexes validates the vector properties of the entities a
// statement created or changed against the vector indexes covering them.
func (g *graph) checkVectorIndexes() error {
	if len(g.createdNodes)+len(g.nodeSnap)+len(g.createdRels)+len(g.relSnap)+len(g.delNodes) == 0 {
		return nil
	}
	idx, err := g.vectorIndexes()
	if err != nil || len(idx) == 0 {
		return err
	}
	defer func() { g.vectorDeltas(idx) }()
	check := func(entity string, id int64, labels []string, props map[string]any) error {
		for _, d := range idx {
			if d.Entity != entity || !containsStr(labels, d.Targets[0]) {
				continue
			}
			if v := props[d.Props[0]]; v != nil && !vectorValue(v, d.VectorDims) {
				return constraintViolation(d, "%s with %s `%s`: property `%s` must be a vector of dimension %d (vector index `%s`), got %s",
					describeEntity(entity, id), map[string]string{"NODE": "label", "RELATIONSHIP": "type"}[entity],
					d.Targets[0], d.Props[0], d.VectorDims, d.Name, valueTypeName(v, true))
			}
		}
		return nil
	}
	seenN := map[*Node]bool{}
	for _, set := range []map[*Node]bool{g.createdNodes} {
		for n := range set {
			seenN[n] = true
		}
	}
	for n := range g.nodeSnap {
		seenN[n] = true
	}
	for n := range seenN {
		if !n.Deleted {
			if err := check("NODE", n.ID, n.Labels, n.Props); err != nil {
				return err
			}
		}
	}
	seenR := map[*Rel]bool{}
	for r := range g.createdRels {
		seenR[r] = true
	}
	for r := range g.relSnap {
		seenR[r] = true
	}
	for r := range seenR {
		if !r.Deleted {
			if err := check("RELATIONSHIP", r.ID, []string{r.Type}, r.Props); err != nil {
				return err
			}
		}
	}
	return nil
}
