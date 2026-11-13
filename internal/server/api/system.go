package api

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/graingraph/graingraph/internal/server/store"
)

// systemDatabase is the name of Neo4j's administration database. Tools such as
// Neo4j Desktop and Browser query it to discover the databases of a server;
// graingraph-query answers the read-only SHOW DATABASE commands and db.ping() there, from what it
// knows (one user database and the system database itself) and nothing else.
const systemDatabase = "system"

var showDatabasesFields = []string{
	"name", "type", "aliases", "access", "databaseID", "address", "role", "writer",
	"requestedStatus", "currentStatus", "statusMessage", "default", "home", "constituents",
}

var showDefaultDatabaseFields = []string{
	"name", "type", "aliases", "access", "databaseID", "address", "role", "writer",
	"requestedStatus", "currentStatus", "statusMessage",
}

var (
	showProceduresFields = []string{"name", "description", "mode", "worksOnSystem", "signature", "argumentDescription",
		"returnDescription", "admin", "rolesExecution", "rolesBoostedExecution", "isDeprecated", "option"}
	showFunctionsFields = []string{"name", "category", "description", "isBuiltIn", "argumentDescription", "signature",
		"returnDescription", "aggregating", "rolesExecution", "rolesBoostedExecution", "isDeprecated", "deprecatedBy"}
)

// adminPrivileges are the privileges of the PUBLIC and admin roles, as Neo4j
// lists them (action, resource, graph, segment, role).
var adminPrivileges = [][5]string{
	{"load", "file", "*", "ALL DATA", "PUBLIC"},
	{"execute", "database", "*", "FUNCTION(*)", "PUBLIC"},
	{"execute", "database", "*", "PROCEDURE(*)", "PUBLIC"},
	{"access", "database", "HOME", "database", "PUBLIC"},
	{"load", "file", "*", "ALL DATA", "admin"},
	{"match", "all_properties", "*", "NODE(*)", "admin"},
	{"write", "graph", "*", "NODE(*)", "admin"},
	{"match", "all_properties", "*", "RELATIONSHIP(*)", "admin"},
	{"write", "graph", "*", "RELATIONSHIP(*)", "admin"},
	{"transaction_management", "database", "*", "USER(*)", "admin"},
	{"access", "database", "*", "database", "admin"},
	{"constraint", "database", "*", "database", "admin"},
	{"dbms_actions", "database", "*", "database", "admin"},
	{"index", "database", "*", "database", "admin"},
	{"show_constraint", "database", "*", "database", "admin"},
	{"show_index", "database", "*", "database", "admin"},
	{"start_database", "database", "*", "database", "admin"},
	{"stop_database", "database", "*", "database", "admin"},
	{"token", "database", "*", "database", "admin"},
}

