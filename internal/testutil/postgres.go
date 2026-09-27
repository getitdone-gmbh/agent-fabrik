package testutil

import (
	"database/sql"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// PostgresURL returns a URL to a dedicated test database named ar_<name>
// on the server in AR_TEST_DATABASE_URL, creating it if needed, or "" when
// the variable is unset. Separate databases keep packages that run in
// parallel from competing for the (per-database) leader lock.
func PostgresURL(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("AR_TEST_DATABASE_URL")
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dbname := "ar_" + name
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, dbname).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := db.Exec(`CREATE DATABASE ` + dbname); err != nil {
			t.Fatal(err)
		}
	}
	u.Path = "/" + dbname
	return u.String()
}
