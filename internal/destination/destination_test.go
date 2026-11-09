package destination_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer tdb.Close()
	if pool, err = pgxpool.New(ctx, tdb.DSN); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	return m.Run()
}

const form = `{"BankDetails":{"AccountNumber":"000123456789","BankCode":"021000021","AccountType":"Checking"},` +
	`"AccountHolderAddress":{"Address":"1 Main St","City":"New York","State":"NY","PostalCode":"10001"},"PaymentPurpose":"Invoice"}`

func key(b byte) []byte { return bytes.Repeat([]byte{b}, destination.KeySize) }

func newStore(t *testing.T, k []byte) *destination.Store {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	s, err := destination.NewStore(pool, "noah", k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newCustomer(t *testing.T) uuid.UUID {
	t.Helper()
	c, _, err := customer.Create(context.Background(), pool, "Acme "+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func create(t *testing.T, s *destination.Store, cust uuid.UUID) destination.Destination {
	t.Helper()
	d, err := s.Create(context.Background(), destination.CreateRequest{
		CustomerID: cust, Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch", Form: []byte(form),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---- unit ----

func TestCheckForm(t *testing.T) {
	deep := `{"a":{"b":{"c":{"d":{"e":"x"}}}}}`
	many := "{" + strings.TrimSuffix(strings.Repeat(`"k":"v",`, 1), ",")
	for i := range 60 {
		many += fmt.Sprintf(`,"k%d":"v"`, i)
	}
	many += "}"
	for _, tc := range []struct {
		name, in string
		ok       bool
	}{
		{"bank form", form, true},
		{"booleans", `{"Confirm":true,"BankDetails":{"AccountNumber":"1234"}}`, true},
		{"empty", ``, false},
		{"empty object", `{}`, false},
		{"array", `["12345678"]`, false},
		{"string", `"12345678"`, false},
		{"number value", `{"AccountNumber":12345678}`, false},
		{"null value", `{"AccountNumber":null}`, false},
		{"array value", `{"AccountNumber":["1"]}`, false},
		{"control character", "{\"AccountNumber\":\"12\\u000034\"}", false},
		{"too deep", deep, false},
		{"too many fields", many, false},
		{"long value", `{"a":"` + strings.Repeat("x", 257) + `"}`, false},
		{"trailing data", `{"a":"b"} {}`, false},
		{"not JSON", `{a:b}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := destination.CheckForm([]byte(tc.in))
			if (err == nil) != tc.ok {
				t.Fatalf("CheckForm(%s) = %v, want ok=%v", tc.in, err, tc.ok)
			}
			if err != nil && !errors.Is(err, destination.ErrInvalid) {
				t.Fatalf("error %v is not ErrInvalid", err)
			}
		})
	}
}

func TestDisplayShowsOnlyLastFour(t *testing.T) {
	for _, tc := range []struct{ form, want string }{
		{form, "BankAch ****6789"},
		{`{"IBAN":"DE89 3704 0044 0532 0130 00"}`, "BankAch ****3000"},
		{`{"BankDetails":{"accountNumber":"123"}}`, "BankAch ****123"},
		{`{"PaymentPurpose":"Invoice"}`, "BankAch"},
		{`{"AccountNumber":"1111","Iban":"2222"}`, "BankAch ****1111"}, // sorted keys: deterministic
	} {
		if got := destination.Display("BankAch", []byte(tc.form)); got != tc.want {
			t.Errorf("Display(%s) = %q, want %q", tc.form, got, tc.want)
		}
	}
}

func TestParseKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(key(7))
	if k, err := destination.ParseKey(" " + good + "\n"); err != nil || !bytes.Equal(k, key(7)) {
		t.Fatalf("ParseKey(good) = %v, %v", k, err)
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := destination.ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

// ---- database ----

// Bank details are stored encrypted: the row holds no account number, and
// ForQuote returns exactly the form that was saved.
func TestFormEncryptedAtRest(t *testing.T) {
	s := newStore(t, key(1))
	cust := newCustomer(t)
	d := create(t, s, cust)
	if d.Display != "BankAch ****6789" || d.Status != "active" || d.Rail != "noah" {
		t.Fatalf("destination %+v", d)
	}

	var sealed []byte
	if err := pool.QueryRow(t.Context(), `SELECT form_ciphertext FROM destinations WHERE id = $1`, d.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"000123456789", "021000021", "Main St"} {
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatalf("ciphertext contains %q in clear", secret)
		}
	}

	got, err := s.ForQuote(t.Context(), cust, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := destination.CheckForm([]byte(form))
	if !bytes.Equal(got.Form, want) || got.Country != "US" || got.PaymentMethodType != "BankAch" {
		t.Fatalf("ForQuote = %+v, form %s", got, got.Form)
	}
}

// A ciphertext only opens on its own row with the right key: copying it
// onto another row (associated data) or using another key fails, and
// nothing falls back to returning garbage.
func TestTamperedOrWrongKeyRefused(t *testing.T) {
	s := newStore(t, key(2))
	cust := newCustomer(t)
	a, b := create(t, s, cust), create(t, s, cust)

	other := newStore(t, key(3))
	if _, err := other.ForQuote(t.Context(), cust, a.ID); err == nil {
		t.Fatal("decrypted with the wrong key")
	}

	// Copy a's ciphertext onto b, as an attacker with database access might.
	// The test connects as the owner role, which may update any column.
	if _, err := pool.Exec(t.Context(),
		`UPDATE destinations SET form_ciphertext = (SELECT form_ciphertext FROM destinations WHERE id = $1) WHERE id = $2`,
		a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForQuote(t.Context(), cust, b.ID); err == nil {
		t.Fatal("a ciphertext moved to another row decrypted")
	}
}

// Another customer's destination does not exist for you; Check also
// refuses a different currency or rail.
func TestOwnershipAndCheck(t *testing.T) {
	s := newStore(t, key(4))
	alice, bob := newCustomer(t), newCustomer(t)
	d := create(t, s, alice)

	if _, err := s.Get(t.Context(), bob, d.ID); !errors.Is(err, destination.ErrNotFound) {
		t.Fatalf("bob reads alice's destination: %v", err)
	}
	if _, err := s.ForQuote(t.Context(), bob, d.ID); !errors.Is(err, destination.ErrNotFound) {
		t.Fatalf("bob quotes to alice's destination: %v", err)
	}
	if err := s.Check(t.Context(), bob, d.ID, "USD"); !errors.Is(err, destination.ErrNotFound) {
		t.Fatalf("Check for bob: %v", err)
	}
	if err := s.Check(t.Context(), alice, d.ID, "USD"); err != nil {
		t.Fatalf("Check for alice: %v", err)
	}
	if err := s.Check(t.Context(), alice, d.ID, "EUR"); !errors.Is(err, destination.ErrInvalid) {
		t.Fatalf("Check in EUR: %v", err)
	}
	mockStore, err := destination.NewStore(pool, "mock", key(4))
	if err != nil {
		t.Fatal(err)
	}
	if err := mockStore.Check(t.Context(), alice, d.ID, "USD"); !errors.Is(err, destination.ErrInvalid) {
		t.Fatalf("Check on another rail: %v", err)
	}
	if list, err := s.List(t.Context(), bob); err != nil || len(list) != 0 {
		t.Fatalf("bob's list: %v %v", list, err)
	}
}

// Without a key the store can still check destinations and remember
// payment methods (the worker), but never create or read bank details.
func TestNoKeyStore(t *testing.T) {
	s := newStore(t, nil)
	if _, err := s.Create(t.Context(), destination.CreateRequest{}); !errors.Is(err, destination.ErrNoKey) {
		t.Fatalf("Create without key: %v", err)
	}
	withKey := newStore(t, key(5))
	cust := newCustomer(t)
	d := create(t, withKey, cust)
	if _, err := s.ForQuote(t.Context(), cust, d.ID); !errors.Is(err, destination.ErrNoKey) {
		t.Fatalf("ForQuote without key: %v", err)
	}
	if err := s.Check(t.Context(), cust, d.ID, "USD"); err != nil {
		t.Fatalf("Check without key: %v", err)
	}
}