// systemOutcome answers a statement sent to the system database: the commands
// the Neo4j tools issue when they connect, answered from what graingraph-query
// knows (one user database and the system database; no procedures or
// functions to list). A YIELD clause selects and orders the columns.
func (h *Handler) systemOutcome(statement, address, user string) (*store.Outcome, *apiError) {
	words := strings.Fields(strings.TrimRight(strings.TrimSpace(statement), "; \t\r\n"))
	head, yield := words, []string(nil)
	for i, w := range words {
		if strings.EqualFold(w, "YIELD") {
			head = words[:i]
			for _, c := range strings.Split(strings.Join(words[i+1:], " "), ",") {
				if c = strings.TrimSpace(c); c != "" {
					yield = append(yield, c)
				}
			}
			break
		}
	}
	upper := make([]string, len(head))
	for i, w := range head {
		upper[i] = strings.ToUpper(w)
	}
	cmd := strings.Join(upper, " ")

	row := func(name, typ string, def bool) []any {
		return []any{name, typ, []any{}, "read-write", h.databaseID(name), address, "primary", true,
			"online", "online", "", def, def, []any{}}
	}
	all := [][]any{row(h.database, "standard", true), row(systemDatabase, "system", false)}
	var (
		fields []string
		rows   [][]any
	)
	switch {
	case cmd == "CALL DB.PING()" || cmd == "CALL DB.PING":
		fields, rows = []string{"success"}, [][]any{{true}}
	case cmd == "SHOW DATABASES":
		fields, rows = showDatabasesFields, all
	case cmd == "SHOW DEFAULT DATABASE" || cmd == "SHOW HOME DATABASE":
		fields, rows = showDefaultDatabaseFields, [][]any{row(h.database, "standard", true)[:11]}
	case len(head) == 3 && upper[0] == "SHOW" && upper[1] == "DATABASE":
		name := strings.Trim(head[2], "`")
		for _, r := range all {
			if strings.EqualFold(r[0].(string), name) {
				fields, rows = showDatabasesFields, [][]any{r}
			}
		}
		if rows == nil {
			return nil, clientError(CodeDatabaseNotFound, "Database does not exist. Database name: '%s'.", name).withStatus(http.StatusNotFound)
		}
	case cmd == "SHOW CURRENT USER":
		fields = []string{"user", "roles", "passwordChangeRequired", "suspended", "home"}
		rows = [][]any{{user, []any{"admin", "PUBLIC"}, false, false, nil}}
	case cmd == "CALL DBMS.COMPONENTS()" || cmd == "CALL DBMS.COMPONENTS":
		fields = []string{"name", "versions", "edition"}
		rows = [][]any{{"Neo4j Kernel", []any{emulatedNeo4jVersion}, emulatedNeo4jEdition}}
	case cmd == "CALL DBMS.INFO()" || cmd == "CALL DBMS.INFO":
		fields = []string{"id", "name", "creationDate"}
		rows = [][]any{{"graingraph-query", h.database, h.created.UTC().Format(time.RFC3339)}}
	case cmd == "CALL DBMS.LICENSEAGREEMENTDETAILS()" || cmd == "CALL DBMS.LICENSEAGREEMENTDETAILS":
		fields, rows = []string{"status", "daysLeftOnTrial", "totalTrialDays"}, [][]any{{"yes", nil, nil}}
	case strings.HasPrefix(cmd, "CALL DBMS.LISTCONFIG"):
		// Tools read settings such as dbms.security.auth_enabled (Bloom refuses
		// to start unless it is true). graingraph-query always requires auth.
		search := ""
		if open, end := strings.Index(statement, "("), strings.LastIndex(statement, ")"); open >= 0 && end > open {
			search = strings.Trim(strings.TrimSpace(statement[open+1:end]), `'"`)
		}
		fields = []string{"name", "description", "value", "dynamic", "defaultValue", "startupValue", "explicitlySet", "validValues"}
		rows = [][]any{}
		const authName = "dbms.security.auth_enabled"
		if strings.Contains(authName, strings.ToLower(search)) {
			rows = append(rows, []any{authName, "Enable auth requirement to access Neo4j.\nDefaults to `true`.", "true", false, "true", "true", true, "A boolean."})
		}
	case cmd == "SHOW ALL ROLES" || cmd == "SHOW ROLES" || cmd == "SHOW POPULATED ROLES":
		// Everyone is an admin; the built-in roles exist for tools that list them.
		fields = []string{"role"}
		rows = [][]any{{"PUBLIC"}, {"admin"}, {"architect"}, {"editor"}, {"publisher"}, {"reader"}}
		if cmd == "SHOW POPULATED ROLES" {
			rows = [][]any{{"PUBLIC"}, {"admin"}}
		}
	case len(head) >= 3 && upper[0] == "SHOW" && upper[1] == "USER" && upper[len(upper)-1] == "PRIVILEGES" && len(head) <= 4:
		// Everyone is an admin: the privileges Neo4j shows for its default admin.
		who := user
		if len(head) == 4 {
			who = strings.Trim(head[2], "`")
		}
		fields = []string{"access", "action", "resource", "graph", "segment", "role", "immutable", "user"}
		for _, p := range adminPrivileges {
			rows = append(rows, []any{"GRANTED", p[0], p[1], p[2], p[3], p[4], false, who})
		}
	case cmd == "SHOW PROCEDURES" || cmd == "SHOW PROCEDURE":
		fields, rows = showProceduresFields, [][]any{}
	case cmd == "SHOW FUNCTIONS" || cmd == "SHOW FUNCTION":
		fields, rows = showFunctionsFields, [][]any{}
	case cmd == "CALL DBMS.CLIENTCONFIG()" || cmd == "CALL DBMS.CLIENTCONFIG":
		fields, rows = []string{"name", "value"}, [][]any{}
	case strings.HasPrefix(cmd, "CALL DB.AWAITINDEXES") ||
		strings.HasPrefix(cmd, "CALL DB.AWAITINDEX(") ||
		cmd == "CALL DB.AWAITINDEX":
		// db.awaitIndexes([timeOutSeconds]) and db.awaitIndex(indexName[,
		// timeOutSeconds]) let a caller wait for index population to finish
		// before continuing. graingraph builds its property expression indexes
		// synchronously within the statement (in graph.finish), so every index is
		// already online: there is nothing to wait for, and both calls are a no-op
		// that returns Neo4j's VOID result — no columns, no rows. Their arguments
		// are accepted and ignored; this lets importers that pause for an index
		// build proceed unchanged.
		fields, rows = []string{}, [][]any{}
	case len(head) >= 2 && upper[0] == "CALL":
		name := strings.TrimSuffix(strings.SplitN(head[1], "(", 2)[0], "()")
		return nil, clientError("Neo.ClientError.Procedure.ProcedureNotFound", "There is no procedure with the name `%s` registered for this database instance. Please ensure you've spelled the procedure name correctly and that the procedure is properly deployed.", name)
	default:
		return nil, clientError(CodeSyntaxError, "graingraph-query supports only a few read-only administration commands on the system database (SHOW DATABASES, SHOW CURRENT USER, SHOW PROCEDURES, SHOW FUNCTIONS, CALL db.ping(), CALL dbms.components(), CALL dbms.info(), CALL dbms.clientConfig(), CALL db.awaitIndexes(), CALL db.awaitIndex()).")
	}

	if len(yield) > 0 && !(len(yield) == 1 && yield[0] == "*") {
		idx := make([]int, len(yield))
		out := make([]string, len(yield))
		for i, y := range yield {
			name, _, _ := strings.Cut(y, " ") // "name AS alias": keep the alias
			alias := name
			if parts := strings.Fields(y); len(parts) == 3 && strings.EqualFold(parts[1], "AS") {
				alias = parts[2]
			}
			idx[i] = slices.Index(fields, name)
			if idx[i] < 0 {
				return nil, clientError(CodeSyntaxError, "Variable `%s` not defined: the procedure or command has no column of that name.", name)
			}
			out[i] = alias
		}
		projected := make([][]any, len(rows))
		for r, row := range rows {
			projected[r] = make([]any, len(idx))
			for i, j := range idx {
				projected[r][i] = row[j]
			}
		}
		fields, rows = out, projected
	}
	return &store.Outcome{Fields: fields, Values: rows, Counters: &store.Counters{}}, nil
}

