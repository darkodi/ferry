package main

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

// The login role gets exactly ferry_app's grants, its password can be
// rotated by running again, and an administrative role is never touched.
func TestEnsureLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	pg, err := pgtest.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	owner, err := sql.Open("pgx", pg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	pw1, pw2 := strings.Repeat("a1", 16), strings.Repeat("B2", 20)
	if err := ensureLogin(t.Context(), owner, "ferry_svc", pw1); err != nil {
		t.Fatal(err)
	}
	as := func(pw string) *sql.DB {
		u, _ := url.Parse(pg.DSN)
		u.User = url.UserPassword("ferry_svc", pw)
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	svc := as(pw1)
	if _, err := svc.ExecContext(t.Context(), "SELECT count(*) FROM accounts"); err != nil {
		t.Fatalf("the login cannot use ferry_app's grants: %v", err)
	}
	if _, err := svc.ExecContext(t.Context(), "DELETE FROM postings"); err == nil {
		t.Fatal("the login can delete postings")
	}
	if _, err := svc.ExecContext(t.Context(), "CREATE TABLE x (id int)"); err == nil {
		t.Fatal("the login can change the schema")
	}

	// Rotation: run again with a new password; the old one stops working.
	if err := ensureLogin(t.Context(), owner, "ferry_svc", pw2); err != nil {
		t.Fatal(err)
	}
	if err := as(pw2).PingContext(t.Context()); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if err := as(pw1).PingContext(t.Context()); err == nil {
		t.Fatal("old password still works")
	}

	for name, c := range map[string]struct{ role, pw string }{
		"superuser (master user)": {"ferry", pw1},
		"ferry_app itself":        {"ferry_app", pw1},
		"quote in name":           {`x"; DROP TABLE postings; --`, pw1},
		"short password":          {"ferry_svc", "abc"},
		"punctuation in password": {"ferry_svc", strings.Repeat("a", 32) + "'"},
	} {
		if err := ensureLogin(t.Context(), owner, c.role, c.pw); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), c.pw) {
			t.Errorf("%s: error echoes the password", name)
		}
	}
}
