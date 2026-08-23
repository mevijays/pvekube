package server

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// schema mirrors the real FK relationships that matter here: clusters
// reference templates with NO on-delete clause, which is what makes deleting
// an in-use template fail at the database and why the check has to happen
// before anything is destroyed on Proxmox.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE proxmox_connections (id INTEGER PRIMARY KEY);
		CREATE TABLE templates (
			id INTEGER PRIMARY KEY,
			connection_id INTEGER NOT NULL REFERENCES proxmox_connections(id) ON DELETE CASCADE);
		CREATE TABLE clusters (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			connection_id INTEGER NOT NULL REFERENCES proxmox_connections(id) ON DELETE CASCADE,
			template_id INTEGER NOT NULL REFERENCES templates(id));
		INSERT INTO proxmox_connections (id) VALUES (1);
		INSERT INTO templates (id, connection_id) VALUES (10, 1), (11, 1);
	`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestClustersUsingTemplateNamesEveryUser(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO clusters (id,name,connection_id,template_id) VALUES (1,'prod',1,10),(2,'staging',1,10)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}

	users, err := s.clustersUsingTemplate(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("expected both clusters, got %v", users)
	}
	// Names, not counts — the refusal message has to say which cluster.
	joined := strings.Join(users, ",")
	for _, want := range []string{"prod", "staging"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, users)
		}
	}
}

// An unused template must delete cleanly — the guard must not block the
// normal case.
func TestClustersUsingTemplateEmptyForUnusedTemplate(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO clusters (id,name,connection_id,template_id) VALUES (1,'prod',1,10)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}

	users, err := s.clustersUsingTemplate(11)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("template 11 is unused but reported %v", users)
	}
}

// The reason the guard exists: without it the database rejects the delete —
// but only AFTER the Proxmox image has already been destroyed.
func TestDeletingAnInUseTemplateIsRejectedByTheDatabase(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO clusters (id,name,connection_id,template_id) VALUES (1,'prod',1,10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM templates WHERE id = 10`); err == nil {
		t.Fatal("expected a foreign-key error; if this ever passes the guard's rationale no longer holds")
	}
}