// System transactions. Neo4j Desktop opens explicit transactions on the system
// database. They only ever run the read-only commands above, so they are
// tracked here, apart from the store's single transaction: they neither wait
// for nor block a transaction on the graph. They expire like any other.

type systemTxs struct {
	mu  sync.Mutex
	ids map[string]time.Time // transaction id -> expiry (zero: never)
}

func (s *systemTxs) open(timeout time.Duration) (string, time.Time, error) {
	id, err := store.NewTxID()
	if err != nil {
		return "", time.Time{}, err
	}
	var exp time.Time
	if timeout > 0 {
		exp = time.Now().Add(timeout)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]time.Time{}
	}
	now := time.Now()
	for k, e := range s.ids {
		if !e.IsZero() && now.After(e) {
			delete(s.ids, k)
		}
	}
	s.ids[id] = exp
	return id, exp, nil
}

// get reports whether id is open, extending its expiry by timeout.
func (s *systemTxs) get(id string, timeout time.Duration) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.ids[id]
	if !ok || (!exp.IsZero() && time.Now().After(exp)) {
		delete(s.ids, id)
		return time.Time{}, false
	}
	if timeout > 0 {
		exp = time.Now().Add(timeout)
		s.ids[id] = exp
	}
	return exp, true
}

func (s *systemTxs) close(id string) {
	s.mu.Lock()
	delete(s.ids, id)
	s.mu.Unlock()
}

