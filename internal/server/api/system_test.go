package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSystemDatabaseShowCommands(t *testing.T) {
	srv, _ := newTestServer(t)
	post := func(db, stmt string) (int, map[string]any) {
		code, body, _ := do(t, "POST", srv.URL+"/db/"+db+"/query/v2", "", "application/json",
			`{"statement":"`+stmt+`"}`, testUser, testPass)
		return code, body
	}
	for _, tc := range []struct {
		stmt string
		rows int
		cols int
	}{
		{"CALL db.ping()", 1, 1},
		{"SHOW DATABASES", 2, 14},
		{"SHOW CURRENT USER", 1, 5},
		{"CALL dbms.components() YIELD versions, edition, name;", 1, 3},
		{"CALL dbms.info();", 1, 3},
		{"SHOW ALL ROLES", 6, 1},
		{"SHOW USER `neo4j` PRIVILEGES", 19, 8},
		{"SHOW USER PRIVILEGES", 19, 8},
		{"call dbms.listConfig('dbms.security.auth_enabled')", 1, 8},
		{"CALL dbms.listConfig('no.such.setting')", 0, 8},
		{"CALL dbms.licenseAgreementDetails();", 1, 3},
		{" SHOW PROCEDURES YIELD *", 0, 12},
		{" SHOW FUNCTIONS YIELD *", 0, 12},
		{"CALL dbms.clientConfig() YIELD name, value", 0, 2},
		{"CALL db.awaitIndexes()", 0, 0},
		{"CALL db.awaitindexes(300);", 0, 0},
		{"CALL db.awaitindex('idx')", 0, 0},
		{"CALL db.awaitindex('idx', 5)", 0, 0},
		{"SHOW DATABASES YIELD name, currentStatus", 2, 2},
		{"show databases;", 2, 14},
		{"SHOW DEFAULT DATABASE", 1, 11},
		{"SHOW HOME DATABASE", 1, 11},
		{"SHOW DATABASE neo4j", 1, 14},
		{"SHOW DATABASE SYSTEM", 1, 14},
	} {
		code, body := post("system", tc.stmt)
		data, _ := body["data"].(map[string]any)
		if code != http.StatusAccepted || data == nil ||
			len(data["values"].([]any)) != tc.rows || len(data["fields"].([]any)) != tc.cols {
			t.Errorf("%s: %d %v", tc.stmt, code, body)
		}
	}
	if code, body := post("system", "SHOW DATABASE nope"); code != http.StatusNotFound {
		t.Errorf("unknown database: %d %v", code, body)
	}
	if code, body := post("system", "CALL bloom.checkAuthorization()"); code != http.StatusBadRequest || errCode(body) != "Neo.ClientError.Procedure.ProcedureNotFound" {
		t.Errorf("unknown procedure: %d %v", code, body)
	}
	if code, body := post("system", "CREATE USER x SET PASSWORD 'y'"); code != http.StatusBadRequest {
		t.Errorf("unsupported command: %d %v", code, body)
	}
	// Explicit transactions work, without touching the graph's transaction.
	hdr := func(method, path, body string) (int, map[string]any) {
		code, b, _ := do(t, method, srv.URL+path, "", "application/json", body, testUser, testPass)
		return code, b
	}
	code, body := hdr("POST", "/db/system/query/v2/tx", `{"statement":"CALL db.ping()"}`)
	tx, _ := body["transaction"].(map[string]any)
	if code != http.StatusAccepted || tx == nil {
		t.Fatalf("open: %d %v", code, body)
	}
	id := tx["id"].(string)
	if code, body := hdr("POST", "/db/neo4j/query/v2", `{"statement":"RETURN 1 AS x"}`); code != http.StatusAccepted {
		t.Errorf("graph query during a system tx: %d %v", code, body)
	}
	if code, body := hdr("POST", "/db/system/query/v2/tx/"+id, `{"statement":"SHOW DATABASES"}`); code != http.StatusAccepted {
		t.Errorf("run: %d %v", code, body)
	}
	if code, body := hdr("POST", "/db/system/query/v2/tx/"+id+"/commit", `{}`); code != http.StatusAccepted || body["bookmarks"] == nil {
		t.Errorf("commit: %d %v", code, body)
	}
	if code, _ := hdr("POST", "/db/system/query/v2/tx/"+id, `{"statement":"SHOW DATABASES"}`); code != http.StatusNotFound {
		t.Errorf("run after commit: %d", code)
	}
	_, body = hdr("POST", "/db/system/query/v2/tx", `{}`)
	id = body["transaction"].(map[string]any)["id"].(string)
	if code, _ := hdr("DELETE", "/db/system/query/v2/tx/"+id, ``); code != http.StatusOK {
		t.Errorf("rollback: %d", code)
	}
	_, body = hdr("POST", "/db/system/query/v2/tx", `{}`)
	id = body["transaction"].(map[string]any)["id"].(string)
	if code, _ := hdr("POST", "/db/system/query/v2/tx/"+id, `{"statement":"DROP DATABASE x"}`); code != http.StatusBadRequest {
		t.Errorf("unsupported in tx: %d", code)
	}
	if code, _ := hdr("POST", "/db/system/query/v2/tx/"+id, `{"statement":"SHOW DATABASES"}`); code != http.StatusNotFound {
		t.Errorf("a failed statement should destroy the transaction: %d", code)
	}
}

func TestDatabaseIDStable(t *testing.T) {
	a := &Handler{database: "neo4j", idSeed: "/data/a.db"}
	b := &Handler{database: "neo4j", idSeed: "/data/a.db"}
	c := &Handler{database: "neo4j", idSeed: "/data/c.db"}
	if a.databaseID("system") != b.databaseID("system") {
		t.Error("the same seed must give the same ID")
	}
	if a.databaseID("system") == c.databaseID("system") || a.databaseID("system") == a.databaseID("neo4j") {
		t.Error("different files and databases must get different IDs")
	}
	if len(a.databaseID("system")) != 32 {
		t.Errorf("want 32 hex digits, got %q", a.databaseID("system"))
	}
}

func TestSkillsEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	// No credentials: skills are public, like discovery.
	code, body, _ := do(t, "GET", srv.URL+"/skills", "", "", "", "", "")
	list, _ := body["skills"].([]any)
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %v", code, body)
	}
	first := list[0].(map[string]any)
	if first["name"] != "graingraph-query" || first["url"] != srv.URL+"/skills/graingraph-query" || first["description"] == "" {
		t.Errorf("entry: %v", first)
	}

	resp, err := http.Get(srv.URL + "/skills/graingraph-query")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	doc, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("get: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(string(doc), "---\nname: graingraph-query\n") || !strings.Contains(string(doc), srv.URL+"/db/neo4j/query/v2") {
		t.Errorf("document not rendered for this server: %.200q", doc)
	}

	if code, _, _ := do(t, "GET", srv.URL+"/skills/nope", "", "", "", "", ""); code != http.StatusNotFound {
		t.Errorf("unknown skill: %d", code)
	}
	if code, body, _ := do(t, "GET", srv.URL+"/", "", "", "", "", ""); code != http.StatusOK || body["skills"] != srv.URL+"/skills" {
		t.Errorf("discovery: %d %v", code, body)
	}
}