func (h *Handler) isSystem(r *http.Request) bool {
	return strings.EqualFold(r.PathValue("databaseName"), systemDatabase)
}

func systemInfo(id string, exp time.Time) *txInfo {
	info := &txInfo{ID: id}
	if !exp.IsZero() {
		info.Expires = exp.UTC().Format(time.RFC3339)
	}
	return info
}

// systemTx serves the four transaction endpoints for the system database.
// op is "open", "run", "commit" or "rollback".
func (h *Handler) systemTx(w http.ResponseWriter, r *http.Request, op string) {
	id := r.PathValue("transactionID")
	var exp time.Time
	if op != "open" {
		note(w, slog.String("tx_id", id))
		var ok bool
		if exp, ok = h.sysTx.get(id, h.txTimeout); !ok {
			h.reject(w, r, clientError(CodeRequestInvalid, "Transaction with Id: %q was not found. It may have timed out and therefore rolled back.", id).withStatus(http.StatusNotFound))
			return
		}
	}
	if op == "rollback" {
		h.sysTx.close(id)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	rt, ok := h.negotiate(w, r)
	if !ok {
		return
	}
	q, ok := h.decodeBody(w, r)
	if !ok {
		return
	}
	if op == "open" {
		var err error
		timeout := h.txTimeout
		if q.MaxExecutionTime != nil {
			timeout = *q.MaxExecutionTime
		}
		if id, exp, err = h.sysTx.open(timeout); err != nil {
			writeFailure(w, serverError(CodeGeneralError, "%v", err))
			return
		}
		note(w, slog.String("tx_id", id))
	}

	var (
		outcome *store.Outcome
		t0      = time.Now()
		avail   float64
	)
	if q.Statement != "" {
		noteStatement(w, q.Statement, "s")
		var ae *apiError
		outcome, ae = h.systemOutcome(q.Statement, r.Host, basicUser(r))
		avail = elapsedMs(t0)
		if ae != nil {
			h.sysTx.close(id) // a failed statement destroys the transaction
			h.reject(w, r, ae)
			return
		}
	} else if op == "run" {
		h.reject(w, r, clientError(CodeRequestInvalid, "statement cannot be null or empty"))
		return
	}

	meta := resultMeta{queryType: "s", availableAfter: avail, consumedAfter: elapsedMs(t0)}
	if op == "commit" {
		h.sysTx.close(id)
		meta.bookmark = h.store.NewBookmark()
	} else {
		meta.tx = systemInfo(id, exp)
	}
	if outcome == nil {
		if rt.jsonl {
			sw, err := newStreamWriter(w, rt, nil)
			if err != nil {
				return
			}
			defer sw.close()
			_ = sw.summary(&summaryBody{Bookmarks: bookmarksOf(meta.bookmark), Transaction: meta.tx})
			return
		}
		body := &resultResponse{Bookmarks: bookmarksOf(meta.bookmark), Transaction: meta.tx}
		if op == "commit" {
			writeAccepted(w, rt.contentType(), map[string]any{"bookmarks": []string{meta.bookmark}})
			return
		}
		writeAccepted(w, rt.contentType(), body)
		return
	}
	h.writeResult(w, rt, q, meta, outcome)
}

func basicUser(r *http.Request) string {
	u, _, _ := r.BasicAuth()
	return u
}

// databaseID returns the stable identifier SHOW DATABASES reports for the
// named database: 32 hex digits derived from the handler's seed, so it is the
// same across restarts for the same database file. Neo4j Bloom keys its
// perspectives by the system database's ID and does not run without one.
func (h *Handler) databaseID(name string) string {
	seed := h.idSeed
	if seed == "" {
		seed = h.database
	}
	sum := sha256.Sum256([]byte(seed + "\x00" + name))
	return hex.EncodeToString(sum[:16])
}
